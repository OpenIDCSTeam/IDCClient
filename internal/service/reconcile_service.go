// Package service 外部资源对账服务
//
// 负责两类"无法与本地事务一起提交"的外部状态收敛：
//  1. outbox：路由器端口映射等外部调用的持久化重试；
//  2. tombstone 扫描：进程在删除中途退出后，继续完成剩余步骤。
package service

import (
	"fmt"
	"log"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// maxOutboxAttempts 补偿任务的最大尝试次数，超过后标记为最终失败并等待人工处理
const maxOutboxAttempts = 8

// ReconcileService 外部资源对账服务
type ReconcileService struct {
	db       *data.DB
	natSvc   *NATService
	proxySvc *ProxyService
}

// NewReconcileService 创建对账服务
func NewReconcileService(db *data.DB) *ReconcileService {
	return &ReconcileService{db: db}
}

// SetNATService 注入端口映射服务
func (s *ReconcileService) SetNATService(svc *NATService) {
	s.natSvc = svc
}

// SetProxyService 注入反向代理服务
func (s *ReconcileService) SetProxyService(svc *ProxyService) {
	s.proxySvc = svc
}

// EnqueueResourceTask 登记一条外部资源补偿任务
func (s *ReconcileService) EnqueueResourceTask(hsName, vmUUID, action string, payload interface{}) error {
	if s.db == nil {
		return fmt.Errorf("数据库未初始化")
	}
	return s.db.CreateOutboxEntry(hsName, vmUUID, action, payload)
}

// NATRevokePayload 构造 NAT 撤销补偿任务的载荷
func NATRevokePayload(nat model.PortData) map[string]interface{} {
	return map[string]interface{}{
		"wan_port": nat.WanPort,
		"lan_port": nat.LanPort,
		"lan_addr": nat.LanAddr,
	}
}

// PayloadToNAT 从补偿任务载荷还原端口映射
func PayloadToNAT(p map[string]interface{}) (model.PortData, error) {
	toInt := func(v interface{}) int {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		}
		return 0
	}
	nat := model.PortData{
		WanPort: toInt(p["wan_port"]),
		LanPort: toInt(p["lan_port"]),
	}
	if v, ok := p["lan_addr"].(string); ok {
		nat.LanAddr = v
	}
	if nat.WanPort == 0 && nat.LanPort == 0 && nat.LanAddr == "" {
		return nat, fmt.Errorf("NAT 补偿任务载荷无效")
	}
	return nat, nil
}

// StaticUnbindPayload 构造静态绑定解绑补偿任务的载荷（逐张网卡的 IP/MAC）
func StaticUnbindPayload(vm *model.VMConfig) map[string]interface{} {
	nics := []map[string]interface{}{}
	if vm != nil {
		for _, item := range sortedNics(vm.NicAll) {
			nic := item.Nic
			if nic.IP4Addr == "" && nic.MacAddr == "" {
				continue
			}
			nics = append(nics, map[string]interface{}{
				"ip4_addr": nic.IP4Addr,
				"mac_addr": nic.MacAddr,
			})
		}
	}
	return map[string]interface{}{"nic_all": nics}
}

// PayloadToStaticNics 从补偿任务载荷还原网卡列表（仅用于解绑）
func PayloadToStaticNics(p map[string]interface{}) (*model.VMConfig, error) {
	vm := &model.VMConfig{NicAll: map[string]model.NCConfig{}}
	raw, _ := p["nic_all"].([]interface{})
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		var nic model.NCConfig
		if v, ok := m["ip4_addr"].(string); ok {
			nic.IP4Addr = v
		}
		if v, ok := m["mac_addr"].(string); ok {
			nic.MacAddr = v
		}
		if nic.IP4Addr == "" && nic.MacAddr == "" {
			continue
		}
		vm.NicAll[nic.IP4Addr+"/"+nic.MacAddr] = nic
	}
	if len(vm.NicAll) == 0 {
		return nil, fmt.Errorf("静态解绑补偿任务载荷无效")
	}
	return vm, nil
}

// WebProxyPayload 构造反向代理补偿任务的载荷
func WebProxyPayload(web model.WebProxy) map[string]interface{} {
	return map[string]interface{}{
		"web_addr": web.WebAddr,
		"lan_addr": web.LanAddr,
		"lan_port": web.LanPort,
		"is_https": web.IsHTTPS,
		"web_tips": web.WebTips,
	}
}

// PayloadToWebProxy 从补偿任务载荷还原反向代理配置
func PayloadToWebProxy(p map[string]interface{}) (model.WebProxy, error) {
	toInt := func(v interface{}) int {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		}
		return 0
	}
	web := model.WebProxy{
		LanPort: toInt(p["lan_port"]),
	}
	if v, ok := p["web_addr"].(string); ok {
		web.WebAddr = v
	}
	if v, ok := p["lan_addr"].(string); ok {
		web.LanAddr = v
	}
	if v, ok := p["is_https"].(bool); ok {
		web.IsHTTPS = v
	}
	if v, ok := p["web_tips"].(string); ok {
		web.WebTips = v
	}
	if web.WebAddr == "" {
		return web, fmt.Errorf("反向代理补偿任务载荷缺少域名")
	}
	return web, nil
}

// ProcessOutbox 处理一批到期的补偿任务。
// 返回成功完成数与仍需重试的数量。
func (s *ReconcileService) ProcessOutbox(limit int) (done, retried int) {
	if s.db == nil {
		return 0, 0
	}
	entries, err := s.db.ListPendingOutbox(limit)
	if err != nil {
		log.Printf("[reconcile] 读取补偿任务失败: %v", err)
		return 0, 0
	}

	for _, e := range entries {
		if execErr := s.execute(e); execErr != nil {
			log.Printf("[reconcile] 补偿任务 id=%d action=%s 失败: %v", e.ID, e.Action, execErr)
			if markErr := s.db.MarkOutboxRetry(e.ID, execErr.Error(), maxOutboxAttempts); markErr != nil {
				log.Printf("[reconcile] 更新补偿任务状态失败: %v", markErr)
			}
			retried++
			continue
		}
		if markErr := s.db.MarkOutboxDone(e.ID); markErr != nil {
			log.Printf("[reconcile] 标记补偿任务完成失败: %v", markErr)
			continue
		}
		done++
	}
	return done, retried
}

// execute 执行单条补偿任务
func (s *ReconcileService) execute(e *data.OutboxEntry) error {
	switch e.Action {
	case data.OutboxActionNATRevoke:
		if s.natSvc == nil {
			return fmt.Errorf("NAT 服务未初始化")
		}
		nat, err := PayloadToNAT(e.Payload)
		if err != nil {
			return err
		}
		return s.natSvc.RevokePortMap(e.HsName, e.VmUUID, nat)

	case data.OutboxActionStaticUnbind:
		if s.natSvc == nil {
			return fmt.Errorf("NAT 服务未初始化")
		}
		vm, err := PayloadToStaticNics(e.Payload)
		if err != nil {
			return err
		}
		return s.natSvc.UnbindStaticForVM(e.HsName, vm)

	case data.OutboxActionProxySync:
		if s.proxySvc == nil {
			return fmt.Errorf("反向代理服务未初始化")
		}
		return s.proxySvc.RebuildAndReload()

	case data.OutboxActionProxyRevoke:
		if s.proxySvc == nil {
			return fmt.Errorf("反向代理服务未初始化")
		}
		proxy, err := PayloadToWebProxy(e.Payload)
		if err != nil {
			// 载荷损坏时无法补偿，标记为最终失败而不是无限重试
			return err
		}
		return s.proxySvc.RevokeProxy(e.HsName, e.VmUUID, proxy)

	default:
		return fmt.Errorf("未知的补偿动作: %s", e.Action)
	}
}

// PurgeExpired 清理过期的补偿任务与墓碑记录
func (s *ReconcileService) PurgeExpired(outboxDays, tombstoneDays int) {
	if s.db == nil {
		return
	}
	if n, err := s.db.PurgeOutboxDone(outboxDays); err != nil {
		log.Printf("[reconcile] 清理补偿任务失败: %v", err)
	} else if n > 0 {
		log.Printf("[reconcile] 已清理补偿任务 %d 条", n)
	}
	if n, err := s.db.PurgeTombstones(tombstoneDays); err != nil {
		log.Printf("[reconcile] 清理删除墓碑失败: %v", err)
	} else if n > 0 {
		log.Printf("[reconcile] 已清理删除墓碑 %d 条", n)
	}
}

// PendingCount 返回当前待处理补偿任务数量
func (s *ReconcileService) PendingCount() int {
	if s.db == nil {
		return 0
	}
	n, err := s.db.CountPendingOutbox()
	if err != nil {
		return 0
	}
	return n
}
