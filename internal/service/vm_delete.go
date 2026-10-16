// Package service 虚拟机删除工作流
//
// 删除一台虚拟机会同时影响：平台侧虚拟机、路由器端口映射、本地配置、
// 状态历史、用户配额。这些系统无法参与同一个事务，因此把删除设计为
// 「带墓碑的幂等流程」：
//
//   - 开始前先写入 tombstone，记录删除意图与配额归属；
//   - 每个步骤完成后在墓碑中打标，重复执行时跳过已完成步骤；
//   - 外部调用失败时登记到 outbox，由后台重试，而不是让删除卡住；
//   - 全部完成后把墓碑标记为 done（保留一段时间供审计）。
//
// 进程在任意一步崩溃后，调度器扫描未完成的墓碑即可继续删除。
package service

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// DeleteVMWorkflow 虚拟机删除工作流（幂等，可重复执行）
type DeleteVMWorkflow struct {
	DB        *data.DB
	HostSvc   *HostService
	NATSvc    *NATService
	ProxySvc  *ProxyService
	QuotaSvc  *QuotaService
	Reconcile *ReconcileService
}

// NewDeleteVMWorkflow 创建删除工作流
func NewDeleteVMWorkflow(db *data.DB, hostSvc *HostService) *DeleteVMWorkflow {
	return &DeleteVMWorkflow{DB: db, HostSvc: hostSvc}
}

// Run 执行或继续执行删除流程。
//
// quotaUser 为计费用户（主所有者）；为空时不会释放配额。
// 该方法可以安全地重复调用：已完成的步骤会被跳过。
func (w *DeleteVMWorkflow) Run(hsName, vmUUID, quotaUser string) error {
	if w.DB == nil {
		return fmt.Errorf("数据库未初始化")
	}

	// 1) 读取虚拟机配置（驱动内存 → 数据库 → 墓碑快照）
	cfg := w.loadVMConfig(hsName, vmUUID)

	// 2) 写入墓碑，记录删除意图与配额归属
	cfgJSON := ""
	if cfg != nil {
		if raw, err := json.Marshal(cfg); err == nil {
			cfgJSON = string(raw)
		}
	}
	if err := w.DB.CreateTombstone(hsName, vmUUID, quotaUser, cfgJSON); err != nil {
		return fmt.Errorf("创建删除墓碑失败: %w", err)
	}

	tomb, err := w.DB.GetTombstone(hsName, vmUUID)
	if err != nil {
		return fmt.Errorf("读取删除墓碑失败: %w", err)
	}
	if tomb == nil {
		return fmt.Errorf("删除墓碑不存在: %s/%s", hsName, vmUUID)
	}
	if tomb.Status == data.TombstoneDone {
		return nil
	}
	if tomb.QuotaUser != "" {
		quotaUser = tomb.QuotaUser
	}
	// 重启后续做时，配置可能已从库中删除，改用墓碑中的快照
	if cfg == nil && tomb.VmConfig != "" {
		cfg, _ = DecodeVMConfig(tomb.VmConfig)
	}
	steps := tomb.Steps
	if steps == nil {
		steps = map[string]bool{}
	}

	fail := func(step string, e error) error {
		_ = w.DB.SetTombstoneError(hsName, vmUUID, fmt.Sprintf("%s: %v", step, e))
		return e
	}

	// 3) 撤销外部端口映射。失败不阻断删除，改为登记补偿任务后台重试，
	//    否则路由器暂时不可用会导致虚拟机永远无法删除。
	if !steps[data.TombstoneStepNAT] {
		if cfg != nil && w.NATSvc != nil {
			for _, nat := range cfg.NatAll {
				if revokeErr := w.NATSvc.RevokePortMap(hsName, vmUUID, nat); revokeErr != nil {
					if w.Reconcile != nil {
						if enqErr := w.Reconcile.EnqueueResourceTask(hsName, vmUUID,
							data.OutboxActionNATRevoke, NATRevokePayload(nat)); enqErr != nil {
							return fail("撤销端口映射", fmt.Errorf("%v（登记补偿失败: %v）", revokeErr, enqErr))
						}
					} else {
						return fail("撤销端口映射", revokeErr)
					}
				}
			}
		}
		if err := w.DB.MarkTombstoneStep(hsName, vmUUID, data.TombstoneStepNAT, ""); err != nil {
			return fail("记录端口映射步骤", err)
		}
	}

	// 3b) 解除路由器静态绑定（DHCP 静态地址 + ARP），对应 Python 版
	//     VMDelete → IPBinder(False)。同样不阻断删除：失败登记补偿任务后台重试，
	//     否则路由器暂时不可用会让虚拟机永远删不掉、或在路由器上留下残留记录。
	if !steps[data.TombstoneStepStatic] {
		if cfg != nil && w.NATSvc != nil {
			if unbindErr := w.NATSvc.UnbindStaticForVM(hsName, cfg); unbindErr != nil {
				if w.Reconcile != nil {
					if enqErr := w.Reconcile.EnqueueResourceTask(hsName, vmUUID,
						data.OutboxActionStaticUnbind, StaticUnbindPayload(cfg)); enqErr != nil {
						_ = w.DB.SetTombstoneError(hsName, vmUUID,
							fmt.Sprintf("解除静态绑定失败且登记补偿失败: %v / %v", unbindErr, enqErr))
					}
				} else {
					_ = w.DB.SetTombstoneError(hsName, vmUUID, "解除静态绑定失败: "+unbindErr.Error())
				}
			}
		}
		if err := w.DB.MarkTombstoneStep(hsName, vmUUID, data.TombstoneStepStatic, ""); err != nil {
			return fail("记录静态绑定步骤", err)
		}
	}

	// 4) 删除平台侧虚拟机（"不存在"视为已完成，保证幂等）
	if !steps[data.TombstoneStepPlatform] {
		if d := w.HostSvc.GetDriver(hsName); d != nil {
			result, delErr := d.DeleteVM(vmUUID, true)
			if delErr != nil {
				return fail("删除平台虚拟机", delErr)
			}
			if result != nil && !result.Success && !strings.Contains(result.Message, "不存在") {
				return fail("删除平台虚拟机", fmt.Errorf("%s", result.Message))
			}
		}
		if err := w.DB.MarkTombstoneStep(hsName, vmUUID, data.TombstoneStepPlatform, ""); err != nil {
			return fail("记录平台删除步骤", err)
		}
	}

	// 5) 清理本地配置与状态历史
	if !steps[data.TombstoneStepRecords] {
		if err := w.DB.DeleteVMConfig(hsName, vmUUID); err != nil {
			return fail("删除本地配置", err)
		}
		if err := w.DB.DeleteVMStatus(hsName, vmUUID); err != nil {
			// 状态清理失败不阻断：可被周期性清理补齐
			_ = w.DB.SetTombstoneError(hsName, vmUUID, "清理状态记录失败: "+err.Error())
		}
		if err := w.DB.MarkTombstoneStep(hsName, vmUUID, data.TombstoneStepRecords, ""); err != nil {
			return fail("记录清理步骤", err)
		}
	}

	// 6) 释放配额
	if !steps[data.TombstoneStepQuota] {
		if quotaUser != "" && cfg != nil && w.QuotaSvc != nil {
			// 释放配额属于账务补偿：失败不阻断删除，
			// 否则「所有者已被删除」等数据异常会让虚拟机永远删不掉。
			// 错误记录在墓碑中，可通过运维接口查看。
			if relErr := w.QuotaSvc.ReleaseQuota(quotaUser, cfg); relErr != nil {
				_ = w.DB.SetTombstoneError(hsName, vmUUID, "释放配额失败: "+relErr.Error())
			}
		}
		if err := w.DB.MarkTombstoneStep(hsName, vmUUID, data.TombstoneStepQuota, ""); err != nil {
			return fail("记录配额步骤", err)
		}
	}

	// 7) 反向代理：
	//    - 平台自有通道（青州云等）由云端管理，必须逐条调用云端接口销账，
	//      否则云端规则会残留；
	//    - 本地 Caddy 为全量重建：配置已删除，重建一次即可让代理下线。
	//    两种情况失败都不阻断删除，改为登记补偿任务后台重试。
	if !steps[data.TombstoneStepProxy] {
		if cfg != nil && len(cfg.WebAll) > 0 && w.ProxySvc != nil && w.ProxySvc.HasProvider(hsName) {
			for _, web := range cfg.WebAll {
				if revokeErr := w.ProxySvc.RevokeProxy(hsName, vmUUID, web); revokeErr != nil {
					if w.Reconcile != nil {
						if enqErr := w.Reconcile.EnqueueResourceTask(hsName, vmUUID,
							data.OutboxActionProxyRevoke, WebProxyPayload(web)); enqErr != nil {
							_ = w.DB.SetTombstoneError(hsName, vmUUID,
								fmt.Sprintf("撤销云代理失败且登记补偿失败: %v / %v", revokeErr, enqErr))
						}
					} else {
						_ = w.DB.SetTombstoneError(hsName, vmUUID, "撤销云代理失败: "+revokeErr.Error())
					}
				}
			}
		} else if w.Reconcile != nil {
			if err := w.Reconcile.EnqueueResourceTask(hsName, vmUUID,
				data.OutboxActionProxySync, map[string]interface{}{}); err != nil {
				_ = w.DB.SetTombstoneError(hsName, vmUUID, "登记代理同步失败: "+err.Error())
			}
		}
		if err := w.DB.MarkTombstoneStep(hsName, vmUUID, data.TombstoneStepProxy, ""); err != nil {
			_ = w.DB.SetTombstoneError(hsName, vmUUID, "记录代理步骤失败: "+err.Error())
		}
	}

	if err := w.DB.CompleteTombstone(hsName, vmUUID); err != nil {
		return fmt.Errorf("标记删除完成失败: %w", err)
	}
	return nil
}

// SweepIncomplete 扫描未完成的删除墓碑并继续执行（供调度器调用）
func (w *DeleteVMWorkflow) SweepIncomplete(limit int) (resumed int, failed int) {
	if w.DB == nil {
		return 0, 0
	}
	tombs, err := w.DB.ListIncompleteTombstones(limit)
	if err != nil {
		return 0, 0
	}
	for _, t := range tombs {
		if err := w.Run(t.HsName, t.VmUUID, t.QuotaUser); err != nil {
			failed++
			continue
		}
		resumed++
	}
	return resumed, failed
}

// loadVMConfig 依次从驱动内存、数据库读取虚拟机配置
func (w *DeleteVMWorkflow) loadVMConfig(hsName, vmUUID string) *model.VMConfig {
	if w.HostSvc != nil {
		if cfg := w.HostSvc.MustGetVMConfig(hsName, vmUUID); cfg != nil {
			return cfg
		}
	}
	if w.DB == nil {
		return nil
	}
	raw, err := w.DB.GetVMConfig(hsName, vmUUID)
	if err != nil || raw == "" {
		return nil
	}
	cfg, err := DecodeVMConfig(raw)
	if err != nil {
		return nil
	}
	return cfg
}
