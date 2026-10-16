// Package service VM命令执行扩展
package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// DecodeVMConfig 解析数据库中的虚拟机配置。
//
// 兼容历史上被双重编码的记录（配置被存为「JSON 字符串」而不是对象）：
// 这类记录会让后续 json.Unmarshal 到 VMConfig 失败，从而静默跳过该虚拟机。
func DecodeVMConfig(vmJSON string) (*model.VMConfig, error) {
	if strings.TrimSpace(vmJSON) == "" {
		return nil, fmt.Errorf("虚拟机配置不存在")
	}
	trimmed := strings.TrimSpace(vmJSON)
	if strings.HasPrefix(trimmed, `"`) {
		var inner string
		if err := json.Unmarshal([]byte(trimmed), &inner); err == nil {
			trimmed = inner
		}
	}
	var vm model.VMConfig
	if err := json.Unmarshal([]byte(trimmed), &vm); err != nil {
		return nil, err
	}
	return &vm, nil
}

var (
	cmdIndexMu sync.RWMutex
	cmdIndex   = make(map[string]vmLocation) // cmd_id -> {hs_name, vm_uuid}
)

// vmLocation 虚拟机位置信息
type vmLocation struct {
	HsName string
	VmUUID string
}

// CheckVMOwnership 检查用户是否拥有虚拟机操作权限
func (s *HostService) CheckVMOwnership(username, hsName, vmUUID string) bool {
	// 空用户名必须拒绝：否则所有者表中一旦存在空键，
	// 未认证身份会被判定为“拥有”该虚拟机。
	if username == "" || hsName == "" || vmUUID == "" {
		return false
	}
	// 检查虚拟机所有者列表
	vmJSON, err := s.db.GetVMConfig(hsName, vmUUID)
	if err != nil || vmJSON == "" {
		return false
	}

	var vm model.VMConfig
	if err := json.Unmarshal([]byte(vmJSON), &vm); err != nil {
		return false
	}

	// 检查 own_all 中是否包含该用户
	if vm.OwnAll != nil {
		if _, hasOwnership := vm.OwnAll[username]; hasOwnership {
			return true
		}
	}

	return false
}

// VMCount 返回主机下的虚拟机数量（优先驱动内存，其次数据库）
func (s *HostService) VMCount(hsName string) int {
	if d := s.getDriver(hsName); d != nil {
		return len(d.GetVMSaving())
	}
	vms, err := s.db.GetVMSaving(hsName)
	if err != nil {
		return 0
	}
	return len(vms)
}

// MustGetVMConfig 读取虚拟机配置：优先驱动内存，其次数据库；
// 不存在或无法解析时返回 nil。
func (s *HostService) MustGetVMConfig(hsName, vmUUID string) *model.VMConfig {
	if hsName == "" || vmUUID == "" {
		return nil
	}
	if d := s.GetDriver(hsName); d != nil {
		if cfg, err := d.GetVMDetail(vmUUID); err == nil {
			return cfg
		}
	}
	raw, err := s.db.GetVMConfig(hsName, vmUUID)
	if err != nil || raw == "" {
		return nil
	}
	cfg, err := DecodeVMConfig(raw)
	if err != nil {
		return nil
	}
	return cfg
}

// VMExists 检查虚拟机是否存在
//
// 注意：配置不存在时 GetVMConfig 返回 ("", nil)，
// 因此必须同时判断非空与无错误，否则不存在的计算机会被判定为存在。
func (s *HostService) VMExists(hsName, vmUUID string) bool {
	if hsName == "" || vmUUID == "" {
		return false
	}
	vmJSON, err := s.db.GetVMConfig(hsName, vmUUID)
	return err == nil && vmJSON != ""
}

// SetVMCmd 设置虚拟机待执行命令
func (s *HostService) SetVMCmd(hsName, vmUUID string, cmdData map[string]interface{}) error {
	vmJSON, err := s.db.GetVMConfig(hsName, vmUUID)
	if err != nil {
		return err
	}

	vm, err := DecodeVMConfig(vmJSON)
	if err != nil {
		return err
	}

	// 设置命令数据
	vm.VmCmd = cmdData

	// 保存到数据库：直接传结构体，由仓储统一序列化一次。
	// 此前先 Marshal 成 string 再传入，仓储会二次 Marshal，
	// 结果是配置被存为「JSON 字符串」而非对象，后续无法解析。
	if err := s.db.UpsertVMConfig(hsName, vmUUID, &vm); err != nil {
		return err
	}

	// 建立 cmd_id 索引（用于快速查找）
	if cmdID, ok := cmdData["cmd_id"].(string); ok {
		cmdIndexMu.Lock()
		cmdIndex[cmdID] = vmLocation{HsName: hsName, VmUUID: vmUUID}
		cmdIndexMu.Unlock()
	}

	return nil
}

// SetVMCmdResult 设置虚拟机命令执行结果
func (s *HostService) SetVMCmdResult(hsName, vmUUID string, result map[string]interface{}) error {
	vmJSON, err := s.db.GetVMConfig(hsName, vmUUID)
	if err != nil {
		return err
	}

	vm, err := DecodeVMConfig(vmJSON)
	if err != nil {
		return err
	}

	// 仅当回传的 cmd_id 与当前待执行命令一致时才写入结果并清空命令，
	// 避免迟到的旧回包覆盖或清除新下发的命令。
	if cmdID, ok := result["cmd_id"].(string); ok && cmdID != "" {
		if pending, ok := vm.VmCmd["cmd_id"].(string); ok && pending != "" && pending != cmdID {
			return fmt.Errorf("命令回传与当前待执行命令不匹配: %s", cmdID)
		}
	}

	// 设置结果数据
	vm.VmCmdResult = result
	// 清除待执行命令
	vm.VmCmd = nil

	// 保存到数据库：只序列化一次
	if err := s.db.UpsertVMConfig(hsName, vmUUID, vm); err != nil {
		return err
	}

	// 清除 cmd_id 索引
	if cmdID, ok := result["cmd_id"].(string); ok {
		cmdIndexMu.Lock()
		delete(cmdIndex, cmdID)
		cmdIndexMu.Unlock()
	}

	return nil
}

// GetVMCmdStatus 获取虚拟机命令执行状态
func (s *HostService) GetVMCmdStatus(hsName, vmUUID string) (pendingCmd, lastResult map[string]interface{}, err error) {
	vmJSON, err := s.db.GetVMConfig(hsName, vmUUID)
	if err != nil {
		return nil, nil, err
	}

	vm, err := DecodeVMConfig(vmJSON)
	if err != nil {
		return nil, nil, err
	}

	return vm.VmCmd, vm.VmCmdResult, nil
}

// FindVMByCmdID 根据 cmd_id 查找虚拟机（O(1) 复杂度）
func (s *HostService) FindVMByCmdID(cmdID string) (hsName, vmUUID string, found bool) {
	cmdIndexMu.RLock()
	loc, exists := cmdIndex[cmdID]
	cmdIndexMu.RUnlock()

	if !exists {
		return "", "", false
	}

	return loc.HsName, loc.VmUUID, true
}

// SyncPushVM 同步推送虚拟机配置（智能合并）
// 与 Python 版 api_sync_push / _sync_pull 的合并语义保持一致，实现见 vm_merge.go
func (s *HostService) SyncPushVM(hsName string, remoteVMs map[string]map[string]interface{}) (int, error) {
	return s.MergeRemoteVMs(hsName, remoteVMs)
}

// CheckHostAccess 检查用户是否有访问指定主机的权限。
//
// 管理员直接放行；普通用户必须存在、处于启用状态，
// 且 assigned_hosts 中包含该主机。
//
// 该实现取代了此前恒返回 true 的占位逻辑：恒真实现会让
// “已登录”等价于“可操作任意主机”，属于越权边界缺失。
func (s *HostService) CheckHostAccess(username, hsName string) bool {
	if username == "" || hsName == "" {
		return false
	}
	user, err := s.db.GetUserByUsername(username)
	if err != nil || user == nil {
		return false
	}
	if model.SafeGetBool(user, "is_admin") {
		return true
	}
	if !model.SafeGetBool(user, "is_active") {
		return false
	}
	return containsHost(assignedHostsOf(user), hsName)
}

// assignedHostsOf 兼容 []string、JSON 字符串与 []interface{} 三种存储形态。
func assignedHostsOf(user map[string]interface{}) []string {
	switch v := user["assigned_hosts"].(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if v == "" {
			return nil
		}
		var out []string
		if err := json.Unmarshal([]byte(v), &out); err != nil {
			return nil
		}
		return out
	default:
		return nil
	}
}

// containsHost 判断切片中是否包含指定主机名。
func containsHost(hosts []string, hsName string) bool {
	for _, h := range hosts {
		if h == hsName {
			return true
		}
	}
	return false
}
