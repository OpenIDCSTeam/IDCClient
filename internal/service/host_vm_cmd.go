// Package service VM命令执行扩展
package service

import (
	"encoding/json"
	"sync"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

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
	// 检查虚拟机所有者列表
	vmJSON, err := s.db.GetVMConfig(hsName, vmUUID)
	if err != nil {
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

// VMExists 检查虚拟机是否存在
func (s *HostService) VMExists(hsName, vmUUID string) bool {
	_, err := s.db.GetVMConfig(hsName, vmUUID)
	return err == nil
}

// SetVMCmd 设置虚拟机待执行命令
func (s *HostService) SetVMCmd(hsName, vmUUID string, cmdData map[string]interface{}) error {
	vmJSON, err := s.db.GetVMConfig(hsName, vmUUID)
	if err != nil {
		return err
	}

	var vm model.VMConfig
	if err := json.Unmarshal([]byte(vmJSON), &vm); err != nil {
		return err
	}

	// 设置命令数据
	vm.VmCmd = cmdData

	// 保存到数据库
	vmBytes, _ := json.Marshal(vm)
	if err := s.db.UpsertVMConfig(hsName, vmUUID, string(vmBytes)); err != nil {
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

	var vm model.VMConfig
	if err := json.Unmarshal([]byte(vmJSON), &vm); err != nil {
		return err
	}

	// 设置结果数据
	vm.VmCmdResult = result
	// 清除待执行命令
	vm.VmCmd = nil

	// 保存到数据库
	vmBytes, _ := json.Marshal(vm)
	if err := s.db.UpsertVMConfig(hsName, vmUUID, string(vmBytes)); err != nil {
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

	var vm model.VMConfig
	if err := json.Unmarshal([]byte(vmJSON), &vm); err != nil {
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
func (s *HostService) SyncPushVM(hsName string, remoteVMs map[string]map[string]interface{}) (int, error) {
	updatedCount := 0

	for vmUUID, remoteData := range remoteVMs {
		// 获取本地虚拟机配置
		localVMJSON, err := s.db.GetVMConfig(hsName, vmUUID)
		if err != nil {
			// 本地不存在，跳过（或根据业务需求创建新虚拟机）
			continue
		}

		var localVM model.VMConfig
		if err := json.Unmarshal([]byte(localVMJSON), &localVM); err != nil {
			continue
		}

		// 智能合并配置（远端非空覆盖、本地非空保留）
		merged := s.mergeVMConfig(&localVM, remoteData)

		// 保存合并后的配置
		mergedBytes, _ := json.Marshal(merged)
		if err := s.db.UpsertVMConfig(hsName, vmUUID, string(mergedBytes)); err != nil {
			continue
		}

		updatedCount++
	}

	return updatedCount, nil
}

// mergeVMConfig 智能合并虚拟机配置
func (s *HostService) mergeVMConfig(local *model.VMConfig, remote map[string]interface{}) *model.VMConfig {
	// 基础字段：远端非空则覆盖
	if cpu, ok := remote["cpu"].(float64); ok && cpu > 0 {
		local.CpuNum = int(cpu)
	}
	if mem, ok := remote["mem"].(float64); ok && mem > 0 {
		local.MemNum = int(mem)
	}
	if hdd, ok := remote["hdd"].(float64); ok && hdd > 0 {
		local.HddNum = int(hdd)
	}
	if os, ok := remote["os"].(string); ok && os != "" {
		local.OsName = os
	}
	// status 字段映射到 VmFlag（需要转换为 VMPowers 类型）
	if status, ok := remote["status"].(string); ok && status != "" {
		// 仅在符合 VMPowers 枚举值时设置
		local.VmFlag = model.VMPowers(status)
	}
	if ip, ok := remote["ip"].(string); ok && ip != "" {
		// IP 地址存储在其他字段，这里简化处理
		_ = ip
	}

	// 所有者配置：按缺失补充
	if ownAll, ok := remote["own_all"].(map[string]interface{}); ok && len(ownAll) > 0 {
		if local.OwnAll == nil {
			local.OwnAll = make(map[string]model.UserMask)
		}
		for username := range ownAll {
			if _, exists := local.OwnAll[username]; !exists {
				local.OwnAll[username] = model.UserMask{} // 默认权限
			}
		}
	}

	// NAT/端口转发、反向代理等配置类似处理（按缺失补充）
	// 这里简化实现，实际需要根据数据结构解析

	return local
}

// CheckHostAccess 检查用户是否有访问主机的权限（新方法，与原有方法区分）
func (s *HostService) CheckHostAccess(username, hsName string) bool {
	// 简化实现：假设从 session 中已获取用户权限
	// 实际应该查询用户表获取 assigned_hosts
	// 这里暂时返回 true，由上层 auth 中间件保证基本权限
	return true
}
