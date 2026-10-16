// Package api 虚拟机异步任务执行器
//
// 与 Python 版语义对齐：创建 / 更新 / 删除 / 重装虚拟机都是异步操作，
// 接口立即返回 {task_id}，实际动作在任务执行器中完成。
// 这样长时间的平台操作不会占用 HTTP 请求，也便于前端展示进度与终止。
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/logger"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/service"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/task"
)

// 任务参数键名（对应 Python 版 _submit_async 的 params）
const (
	taskParamVMConfig     = "vm_config"
	taskParamOldConfig    = "old_config"
	taskParamQuotaUser    = "quota_user"
	taskParamRemoveBackup = "remove_backup"
)

// vmTaskExecutor 执行虚拟机相关的异步任务
type vmTaskExecutor struct {
	app *App
}

// marshalTaskParams 将任务参数序列化。
// 结构化参数（如 VMConfig）序列化为 JSON 字符串存放，
// 避免元数据在数据库中嵌套过深。
func marshalTaskParams(in map[string]interface{}) (map[string]interface{}, error) {
	out := make(map[string]interface{}, len(in))
	for k, v := range in {
		switch val := v.(type) {
		case nil, string, bool, int, int64, float64:
			out[k] = val
		default:
			data, err := json.Marshal(val)
			if err != nil {
				return nil, err
			}
			out[k] = string(data)
		}
	}
	return out, nil
}

// submitVMTask 创建并提交一个虚拟机任务，返回任务ID。
//
// 同一虚拟机同时只允许一个未完成任务：并发创建/删除同一虚拟机会
// 让平台状态与本地配置互相覆盖。
func (app *App) submitVMTask(name string, taskType task.TaskType, createdBy,
	hsName, vmUUID string, params map[string]interface{}) (string, error) {

	if app.TaskManager.HasActiveTaskForVM(hsName, vmUUID) {
		return "", fmt.Errorf("该虚拟机已有正在执行的任务，请稍后再试")
	}

	metadata := map[string]interface{}{
		"hs_name": hsName,
		"vm_uuid": vmUUID,
	}
	for k, v := range params {
		metadata[k] = v
	}

	t, err := app.TaskManager.CreateVMTask(name, taskType, createdBy, hsName, vmUUID, metadata)
	if err != nil {
		return "", err
	}
	if err := app.TaskManager.SubmitTask(t.ID, &vmTaskExecutor{app: app}, nil); err != nil {
		return "", err
	}
	return t.ID, nil
}

// Execute 按任务类型分发执行
func (e *vmTaskExecutor) Execute(ctx context.Context, t *task.Task) error {
	switch t.Type {
	case task.TaskTypeVMCreate:
		return e.createVM(ctx, t)
	case task.TaskTypeVMDelete:
		return e.deleteVM(ctx, t)
	case task.TaskTypeVMUpdate:
		return e.updateVM(ctx, t)
	case task.TaskTypeVMReinstall:
		return e.reinstallVM(ctx, t)
	default:
		return fmt.Errorf("不支持的任务类型: %s", t.Type)
	}
}

// taskLog 构造带任务上下文的日志事件（操作人 / 主机 / 虚拟机 / 任务号）。
//
// 日志里带上这些字段，前端「系统日志」才能回答
// 「谁在哪台主机上对哪台虚拟机做了什么」，而不是一片「未知」。
func (e *vmTaskExecutor) taskLog(t *task.Task, action string) *logger.Event {
	ev := e.app.Logger.Event("vm", action)
	if t == nil {
		return ev
	}
	ev.User(t.CreatedBy).Host(t.HsName).Detail("task_id", t.ID)
	if t.VmUUID != "" {
		ev.VM(t.VmUUID, "")
	}
	return ev
}

// vmSpecDetails 提取虚拟机规格，作为日志的结构化明细。
func vmSpecDetails(cfg *model.VMConfig) map[string]interface{} {
	if cfg == nil {
		return nil
	}
	return map[string]interface{}{
		"os":         cfg.OsName,
		"cpu":        cfg.CpuNum,
		"mem":        cfg.MemNum,
		"disk":       cfg.HddNum,
		"gpu_mem":    cfg.GpuMem,
		"speed_up":   cfg.SpeedU,
		"speed_down": cfg.SpeedD,
	}
}

// taskVMConfig 从任务参数中解析虚拟机配置
func taskVMConfig(t *task.Task, key string) (*model.VMConfig, error) {
	raw, _ := t.Metadata[key].(string)
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("任务缺少参数: %s", key)
	}
	cfg, err := service.DecodeVMConfig(raw)
	if err != nil {
		return nil, fmt.Errorf("解析任务参数 %s 失败: %w", key, err)
	}
	return cfg, nil
}

// taskQuotaUser 返回该任务应计费的用户名
func taskQuotaUser(t *task.Task) string {
	v, _ := t.Metadata[taskParamQuotaUser].(string)
	return v
}

// loadVMConfig 依次从驱动内存与数据库读取虚拟机配置
func (e *vmTaskExecutor) loadVMConfig(hsName, vmUUID string) *model.VMConfig {
	if d := e.app.HostSvc.GetDriver(hsName); d != nil {
		if cfg, err := d.GetVMDetail(vmUUID); err == nil {
			return cfg
		}
	}
	raw, err := e.app.DB.GetVMConfig(hsName, vmUUID)
	if err != nil || raw == "" {
		return nil
	}
	cfg, err := service.DecodeVMConfig(raw)
	if err != nil {
		return nil
	}
	return cfg
}

// createVM 创建虚拟机：平台创建 → 落库 → 扣减配额
func (e *vmTaskExecutor) createVM(ctx context.Context, t *task.Task) error {
	start := time.Now()
	cfg, err := taskVMConfig(t, taskParamVMConfig)
	if err != nil {
		e.taskLog(t, "创建虚拟机").Err(err).Errorf("任务参数解析失败：%v", err)
		return err
	}
	d := e.app.HostSvc.GetDriver(t.HsName)
	if d == nil {
		e.taskLog(t, "创建虚拟机").VM(cfg.VmUUID, cfg.OsName).
			Errorf("主机不存在或未启用：%s", t.HsName)
		return fmt.Errorf("主机不存在或未启用: %s", t.HsName)
	}

	// 下发过程细节走 DEBUG：任务开始/结束由任务引擎与成功日志体现
	e.taskLog(t, "创建虚拟机").VM(cfg.VmUUID, cfg.OsName).
		Details(vmSpecDetails(cfg)).
		Debug("开始向平台下发创建请求")

	// 网络配置检查：为缺地址的网卡从 IP 池分配 IP，并按 IP 推导 MAC。
	// 对应 Python ProxmoxQemu.VMCreate 开头的 NetCheck —— 缺少这一步，
	// 网卡既拿不到固定 IP、也没有与路由器绑定的 MAC，DHCP/ARP 静态绑定会全部被跳过。
	e.netCheck(t, cfg)

	_ = e.app.TaskManager.UpdateProgress(t.ID, 10, "正在创建虚拟机")
	result, err := d.CreateVM(cfg)
	if err != nil {
		e.taskLog(t, "创建虚拟机").VM(cfg.VmUUID, cfg.OsName).
			Details(vmSpecDetails(cfg)).Duration(time.Since(start)).Err(err).
			Errorf("虚拟机创建失败：%v", err)
		return fmt.Errorf("创建虚拟机失败: %w", err)
	}
	if result != nil && !result.Success {
		err := fmt.Errorf("%s", result.Message)
		e.taskLog(t, "创建虚拟机").VM(cfg.VmUUID, cfg.OsName).
			Details(vmSpecDetails(cfg)).Duration(time.Since(start)).Err(err).
			Errorf("虚拟机创建失败：%s", result.Message)
		return err
	}

	if err := e.app.DB.UpsertVMConfig(t.HsName, cfg.VmUUID, cfg); err != nil {
		e.taskLog(t, "创建虚拟机").VM(cfg.VmUUID, cfg.OsName).Err(err).
			Errorf("虚拟机已在平台创建，但保存配置失败：%v", err)
		return fmt.Errorf("保存虚拟机配置失败: %w", err)
	}

	// 创建成功后才扣减配额：失败路径无需回滚
	if user := taskQuotaUser(t); user != "" {
		if err := e.app.QuotaSvc.DeductQuota(user, cfg); err != nil {
			e.app.Logger.Event("quota", "配额扣减").Host(t.HsName).VM(cfg.VmUUID, cfg.OsName).
				User(t.CreatedBy).Detail("quota_user", user).Err(err).
				Warn("虚拟机 %s 创建后扣减配额失败: %v", cfg.VmUUID, err)
		}
	}

	// 路由器静态绑定（爱快 DHCP 静态地址 + ARP），
	// 对应 Python 版 VMCreate → IPBinder(True)。
	// 路由器不可用不应让创建任务失败，仅记录告警。
	e.bindStatic(t, cfg)

	e.taskLog(t, "创建虚拟机").VM(cfg.VmUUID, cfg.OsName).
		Details(vmSpecDetails(cfg)).
		Detail("owner", taskQuotaUser(t)).
		Duration(time.Since(start)).
		Info("虚拟机创建成功：%s", cfg.VmUUID)

	_ = e.app.TaskManager.UpdateProgress(t.ID, 100, "创建完成")
	return nil
}

// netCheck 网卡地址分配（对应 Python BasicServer.NetCheck）。
//
// 为缺 IP 的网卡从主机 ipaddr_maps 地址池分配地址，并按 IP 推导 MAC
// （与 Python NCConfig.send_mac 一致）。
//
// 失败时记录 ERROR 但不阻断创建：某些场景（如纯手工指定 IP）不需要自动分配，
// 但必须把原因显式暴露出来，否则会表现为「创建成功却一条绑定都没有」。
func (e *vmTaskExecutor) netCheck(t *task.Task, cfg *model.VMConfig) {
	if e.app.IPAllocSvc == nil || cfg == nil || len(cfg.NicAll) == 0 {
		return
	}

	if err := e.app.IPAllocSvc.NetCheck(t.HsName, cfg); err != nil {
		e.taskLog(t, "网络配置检查").VM(cfg.VmUUID, cfg.OsName).Err(err).
			Errorf("虚拟机 %s 网络配置检查失败（该虚拟机的网卡可能缺少 IP，静态绑定将被跳过）：%v",
				cfg.VmUUID, err)
		return
	}

	e.taskLog(t, "网络配置检查").VM(cfg.VmUUID, cfg.OsName).
		Detail("nics", len(cfg.NicAll)).
		Debug("虚拟机 %s 网卡地址分配完成，共 %d 张网卡", cfg.VmUUID, len(cfg.NicAll))
}

// bindStatic 下发虚拟机网卡的静态绑定（未配置爱快时静默跳过）。
//
// 必须区分「真的绑定了」与「因缺少 IP/MAC 而跳过」：后者说明网卡还没分配地址，
// 属于必须暴露的配置缺失 —— 原先两种情况都记成「绑定成功」，
// 会让人误以为路由器上已有规则，实际上一条都没下发。
func (e *vmTaskExecutor) bindStatic(t *task.Task, cfg *model.VMConfig) {
	if e.app.NATService == nil || cfg == nil {
		return
	}

	res, err := e.app.NATService.BindStaticForVM(t.HsName, cfg.VmUUID, cfg)

	// 主机未配置爱快：静态绑定属于可选能力，静默跳过
	if res.NotConfigured {
		return
	}
	if err != nil {
		e.taskLog(t, "静态IP绑定").VM(cfg.VmUUID, cfg.OsName).Err(err).
			Warn("虚拟机 %s 静态绑定失败: %v", cfg.VmUUID, err)
	}
	if len(res.Skipped) > 0 {
		// 有网卡但缺 IP/MAC：绑定无处可下，属于必须暴露的配置缺失。
		// 原先这种情况也被记成「绑定成功」，会让人误以为路由器上已有规则。
		e.taskLog(t, "静态IP绑定").VM(cfg.VmUUID, cfg.OsName).
			Detail("skipped", strings.Join(res.Skipped, ",")).
			Warn("虚拟机 %s 有网卡缺少 IP/MAC，未下发静态绑定：%s", cfg.VmUUID, res.Summary())
		return
	}
	if err != nil {
		return
	}
	// 绑定成功属于过程细节：Debug 级别只落文件，失败与跳过才升级为 WARNING
	e.taskLog(t, "静态IP绑定").VM(cfg.VmUUID, cfg.OsName).
		Detail("bound", res.Bound).
		Debug("虚拟机 %s 静态地址绑定完成：%s", cfg.VmUUID, res.Summary())
}

// updateVM 更新虚拟机配置：平台更新 → 落库 → 调整配额
func (e *vmTaskExecutor) updateVM(ctx context.Context, t *task.Task) error {
	start := time.Now()
	newCfg, err := taskVMConfig(t, taskParamVMConfig)
	if err != nil {
		e.taskLog(t, "更新虚拟机").Err(err).Errorf("任务参数解析失败：%v", err)
		return err
	}
	oldCfg, oldErr := taskVMConfig(t, taskParamOldConfig)
	if oldErr != nil {
		oldCfg = e.loadVMConfig(t.HsName, t.VmUUID)
	}

	d := e.app.HostSvc.GetDriver(t.HsName)
	if d == nil {
		e.taskLog(t, "更新虚拟机").VM(newCfg.VmUUID, newCfg.OsName).
			Errorf("主机不存在或未启用：%s", t.HsName)
		return fmt.Errorf("主机不存在或未启用: %s", t.HsName)
	}

	e.taskLog(t, "更新虚拟机").VM(newCfg.VmUUID, newCfg.OsName).
		Detail("old", vmSpecDetails(oldCfg)).
		Detail("new", vmSpecDetails(newCfg)).
		Debug("开始向平台下发更新请求")

	// 网络配置检查：新增网卡需补 IP/MAC（对应 Python VMUpdate → NetCheck）
	e.netCheck(t, newCfg)

	_ = e.app.TaskManager.UpdateProgress(t.ID, 20, "正在更新虚拟机")
	result, err := d.UpdateVM(newCfg, oldCfg)
	if err != nil {
		e.taskLog(t, "更新虚拟机").VM(newCfg.VmUUID, newCfg.OsName).
			Duration(time.Since(start)).Err(err).
			Errorf("虚拟机更新失败：%v", err)
		return fmt.Errorf("更新虚拟机失败: %w", err)
	}
	if result != nil && !result.Success {
		err := fmt.Errorf("%s", result.Message)
		e.taskLog(t, "更新虚拟机").VM(newCfg.VmUUID, newCfg.OsName).
			Duration(time.Since(start)).Err(err).
			Errorf("虚拟机更新失败：%s", result.Message)
		return err
	}

	if err := e.app.DB.UpsertVMConfig(t.HsName, newCfg.VmUUID, newCfg); err != nil {
		e.taskLog(t, "更新虚拟机").VM(newCfg.VmUUID, newCfg.OsName).Err(err).
			Errorf("平台更新成功但保存配置失败：%v", err)
		return fmt.Errorf("保存虚拟机配置失败: %w", err)
	}

	if user := taskQuotaUser(t); user != "" && oldCfg != nil {
		if err := e.app.QuotaSvc.AdjustQuota(user, oldCfg, newCfg); err != nil {
			e.app.Logger.Event("quota", "配额调整").Host(t.HsName).VM(newCfg.VmUUID, newCfg.OsName).
				User(t.CreatedBy).Detail("quota_user", user).Err(err).
				Warn("虚拟机 %s 更新后调整配额失败: %v", newCfg.VmUUID, err)
		}
	}

	// 网卡 IP/MAC 可能被改动，重新下发静态绑定（对应 Python 版 VMUpdate → IPBinder(True)）
	e.bindStatic(t, newCfg)

	e.taskLog(t, "更新虚拟机").VM(newCfg.VmUUID, newCfg.OsName).
		Detail("old", vmSpecDetails(oldCfg)).
		Detail("new", vmSpecDetails(newCfg)).
		Duration(time.Since(start)).
		Info("虚拟机配置更新成功：%s", newCfg.VmUUID)

	_ = e.app.TaskManager.UpdateProgress(t.ID, 100, "更新完成")
	return nil
}

// reinstallVM 重装虚拟机系统
func (e *vmTaskExecutor) reinstallVM(ctx context.Context, t *task.Task) error {
	start := time.Now()
	cfg := e.loadVMConfig(t.HsName, t.VmUUID)
	if cfg == nil {
		e.taskLog(t, "重装系统").Errorf("虚拟机不存在：%s/%s", t.HsName, t.VmUUID)
		return fmt.Errorf("虚拟机不存在: %s/%s", t.HsName, t.VmUUID)
	}
	d := e.app.HostSvc.GetDriver(t.HsName)
	if d == nil {
		e.taskLog(t, "重装系统").VM(cfg.VmUUID, cfg.OsName).
			Errorf("主机不存在或未启用：%s", t.HsName)
		return fmt.Errorf("主机不存在或未启用: %s", t.HsName)
	}

	e.taskLog(t, "重装系统").VM(cfg.VmUUID, cfg.OsName).
		Details(vmSpecDetails(cfg)).Debug("开始向平台下发重装请求")

	_ = e.app.TaskManager.UpdateProgress(t.ID, 20, "正在重装系统")
	result, err := d.ReinstallVM(cfg)
	if err != nil {
		e.taskLog(t, "重装系统").VM(cfg.VmUUID, cfg.OsName).
			Duration(time.Since(start)).Err(err).
			Errorf("系统重装失败：%v", err)
		return fmt.Errorf("重装系统失败: %w", err)
	}
	if result != nil && !result.Success {
		err := fmt.Errorf("%s", result.Message)
		e.taskLog(t, "重装系统").VM(cfg.VmUUID, cfg.OsName).
			Duration(time.Since(start)).Err(err).
			Errorf("系统重装失败：%s", result.Message)
		return err
	}

	if err := e.app.DB.UpsertVMConfig(t.HsName, cfg.VmUUID, cfg); err != nil {
		e.taskLog(t, "重装系统").VM(cfg.VmUUID, cfg.OsName).Err(err).
			Errorf("平台重装成功但保存配置失败：%v", err)
		return fmt.Errorf("保存虚拟机配置失败: %w", err)
	}

	// 重装会重置系统盘：属于高影响操作，用 WARNING 级别确保重点留痕（结果仍为成功）
	e.taskLog(t, "重装系统").VM(cfg.VmUUID, cfg.OsName).
		Details(vmSpecDetails(cfg)).Duration(time.Since(start)).
		Emit(logger.LevelWarn, "系统重装已提交并完成：%s（%s）", cfg.VmUUID, cfg.OsName)

	_ = e.app.TaskManager.UpdateProgress(t.ID, 100, "重装完成")
	return nil
}

// deleteVM 删除虚拟机。
//
// 委托给 service.DeleteVMWorkflow：该流程带持久化墓碑，可断点续做，
// 外部调用失败会转入 outbox 后台重试，而不是让删除卡住或留下残留。
// 顺序与 Python 版 _task_delete_vm 一致：
// 撤销端口映射 → 删除平台虚拟机 → 清理本地记录 → 释放配额。
func (e *vmTaskExecutor) deleteVM(ctx context.Context, t *task.Task) error {
	start := time.Now()
	_ = e.app.TaskManager.UpdateProgress(t.ID, 40, "正在删除虚拟机")

	if e.app.DeleteWorkflow == nil {
		e.taskLog(t, "删除虚拟机").Errorf("删除工作流未初始化")
		return fmt.Errorf("删除工作流未初始化")
	}

	cfg := e.loadVMConfig(t.HsName, t.VmUUID)
	if cfg != nil {
		e.taskLog(t, "删除虚拟机").VM(cfg.VmUUID, cfg.OsName).
			Details(vmSpecDetails(cfg)).Debug("开始删除虚拟机（含端口映射与配额释放）")
	} else {
		e.taskLog(t, "删除虚拟机").Debug("开始删除虚拟机 %s", t.VmUUID)
	}

	if err := e.app.DeleteWorkflow.Run(t.HsName, t.VmUUID, taskQuotaUser(t)); err != nil {
		e.taskLog(t, "删除虚拟机").Duration(time.Since(start)).Err(err).
			Errorf("虚拟机删除失败：%v", err)
		return err
	}

	// 删除属于不可逆的高影响操作：用 WARNING 级别确保重点留痕（结果仍为成功）
	e.taskLog(t, "删除虚拟机").VM(t.VmUUID, "").
		Detail("quota_user", taskQuotaUser(t)).
		Duration(time.Since(start)).
		Emit(logger.LevelWarn, "虚拟机已删除：%s", t.VmUUID)

	_ = e.app.TaskManager.UpdateProgress(t.ID, 100, "删除完成")
	return nil
}
