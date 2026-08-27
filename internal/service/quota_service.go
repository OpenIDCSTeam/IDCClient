// Package service 配额管理服务
package service

import (
	"fmt"
	"sync"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// QuotaService 配额管理服务
type QuotaService struct {
	DB *data.DB
	mu sync.Mutex // 配额操作锁,防止并发竞争
}

// NewQuotaService 创建配额服务
func NewQuotaService(db *data.DB) *QuotaService {
	return &QuotaService{
		DB: db,
	}
}

// CheckQuota 检查用户配额是否充足
func (s *QuotaService) CheckQuota(username string, vmCfg *model.VMConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	userMap, err := s.DB.GetUserByUsername(username)
	if err != nil {
		return fmt.Errorf("获取用户信息失败: %w", err)
	}
	if userMap == nil {
		return fmt.Errorf("用户不存在: %s", username)
	}

	// 使用辅助函数安全获取int值
	usedCPU := model.SafeGetInt(userMap, "used_cpu")
	quotaCPU := model.SafeGetInt(userMap, "quota_cpu")
	usedRAM := model.SafeGetInt(userMap, "used_ram")
	quotaRAM := model.SafeGetInt(userMap, "quota_ram")
	usedSSD := model.SafeGetInt(userMap, "used_ssd")
	quotaSSD := model.SafeGetInt(userMap, "quota_ssd")
	usedGPU := model.SafeGetInt(userMap, "used_gpu")
	quotaGPU := model.SafeGetInt(userMap, "quota_gpu")

	// 检查CPU配额
	if usedCPU+vmCfg.CpuNum > quotaCPU {
		return fmt.Errorf("CPU配额不足: 已用%d/%d核, 需要%d核",
			usedCPU, quotaCPU, vmCfg.CpuNum)
	}

	// 检查内存配额
	if usedRAM+vmCfg.MemNum > quotaRAM {
		return fmt.Errorf("内存配额不足: 已用%d/%d MB, 需要%d MB",
			usedRAM, quotaRAM, vmCfg.MemNum)
	}

	// 检查磁盘配额
	totalDisk := vmCfg.HddNum
	for _, hdd := range vmCfg.HddAll {
		totalDisk += hdd.HddSize
	}
	if usedSSD+totalDisk > quotaSSD {
		return fmt.Errorf("磁盘配额不足: 已用%d/%d MB, 需要%d MB",
			usedSSD, quotaSSD, totalDisk)
	}

	// 检查GPU配额
	if vmCfg.GpuMem > 0 && usedGPU+vmCfg.GpuMem > quotaGPU {
		return fmt.Errorf("GPU配额不足: 已用%d/%d, 需要%d",
			usedGPU, quotaGPU, vmCfg.GpuMem)
	}

	// 检查NAT端口配额
	userID := model.SafeGetInt(userMap, "id")
	quotaNatPorts := model.SafeGetInt(userMap, "quota_nat_ports")
	quotaWebProxy := model.SafeGetInt(userMap, "quota_web_proxy")
	
	natCount := len(vmCfg.NatAll)
	existingNatCount, _ := s.DB.CountUserNATRules(userID)
	if existingNatCount+natCount > quotaNatPorts {
		return fmt.Errorf("NAT端口配额不足: 已用%d/%d, 需要%d",
			existingNatCount, quotaNatPorts, natCount)
	}

	// 检查Web代理配额
	webCount := len(vmCfg.WebAll)
	existingWebCount, _ := s.DB.CountUserWebProxies(userID)
	if existingWebCount+webCount > quotaWebProxy {
		return fmt.Errorf("Web代理配额不足: 已用%d/%d, 需要%d",
			existingWebCount, quotaWebProxy, webCount)
	}

	return nil
}

// DeductQuota 扣减用户配额(创建VM时调用)
func (s *QuotaService) DeductQuota(username string, vmCfg *model.VMConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	userMap, err := s.DB.GetUserByUsername(username)
	if err != nil {
		return fmt.Errorf("获取用户信息失败: %w", err)
	}

	userID := model.SafeGetInt(userMap, "id")
	usedCPU := model.SafeGetInt(userMap, "used_cpu")
	usedRAM := model.SafeGetInt(userMap, "used_ram")
	usedSSD := model.SafeGetInt(userMap, "used_ssd")
	usedGPU := model.SafeGetInt(userMap, "used_gpu")

	// 计算总磁盘大小
	totalDisk := vmCfg.HddNum
	for _, hdd := range vmCfg.HddAll {
		totalDisk += hdd.HddSize
	}

	// 更新资源使用量
	updates := map[string]interface{}{
		"used_cpu": usedCPU + vmCfg.CpuNum,
		"used_ram": usedRAM + vmCfg.MemNum,
		"used_ssd": usedSSD + totalDisk,
	}

	if vmCfg.GpuMem > 0 {
		updates["used_gpu"] = usedGPU + vmCfg.GpuMem
	}

	return s.DB.UpdateUser(userID, updates)
}

// ReleaseQuota 释放用户配额(删除VM时调用)
func (s *QuotaService) ReleaseQuota(username string, vmCfg *model.VMConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	userMap, err := s.DB.GetUserByUsername(username)
	if err != nil {
		return fmt.Errorf("获取用户信息失败: %w", err)
	}

	userID := model.SafeGetInt(userMap, "id")
	usedCPU := model.SafeGetInt(userMap, "used_cpu")
	usedRAM := model.SafeGetInt(userMap, "used_ram")
	usedSSD := model.SafeGetInt(userMap, "used_ssd")
	usedGPU := model.SafeGetInt(userMap, "used_gpu")

	// 计算总磁盘大小
	totalDisk := vmCfg.HddNum
	for _, hdd := range vmCfg.HddAll {
		totalDisk += hdd.HddSize
	}

	// 更新资源使用量(确保不为负数)
	updates := map[string]interface{}{
		"used_cpu": max(0, usedCPU-vmCfg.CpuNum),
		"used_ram": max(0, usedRAM-vmCfg.MemNum),
		"used_ssd": max(0, usedSSD-totalDisk),
	}

	if vmCfg.GpuMem > 0 {
		updates["used_gpu"] = max(0, usedGPU-vmCfg.GpuMem)
	}

	return s.DB.UpdateUser(userID, updates)
}

// AdjustQuota 调整用户配额(更新VM时调用)
func (s *QuotaService) AdjustQuota(username string, oldCfg, newCfg *model.VMConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	userMap, err := s.DB.GetUserByUsername(username)
	if err != nil {
		return fmt.Errorf("获取用户信息失败: %w", err)
	}

	userID := model.SafeGetInt(userMap, "id")
	usedCPU := model.SafeGetInt(userMap, "used_cpu")
	usedRAM := model.SafeGetInt(userMap, "used_ram")
	usedSSD := model.SafeGetInt(userMap, "used_ssd")
	usedGPU := model.SafeGetInt(userMap, "used_gpu")
	quotaCPU := model.SafeGetInt(userMap, "quota_cpu")
	quotaRAM := model.SafeGetInt(userMap, "quota_ram")
	quotaSSD := model.SafeGetInt(userMap, "quota_ssd")
	quotaGPU := model.SafeGetInt(userMap, "quota_gpu")

	// 计算差异
	cpuDiff := newCfg.CpuNum - oldCfg.CpuNum
	memDiff := newCfg.MemNum - oldCfg.MemNum
	gpuDiff := newCfg.GpuMem - oldCfg.GpuMem

	// 计算磁盘差异
	oldDisk := oldCfg.HddNum
	for _, hdd := range oldCfg.HddAll {
		oldDisk += hdd.HddSize
	}
	newDisk := newCfg.HddNum
	for _, hdd := range newCfg.HddAll {
		newDisk += hdd.HddSize
	}
	diskDiff := newDisk - oldDisk

	// 如果是增加资源,先检查配额
	if cpuDiff > 0 && usedCPU+cpuDiff > quotaCPU {
		return fmt.Errorf("CPU配额不足")
	}
	if memDiff > 0 && usedRAM+memDiff > quotaRAM {
		return fmt.Errorf("内存配额不足")
	}
	if diskDiff > 0 && usedSSD+diskDiff > quotaSSD {
		return fmt.Errorf("磁盘配额不足")
	}
	if gpuDiff > 0 && usedGPU+gpuDiff > quotaGPU {
		return fmt.Errorf("GPU配额不足")
	}

	// 更新资源使用量
	updates := map[string]interface{}{
		"used_cpu": max(0, usedCPU+cpuDiff),
		"used_ram": max(0, usedRAM+memDiff),
		"used_ssd": max(0, usedSSD+diskDiff),
		"used_gpu": max(0, usedGPU+gpuDiff),
	}

	return s.DB.UpdateUser(userID, updates)
}

// GetQuotaInfo 获取用户配额信息
func (s *QuotaService) GetQuotaInfo(username string) (map[string]interface{}, error) {
	userMap, err := s.DB.GetUserByUsername(username)
	if err != nil {
		return nil, fmt.Errorf("获取用户信息失败: %w", err)
	}

	usedCPU := model.SafeGetInt(userMap, "used_cpu")
	quotaCPU := model.SafeGetInt(userMap, "quota_cpu")
	usedRAM := model.SafeGetInt(userMap, "used_ram")
	quotaRAM := model.SafeGetInt(userMap, "quota_ram")
	usedSSD := model.SafeGetInt(userMap, "used_ssd")
	quotaSSD := model.SafeGetInt(userMap, "quota_ssd")
	usedGPU := model.SafeGetInt(userMap, "used_gpu")
	quotaGPU := model.SafeGetInt(userMap, "quota_gpu")
	quotaNatPorts := model.SafeGetInt(userMap, "quota_nat_ports")
	quotaWebProxy := model.SafeGetInt(userMap, "quota_web_proxy")

	return map[string]interface{}{
		"cpu": map[string]int{
			"total": quotaCPU,
			"used":  usedCPU,
			"free":  quotaCPU - usedCPU,
		},
		"ram": map[string]int{
			"total": quotaRAM,
			"used":  usedRAM,
			"free":  quotaRAM - usedRAM,
		},
		"ssd": map[string]int{
			"total": quotaSSD,
			"used":  usedSSD,
			"free":  quotaSSD - usedSSD,
		},
		"gpu": map[string]int{
			"total": quotaGPU,
			"used":  usedGPU,
			"free":  quotaGPU - usedGPU,
		},
		"nat_ports": map[string]int{
			"total": quotaNatPorts,
			"free":  quotaNatPorts,
		},
		"web_proxy": map[string]int{
			"total": quotaWebProxy,
			"free":  quotaWebProxy,
		},
	}, nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
