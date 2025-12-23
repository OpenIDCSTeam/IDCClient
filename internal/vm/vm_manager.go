package vm

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/host"
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/models"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

// VMManager 虚拟机管理器
type VMManager struct {
	hostManager *host.HostManager
	logger      *logrus.Logger
}

// NewVMManager 创建虚拟机管理器
func NewVMManager(hostManager *host.HostManager, logger *logrus.Logger) *VMManager {
	return &VMManager{
		hostManager: hostManager,
		logger:      logger,
	}
}

// GetVMs 获取主机下的所有虚拟机
func (v *VMManager) GetVMs(hsName string) (map[string]*models.VMInfo, error) {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return nil, fmt.Errorf("主机不存在: %s", hsName)
	}

	// 重新加载数据
	err := server.LoadData()
	if err != nil {
		return nil, fmt.Errorf("加载虚拟机数据失败: %w", err)
	}

	vmConfigs := server.GetAllVMs()
	vmInfos := make(map[string]*models.VMInfo)

	for vmUUID, vmConfig := range vmConfigs {
		vmStatus := server.VMStatus(vmUUID)

		// 转换指针切片为值切片
		statusList := make([]models.HWStatus, len(vmStatus))
		for i, status := range vmStatus {
			if status != nil {
				statusList[i] = *status
			}
		}

		vmInfo := &models.VMInfo{
			UUID:         vmUUID,
			Name:         vmConfig.VMName,
			OSName:       vmConfig.OSName,
			CPU:          vmConfig.CPUNum,
			Memory:       int64(vmConfig.RAMNum),
			Disk:         int64(vmConfig.HDDNum),
			Config:       *vmConfig,
			StatusList:   statusList,
			Networks:     make([]models.NCConfig, 0),
			PortForwards: vmConfig.NatAll,
			Proxies:      vmConfig.WebAll,
		}

		// 转换网卡配置
		for _, nicConfig := range vmConfig.NicAll {
			vmInfo.Networks = append(vmInfo.Networks, nicConfig)
		}

		// 获取状态，包含离线判断逻辑
		if len(vmStatus) > 0 {
			latestStatus := vmStatus[len(vmStatus)-1]
			vmInfo.Status = latestStatus.Status
			
			// 获取虚拟机的updated_at时间戳进行离线判断
			vmConfigDAO := v.hostManager.GetVMConfigDAO()
			if vmConfigDAO != nil {
				_, _, updatedAt, err := vmConfigDAO.GetVMConfigWithTimestamp(hsName, vmUUID)
				if err == nil && updatedAt != nil {
					// 如果updated_at比现在早超过10分钟，则视为离线
					if time.Since(*updatedAt) > 10*time.Minute {
						vmInfo.Status = "offline"
					}
				}
			}
		} else {
			vmInfo.Status = "unknown"
		}

		vmInfos[vmUUID] = vmInfo
	}

	return vmInfos, nil
}

// GetVM 获取单个虚拟机信息
func (v *VMManager) GetVM(hsName, vmUUID string) (*models.VMInfo, error) {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return nil, fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return nil, fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	vmStatus := server.VMStatus(vmUUID)

	// 转换指针切片为值切片
	statusList := make([]models.HWStatus, len(vmStatus))
	for i, status := range vmStatus {
		if status != nil {
			statusList[i] = *status
		}
	}

	vmInfo := &models.VMInfo{
		UUID:         vmUUID,
		Name:         vmConfig.VMName,
		OSName:       vmConfig.OSName,
		CPU:          vmConfig.CPUNum,
		Memory:       int64(vmConfig.RAMNum),
		Disk:         int64(vmConfig.HDDNum),
		Config:       *vmConfig,
		StatusList:   statusList,
		Networks:     make([]models.NCConfig, 0),
		PortForwards: vmConfig.NatAll,
		Proxies:      vmConfig.WebAll,
	}

	// 转换网卡配置
	for _, nicConfig := range vmConfig.NicAll {
		vmInfo.Networks = append(vmInfo.Networks, nicConfig)
	}

	// 获取状态，包含离线判断逻辑
	if len(vmStatus) > 0 {
		latestStatus := vmStatus[len(vmStatus)-1]
		vmInfo.Status = latestStatus.Status
		now := time.Now()
		vmInfo.LastSeen = &now
		
		// 获取虚拟机的updated_at时间戳进行离线判断
		vmConfigDAO := v.hostManager.GetVMConfigDAO()
		if vmConfigDAO != nil {
			_, _, updatedAt, err := vmConfigDAO.GetVMConfigWithTimestamp(hsName, vmUUID)
			if err == nil && updatedAt != nil {
				// 如果updated_at比现在早超过10分钟，则视为离线
				if time.Since(*updatedAt) > 10*time.Minute {
					vmInfo.Status = "offline"
				}
			}
		}
	} else {
		vmInfo.Status = "unknown"
	}

	// 获取VNC URL
	if vmConfig.VCEnabled {
		vncURL, err := server.VMConsole(vmUUID)
		if err == nil && vncURL != "" {
			vmInfo.VNCURL = vncURL
		}
	}

	return vmInfo, nil
}

// CreateVM 创建虚拟机
func (v *VMManager) CreateVM(hsName string, vmConfig *models.VMConfig) error {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return fmt.Errorf("主机不存在: %s", hsName)
	}

	// 验证虚拟机配置
	if err := v.validateVMConfig(vmConfig); err != nil {
		return fmt.Errorf("虚拟机配置验证失败: %w", err)
	}

	// 生成VNC端口和密码
	if vmConfig.VCPort == 0 {
		vmConfig.VCPort = v.generateRandomPort()
	}
	if vmConfig.VCPass == "" {
		vmConfig.VCPass = v.generateRandomPassword()
	}

	// 创建虚拟机
	result := server.CreateVM(vmConfig)
	if !result.Success {
		return fmt.Errorf("创建虚拟机失败: %s", result.Message)
	}

	// 执行网络配置
	if len(vmConfig.NicAll) > 0 {
		netResult := server.NCCreate(vmConfig, true)
		if !netResult.Success {
			v.logger.Warnf("网络配置失败: %s", netResult.Message)
		}
	}

	v.logger.Infof("虚拟机 %s 创建成功", vmConfig.VMUUID)
	return nil
}

// UpdateVM 更新虚拟机配置
func (v *VMManager) UpdateVM(hsName, vmUUID string, newConfig *models.VMConfig) error {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return fmt.Errorf("主机不存在: %s", hsName)
	}

	oldConfig := server.GetVM(vmUUID)
	if oldConfig == nil {
		return fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	// 验证新配置
	if err := v.validateVMConfig(newConfig); err != nil {
		return fmt.Errorf("虚拟机配置验证失败: %w", err)
	}

	// 设置UUID
	newConfig.VMUUID = vmUUID

	// 更新虚拟机
	result := server.UpdateVM(newConfig, oldConfig)
	if !result.Success {
		return fmt.Errorf("更新虚拟机失败: %s", result.Message)
	}

	// 如果网络配置发生变化，重新配置网络
	if v.networkConfigChanged(oldConfig, newConfig) {
		// 先删除旧的网络配置
		server.NCCreate(oldConfig, false)
		// 创建新的网络配置
		netResult := server.NCCreate(newConfig, true)
		if !netResult.Success {
			v.logger.Warnf("网络配置更新失败: %s", netResult.Message)
		}
	}

	v.logger.Infof("虚拟机 %s 更新成功", vmUUID)
	return nil
}

// DeleteVM 删除虚拟机
func (v *VMManager) DeleteVM(hsName, vmUUID string) error {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	// 删除网络配置
	if len(vmConfig.NicAll) > 0 {
		server.NCCreate(vmConfig, false)
	}

	// 删除虚拟机
	result := server.DeleteVM(vmUUID)
	if !result.Success {
		return fmt.Errorf("删除虚拟机失败: %s", result.Message)
	}

	v.logger.Infof("虚拟机 %s 删除成功", vmUUID)
	return nil
}

// VMPower 虚拟机电源控制
func (v *VMManager) VMPower(hsName, vmUUID string, action models.VMPowers) error {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	result := server.VMPower(vmUUID, action)
	if !result.Success {
		return fmt.Errorf("电源操作失败: %s", result.Message)
	}

	v.logger.Infof("虚拟机 %s 执行电源操作: %s", vmUUID, action)
	return nil
}

// VMPassword 修改虚拟机密码
func (v *VMManager) VMPassword(hsName, vmUUID, password string) error {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	if password == "" {
		return fmt.Errorf("密码不能为空")
	}

	result := server.VMPassword(vmUUID, password)
	if !result.Success {
		return fmt.Errorf("修改密码失败: %s", result.Message)
	}

	v.logger.Infof("虚拟机 %s 密码修改成功", vmUUID)
	return nil
}

// VMConsole 获取虚拟机VNC控制台URL
func (v *VMManager) VMConsole(hsName, vmUUID string) (string, error) {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return "", fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return "", fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	if !vmConfig.VCEnabled {
		return "", fmt.Errorf("VNC控制台未启用")
	}

	consoleURL, err := server.VMConsole(vmUUID)
	if err != nil {
		return "", fmt.Errorf("获取VNC控制台失败: %w", err)
	}

	v.logger.Infof("获取虚拟机 %s VNC控制台URL: %s", vmUUID, consoleURL)
	return consoleURL, nil
}

// VMStatus 获取虚拟机状态
func (v *VMManager) VMStatus(hsName, vmUUID string) ([]*models.HWStatus, error) {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return nil, fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return nil, fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	statuses := server.VMStatus(vmUUID)
	
	// 检查虚拟机是否离线（如果updated_at比现在早超过10分钟）
	vmConfigDAO := v.hostManager.GetVMConfigDAO()
	if vmConfigDAO != nil {
		_, _, updatedAt, err := vmConfigDAO.GetVMConfigWithTimestamp(hsName, vmUUID)
		if err == nil && updatedAt != nil {
			// 如果updated_at比现在早超过10分钟，则视为离线
			if time.Since(*updatedAt) > 10*time.Minute {
				// 如果离线且状态列表不为空，更新最新状态为offline
				if len(statuses) > 0 {
					latestStatus := statuses[len(statuses)-1]
					if latestStatus != nil {
						latestStatus.Status = "offline"
						latestStatus.AcStatus = "STOPPED" // 同时更新ac_status字段
					}
				}
			}
		}
	}
	
	return statuses, nil
}

// ScanVMs 扫描主机上的虚拟机
func (v *VMManager) ScanVMs(hsName, prefix string) (*models.ZMessage, error) {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return nil, fmt.Errorf("主机不存在: %s", hsName)
	}

	// 重新加载数据
	err := server.LoadData()
	if err != nil {
		return nil, fmt.Errorf("加载数据失败: %w", err)
	}

	result := server.VMScan(prefix)
	if result.Success {
		// 保存配置
		err = server.SaveData()
		if err != nil {
			v.logger.Errorf("保存扫描结果失败: %v", err)
		}

		// 保存系统配置
		if err := v.hostManager.SaveAll(); err != nil {
			v.logger.Errorf("保存系统配置失败: %v", err)
		}
	}

	return result, nil
}

// VMStatusResponse 虚拟机状态响应
type VMStatusResponse struct {
	HSName string `json:"hs_name"`
	VMUUID string `json:"vm_uuid"`
	VMPass string `json:"vm_pass"`
}

// UploadStatus 虚拟机状态上报
func (v *VMManager) UploadStatus(macAddr string, statusData map[string]interface{}) (*VMStatusResponse, error) {
	// 遍历所有主机查找匹配的虚拟机
	hosts := v.hostManager.GetAllHosts()

	for hsName, server := range hosts {
		// 重新加载虚拟机配置
		err := server.LoadData()
		if err != nil {
			v.logger.Errorf("主机 %s 加载配置失败: %v", hsName, err)
			continue
		}

		vmConfigs := server.GetAllVMs()

		for vmUUID, vmConfig := range vmConfigs {
			// 检查网卡MAC地址
			for _, nicConfig := range vmConfig.NicAll {
				if strings.EqualFold(nicConfig.MACAddr, macAddr) {
					// 找到匹配的虚拟机
					v.logger.Infof("找到匹配的虚拟机: 主机=%s, UUID=%s", hsName, vmUUID)

					// 创建状态对象
					hwStatus := &models.HWStatus{}

					// 解析状态数据
					if cpuUsage, ok := statusData["cpu_usage"].(float64); ok {
						hwStatus.CPUUsage = cpuUsage
					}
					if memUsage, ok := statusData["mem_usage"].(float64); ok {
						hwStatus.MemUsage = int64(memUsage)
					}
					if diskUsage, ok := statusData["disk_usage"].(float64); ok {
						hwStatus.DiskUsage = diskUsage
					}
					if fluUsage, ok := statusData["flu_usage"].(float64); ok {
						hwStatus.FluUsage = fluUsage
					}
					if networkIn, ok := statusData["network_in"].(float64); ok {
						hwStatus.NetworkIn = int64(networkIn)
					}
					if networkOut, ok := statusData["network_out"].(float64); ok {
						hwStatus.NetworkOut = int64(networkOut)
					}

					hwStatus.Timestamp = time.Now()
					hwStatus.Status = "running"
					hwStatus.AcStatus = "STARTED" // 设置ac_status字段，与Python版本保持一致

					// 更新虚拟机的updated_at时间戳（仅在有数据上报时修改）
					vmConfigDAO := v.hostManager.GetVMConfigDAO()
					if vmConfigDAO != nil {
						err := vmConfigDAO.UpdateVMStatusTimestamp(hsName, vmUUID)
						if err != nil {
							v.logger.Errorf("更新虚拟机状态时间戳失败: %v", err)
						} else {
							v.logger.Infof("更新虚拟机 %s 状态时间戳成功", vmUUID)
						}
					}

					// 保存状态到数据库
					// 这里需要通过数据库DAO来保存，暂时跳过
					// err = v.statusDAO.AddVMStatus(hsName, vmUUID, hwStatus)
					// if err != nil {
					//     v.logger.Errorf("保存虚拟机状态失败: %v", err)
					// }

					return &VMStatusResponse{
						HSName: hsName,
						VMUUID: vmUUID,
						VMPass: vmConfig.OSPass,
					}, nil
				}
			}
		}
	}

	return nil, fmt.Errorf("未找到MAC地址为 %s 的虚拟机", macAddr)
}

// validateVMConfig 验证虚拟机配置
func (v *VMManager) validateVMConfig(config *models.VMConfig) error {
	if config.VMUUID == "" {
		return fmt.Errorf("虚拟机UUID不能为空")
	}
	if config.VMName == "" {
		return fmt.Errorf("虚拟机名称不能为空")
	}
	if config.CPUNum <= 0 {
		return fmt.Errorf("CPU数量必须大于0")
	}
	if config.RAMNum <= 0 {
		return fmt.Errorf("内存大小必须大于0")
	}
	if config.HDDNum <= 0 {
		return fmt.Errorf("硬盘大小必须大于0")
	}

	// 验证网卡配置
	for nicName, nicConfig := range config.NicAll {
		if nicName == "" {
			return fmt.Errorf("网卡名称不能为空")
		}
		if nicConfig.NicType == "" {
			return fmt.Errorf("网卡类型不能为空")
		}
	}

	return nil
}

// generateRandomPort 生成随机端口
func (v *VMManager) generateRandomPort() int {
	return rand.Intn(40000) + 10000 // 10000-49999
}

// generateRandomPassword 生成随机密码
func (v *VMManager) generateRandomPassword() string {
	return uuid.New().String()[:8]
}

// networkConfigChanged 检查网络配置是否发生变化
func (v *VMManager) networkConfigChanged(oldConfig, newConfig *models.VMConfig) bool {
	if len(oldConfig.NicAll) != len(newConfig.NicAll) {
		return true
	}

	for nicName, oldNic := range oldConfig.NicAll {
		newNic, exists := newConfig.NicAll[nicName]
		if !exists {
			return true
		}

		if oldNic.IP4Addr != newNic.IP4Addr ||
			oldNic.IP6Addr != newNic.IP6Addr ||
			oldNic.NicGate != newNic.NicGate ||
			oldNic.NicMask != newNic.NicMask ||
			oldNic.NicType != newNic.NicType {
			return true
		}
	}

	return false
}

// ============================================================================
// NAT端口转发管理
// ============================================================================

// GetNATRules 获取虚拟机NAT端口转发规则
func (v *VMManager) GetNATRules(hsName, vmUUID string) ([]models.PortData, error) {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return nil, fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return nil, fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	return vmConfig.NatAll, nil
}

// AddNATRule 添加虚拟机NAT端口转发规则
func (v *VMManager) AddNATRule(hsName, vmUUID string, rule *models.PortData) error {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	// 验证规则
	if rule.LanPort == 0 || rule.WanPort == 0 {
		return fmt.Errorf("主机端口和虚拟机端口不能为0")
	}

	// 添加规则
	vmConfig.NatAll = append(vmConfig.NatAll, *rule)

	// 保存配置
	err := server.SaveData()
	if err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}

	v.logger.Infof("虚拟机 %s 添加NAT规则: %d -> %d", vmUUID, rule.LanPort, rule.WanPort)
	return nil
}

// DeleteNATRule 删除虚拟机NAT端口转发规则
func (v *VMManager) DeleteNATRule(hsName, vmUUID string, ruleIndex int) error {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	if ruleIndex < 0 || ruleIndex >= len(vmConfig.NatAll) {
		return fmt.Errorf("规则索引超出范围: %d", ruleIndex)
	}

	// 删除规则
	vmConfig.NatAll = append(vmConfig.NatAll[:ruleIndex], vmConfig.NatAll[ruleIndex+1:]...)

	// 保存配置
	err := server.SaveData()
	if err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}

	v.logger.Infof("虚拟机 %s 删除NAT规则索引: %d", vmUUID, ruleIndex)
	return nil
}

// ============================================================================
// IP地址管理
// ============================================================================

// GetIPAddresses 获取虚拟机IP地址列表
func (v *VMManager) GetIPAddresses(hsName, vmUUID string) (map[string]models.NCConfig, error) {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return nil, fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return nil, fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	return vmConfig.NicAll, nil
}

// AddIPAddress 添加虚拟机IP地址
func (v *VMManager) AddIPAddress(hsName, vmUUID, nicName string, ipConfig *models.NCConfig) error {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	// 验证配置
	if nicName == "" {
		return fmt.Errorf("网卡名称不能为空")
	}
	if ipConfig.NicType == "" {
		return fmt.Errorf("网卡类型不能为空")
	}

	// 检查网卡是否已存在
	if _, exists := vmConfig.NicAll[nicName]; exists {
		return fmt.Errorf("网卡 %s 已存在", nicName)
	}

	// 添加网卡配置
	if vmConfig.NicAll == nil {
		vmConfig.NicAll = make(map[string]models.NCConfig)
	}
	vmConfig.NicAll[nicName] = *ipConfig

	// 保存配置
	err := server.SaveData()
	if err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}

	// 应用网络配置
	result := server.NCCreate(vmConfig, true)
	if !result.Success {
		v.logger.Warnf("应用网络配置失败: %s", result.Message)
	}

	v.logger.Infof("虚拟机 %s 添加IP地址: %s", vmUUID, nicName)
	return nil
}

// UpdateIPAddress 修改虚拟机IP地址配置
func (v *VMManager) UpdateIPAddress(hsName, vmUUID, nicName string, ipConfig *models.NCConfig) error {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	// 检查网卡是否存在
	if _, exists := vmConfig.NicAll[nicName]; !exists {
		return fmt.Errorf("网卡 %s 不存在", nicName)
	}

	// 更新网卡配置
	vmConfig.NicAll[nicName] = *ipConfig

	// 保存配置
	err := server.SaveData()
	if err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}

	// 重新应用网络配置
	server.NCCreate(vmConfig, false) // 先删除
	result := server.NCCreate(vmConfig, true) // 再创建
	if !result.Success {
		v.logger.Warnf("应用网络配置失败: %s", result.Message)
	}

	v.logger.Infof("虚拟机 %s 更新IP地址: %s", vmUUID, nicName)
	return nil
}

// DeleteIPAddress 删除虚拟机IP地址（按索引）
func (v *VMManager) DeleteIPAddress(hsName, vmUUID string, ipIndex int) error {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	if ipIndex < 0 || ipIndex >= len(vmConfig.NicAll) {
		return fmt.Errorf("IP索引超出范围: %d", ipIndex)
	}

	// 获取网卡名称列表
	nicNames := make([]string, 0, len(vmConfig.NicAll))
	for nicName := range vmConfig.NicAll {
		nicNames = append(nicNames, nicName)
	}

	if ipIndex >= len(nicNames) {
		return fmt.Errorf("IP索引超出范围: %d", ipIndex)
	}

	nicName := nicNames[ipIndex]
	return v.DeleteIPAddressByName(hsName, vmUUID, nicName)
}

// DeleteIPAddressByName 删除虚拟机IP地址（按网卡名称）
func (v *VMManager) DeleteIPAddressByName(hsName, vmUUID, nicName string) error {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	// 检查网卡是否存在
	if _, exists := vmConfig.NicAll[nicName]; !exists {
		return fmt.Errorf("网卡 %s 不存在", nicName)
	}

	// 删除网卡配置
	delete(vmConfig.NicAll, nicName)

	// 保存配置
	err := server.SaveData()
	if err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}

	// 重新应用网络配置
	server.NCCreate(vmConfig, false) // 先删除
	result := server.NCCreate(vmConfig, true) // 再创建
	if !result.Success {
		v.logger.Warnf("应用网络配置失败: %s", result.Message)
	}

	v.logger.Infof("虚拟机 %s 删除IP地址: %s", vmUUID, nicName)
	return nil
}

// ============================================================================
// 反向代理管理
// ============================================================================

// GetProxyConfigs 获取虚拟机反向代理配置列表
func (v *VMManager) GetProxyConfigs(hsName, vmUUID string) ([]models.WebProxy, error) {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return nil, fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return nil, fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	return vmConfig.WebAll, nil
}

// AddProxyConfig 添加虚拟机反向代理配置
func (v *VMManager) AddProxyConfig(hsName, vmUUID string, proxyConfig *models.WebProxy) error {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	// 验证配置
	if proxyConfig.WebAddr == "" {
		return fmt.Errorf("域名不能为空")
	}
	if proxyConfig.LanPort == 0 {
		return fmt.Errorf("端口不能为0")
	}

	// 添加代理配置
	vmConfig.WebAll = append(vmConfig.WebAll, *proxyConfig)

	// 保存配置
	err := server.SaveData()
	if err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}

	v.logger.Infof("虚拟机 %s 添加反向代理配置: %s:%d", vmUUID, proxyConfig.WebAddr, proxyConfig.LanPort)
	return nil
}

// DeleteProxyConfig 删除虚拟机反向代理配置
func (v *VMManager) DeleteProxyConfig(hsName, vmUUID string, proxyIndex int) error {
	server := v.hostManager.GetHost(hsName)
	if server == nil {
		return fmt.Errorf("主机不存在: %s", hsName)
	}

	vmConfig := server.GetVM(vmUUID)
	if vmConfig == nil {
		return fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}

	if proxyIndex < 0 || proxyIndex >= len(vmConfig.WebAll) {
		return fmt.Errorf("代理索引超出范围: %d", proxyIndex)
	}

	// 删除代理配置
	vmConfig.WebAll = append(vmConfig.WebAll[:proxyIndex], vmConfig.WebAll[proxyIndex+1:]...)

	// 保存配置
	err := server.SaveData()
	if err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}

	v.logger.Infof("虚拟机 %s 删除反向代理配置索引: %d", vmUUID, proxyIndex)
	return nil
}
