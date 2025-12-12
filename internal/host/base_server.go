package host

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/database"
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/models"
	"github.com/sirupsen/logrus"
)

// BaseServer 基础服务器接口
type BaseServer interface {
	// 初始化和生命周期
	Create() *models.ZMessage
	Load() *models.ZMessage
	Unload() *models.ZMessage
	Delete() *models.ZMessage

	// 虚拟机管理
	CreateVM(config *models.VMConfig) *models.ZMessage
	UpdateVM(newConfig, oldConfig *models.VMConfig) *models.ZMessage
	DeleteVM(vmUUID string) *models.ZMessage
	GetVM(vmUUID string) *models.VMConfig
	GetAllVMs() map[string]*models.VMConfig
	VMStatus(vmUUID string) []*models.HWStatus
	VMPower(vmUUID string, action models.VMPowers) *models.ZMessage
	VMPassword(vmUUID, password string) *models.ZMessage
	VMScan(prefix string) *models.ZMessage
	VMConsole(vmUUID string) (string, error)

	// 网络管理
	NetCheck(config *models.VMConfig) (*models.VMConfig, *models.ZMessage)
	NCCreate(config *models.VMConfig, flag bool) *models.ZMessage
	NCUpdate(newConfig, oldConfig *models.VMConfig) *models.ZMessage
	PortsMap(mapInfo *models.PortData, flag bool) *models.ZMessage
	ProxyMap(webInfo *models.WebProxy, proxyMgr interface{}, flag bool) *models.ZMessage

	// 状态和监控
	GetHostStatus() *models.HSStatus
	AddHostStatus(status *models.HSStatus) error
	GetAllHostStatus() []*models.HSStatus

	// 日志管理
	AddLog(log *models.ZMessage) error
	GetLogs(limit int) ([]*models.ZMessage, error)
	CleanOldLogs(days int) (int, error)

	// 工具方法
	GetConfig() *models.HSConfig
	SetConfig(config *models.HSConfig)
	SaveData() error
	LoadData() error
	GetAllocatedIPs() map[string]bool

	// 定时任务
	ExecuteCron() error
}

// AbstractBaseServer 抽象基础服务器实现
type AbstractBaseServer struct {
	hsConfig   *models.HSConfig
	vmSaving   map[string]*models.VMConfig
	vmRemote   interface{}
	db         *database.Database
	hostConfigDAO *database.HostConfigDAO
	vmConfigDAO   *database.VMConfigDAO
	statusDAO    *database.StatusDAO
	logDAO       *database.LogDAO
	logger       *logrus.Logger
	createdAt    time.Time
	updatedAt    time.Time
}

// NewAbstractBaseServer 创建抽象基础服务器
func NewAbstractBaseServer(config *models.HSConfig, db *database.Database, logger *logrus.Logger) *AbstractBaseServer {
	return &AbstractBaseServer{
		hsConfig:     config,
		vmSaving:     make(map[string]*models.VMConfig),
		db:           db,
		hostConfigDAO: database.NewHostConfigDAO(db),
		vmConfigDAO:   database.NewVMConfigDAO(db),
		statusDAO:    database.NewStatusDAO(db),
		logDAO:       database.NewLogDAO(db),
		logger:       logger,
		createdAt:    time.Now(),
		updatedAt:    time.Now(),
	}
}

// Create 创建主机
func (a *AbstractBaseServer) Create() *models.ZMessage {
	logMsg := models.NewZMessage(true, "HSCreate", "主机创建成功")
	a.AddLog(logMsg)
	
	// 创建主机目录
	if a.hsConfig.SystemPath != "" {
		if err := os.MkdirAll(a.hsConfig.SystemPath, 0755); err != nil {
			logMsg := models.NewZMessage(false, "HSCreate", fmt.Sprintf("创建主机目录失败: %v", err))
			a.AddLog(logMsg)
			return logMsg
		}
	}

	return logMsg
}

// Load 加载主机
func (a *AbstractBaseServer) Load() *models.ZMessage {
	err := a.LoadData()
	if err != nil {
		logMsg := models.NewZMessage(false, "HSLoader", fmt.Sprintf("加载主机数据失败: %v", err))
		a.AddLog(logMsg)
		return logMsg
	}

	logMsg := models.NewZMessage(true, "HSLoader", "主机加载成功")
	a.AddLog(logMsg)
	return logMsg
}

// Unload 卸载主机
func (a *AbstractBaseServer) Unload() *models.ZMessage {
	logMsg := models.NewZMessage(true, "HSUnload", "主机卸载成功")
	a.AddLog(logMsg)
	return logMsg
}

// Delete 删除主机
func (a *AbstractBaseServer) Delete() *models.ZMessage {
	// 删除主机目录
	if a.hsConfig.SystemPath != "" {
		if err := os.RemoveAll(a.hsConfig.SystemPath); err != nil {
			a.logger.Warnf("删除主机目录失败: %v", err)
		}
	}

	logMsg := models.NewZMessage(true, "HSDelete", "主机删除成功")
	a.AddLog(logMsg)
	return logMsg
}

// GetConfig 获取主机配置
func (a *AbstractBaseServer) GetConfig() *models.HSConfig {
	return a.hsConfig
}

// SetConfig 设置主机配置
func (a *AbstractBaseServer) SetConfig(config *models.HSConfig) {
	a.hsConfig = config
	a.updatedAt = time.Now()
}

// SaveData 保存数据
func (a *AbstractBaseServer) SaveData() error {
	// 保存主机配置
	err := a.hostConfigDAO.SetHostConfig(a.hsConfig.ServerName, a.hsConfig)
	if err != nil {
		return fmt.Errorf("保存主机配置失败: %w", err)
	}

	// 保存虚拟机配置
	err = a.vmConfigDAO.SetVMConfig(a.hsConfig.ServerName, a.vmSaving)
	if err != nil {
		return fmt.Errorf("保存虚拟机配置失败: %w", err)
	}

	return nil
}

// LoadData 加载数据
func (a *AbstractBaseServer) LoadData() error {
	// 加载主机配置
	config, err := a.hostConfigDAO.GetHostConfig(a.hsConfig.ServerName)
	if err != nil {
		return fmt.Errorf("加载主机配置失败: %w", err)
	}
	if config != nil {
		a.hsConfig = config
	}

	// 加载虚拟机配置
	vmConfigs, err := a.vmConfigDAO.GetVMConfigs(a.hsConfig.ServerName)
	if err != nil {
		return fmt.Errorf("加载虚拟机配置失败: %w", err)
	}
	a.vmSaving = vmConfigs

	return nil
}

// GetVM 获取虚拟机配置
func (a *AbstractBaseServer) GetVM(vmUUID string) *models.VMConfig {
	return a.vmSaving[vmUUID]
}

// GetAllVMs 获取所有虚拟机配置
func (a *AbstractBaseServer) GetAllVMs() map[string]*models.VMConfig {
	return a.vmSaving
}

// AddLog 添加日志
func (a *AbstractBaseServer) AddLog(log *models.ZMessage) error {
	return a.logDAO.AddLog(a.hsConfig.ServerName, log)
}

// GetLogs 获取日志
func (a *AbstractBaseServer) GetLogs(limit int) ([]*models.ZMessage, error) {
	return a.logDAO.GetLogs(a.hsConfig.ServerName, limit)
}

// CleanOldLogs 清理旧日志
func (a *AbstractBaseServer) CleanOldLogs(days int) (int, error) {
	err := a.logDAO.CleanOldLogs(days)
	if err != nil {
		return 0, err
	}
	return days, nil
}

// AddHostStatus 添加主机状态
func (a *AbstractBaseServer) AddHostStatus(status *models.HSStatus) error {
	return a.statusDAO.AddHostStatus(a.hsConfig.ServerName, status)
}

// GetAllHostStatus 获取所有主机状态
func (a *AbstractBaseServer) GetAllHostStatus() []*models.HSStatus {
	statuses, err := a.statusDAO.GetHostStatus(a.hsConfig.ServerName)
	if err != nil {
		a.logger.Errorf("获取主机状态失败: %v", err)
		return nil
	}
	return statuses
}

// GetHostStatus 获取主机状态
func (a *AbstractBaseServer) GetHostStatus() *models.HSStatus {
	status, err := a.statusDAO.GetLatestHostStatus(a.hsConfig.ServerName)
	if err != nil {
		a.logger.Errorf("获取最新主机状态失败: %v", err)
		return nil
	}
	return status
}

// VMStatus 获取虚拟机状态
func (a *AbstractBaseServer) VMStatus(vmUUID string) []*models.HWStatus {
	statuses, err := a.statusDAO.GetVMStatus(a.hsConfig.ServerName, vmUUID)
	if err != nil {
		a.logger.Errorf("获取虚拟机状态失败: %v", err)
		return nil
	}
	return statuses
}

// GetAllocatedIPs 获取已分配的IP地址
func (a *AbstractBaseServer) GetAllocatedIPs() map[string]bool {
	allocated := make(map[string]bool)
	for _, vmConfig := range a.vmSaving {
		for _, nicConfig := range vmConfig.NicAll {
			if nicConfig.IP4Addr != "" {
				allocated[nicConfig.IP4Addr] = true
			}
			if nicConfig.IP6Addr != "" {
				allocated[nicConfig.IP6Addr] = true
			}
		}
	}
	return allocated
}

// ExecuteCron 执行定时任务
func (a *AbstractBaseServer) ExecuteCron() error {
	// 获取主机状态
	hostStatus := a.getSystemStatus()
	if hostStatus != nil {
		err := a.AddHostStatus(hostStatus)
		if err != nil {
			a.logger.Errorf("保存主机状态失败: %v", err)
		}
	}

	a.logger.Debugf("执行主机 %s 的定时任务", a.hsConfig.ServerName)
	return nil
}

// getSystemStatus 获取系统状态（需要具体实现）
func (a *AbstractBaseServer) getSystemStatus() *models.HSStatus {
	// 这里需要实现具体的系统状态获取逻辑
	// 暂时返回一个默认状态
	now := time.Now()
	return &models.HSStatus{
		HostStatus: &models.HWStatus{
			Timestamp: now,
			Status:    "running",
		},
		Version:  "1.0.0",
		Uptime:   int64(time.Since(a.createdAt).Seconds()),
		Platform: "unknown",
		Arch:     "unknown",
	}
}

// CreateVM 创建虚拟机（抽象方法，需要具体实现）
func (a *AbstractBaseServer) CreateVM(config *models.VMConfig) *models.ZMessage {
	// 保存到内存
	a.vmSaving[config.VMUUID] = config
	
	// 保存到数据库
	err := a.SaveData()
	if err != nil {
		logMsg := models.NewZMessage(false, "VMCreate", fmt.Sprintf("保存虚拟机配置失败: %v", err))
		a.AddLog(logMsg)
		return logMsg
	}

	logMsg := models.NewZMessage(true, "VMCreate", fmt.Sprintf("虚拟机 %s 创建成功", config.VMUUID))
	a.AddLog(logMsg)
	return logMsg
}

// UpdateVM 更新虚拟机（抽象方法，需要具体实现）
func (a *AbstractBaseServer) UpdateVM(newConfig, oldConfig *models.VMConfig) *models.ZMessage {
	// 更新内存中的配置
	a.vmSaving[newConfig.VMUUID] = newConfig
	
	// 保存到数据库
	err := a.SaveData()
	if err != nil {
		logMsg := models.NewZMessage(false, "VMUpdate", fmt.Sprintf("保存虚拟机配置失败: %v", err))
		a.AddLog(logMsg)
		return logMsg
	}

	logMsg := models.NewZMessage(true, "VMUpdate", fmt.Sprintf("虚拟机 %s 更新成功", newConfig.VMUUID))
	a.AddLog(logMsg)
	return logMsg
}

// DeleteVM 删除虚拟机（抽象方法，需要具体实现）
func (a *AbstractBaseServer) DeleteVM(vmUUID string) *models.ZMessage {
	// 删除虚拟机目录
	vmPath := filepath.Join(a.hsConfig.SystemPath, vmUUID)
	if err := os.RemoveAll(vmPath); err != nil {
		a.logger.Warnf("删除虚拟机目录失败: %v", err)
	}

	// 从内存中删除
	delete(a.vmSaving, vmUUID)
	
	// 保存到数据库
	err := a.SaveData()
	if err != nil {
		logMsg := models.NewZMessage(false, "VMDelete", fmt.Sprintf("保存配置失败: %v", err))
		a.AddLog(logMsg)
		return logMsg
	}

	logMsg := models.NewZMessage(true, "VMDelete", fmt.Sprintf("虚拟机 %s 删除成功", vmUUID))
	a.AddLog(logMsg)
	return logMsg
}

// 其他抽象方法需要具体实现类来重写
func (a *AbstractBaseServer) VMPower(vmUUID string, action models.VMPowers) *models.ZMessage {
	return models.NewZMessage(false, "VMPower", "该方法需要在具体实现类中重写")
}

func (a *AbstractBaseServer) VMPassword(vmUUID, password string) *models.ZMessage {
	vmConfig := a.GetVM(vmUUID)
	if vmConfig == nil {
		return models.NewZMessage(false, "Password", "虚拟机不存在")
	}

	// 创建新配置
	newConfig := *vmConfig
	newConfig.OSPass = password

	return a.UpdateVM(&newConfig, vmConfig)
}

func (a *AbstractBaseServer) VMScan(prefix string) *models.ZMessage {
	return models.NewZMessage(false, "VMScan", "该方法需要在具体实现类中重写")
}

func (a *AbstractBaseServer) VMConsole(vmUUID string) (string, error) {
	return "", fmt.Errorf("该方法需要在具体实现类中重写")
}

func (a *AbstractBaseServer) NetCheck(config *models.VMConfig) (*models.VMConfig, *models.ZMessage) {
	return config, models.NewZMessage(true, "NetCheck", "网络检查完成")
}

func (a *AbstractBaseServer) NCCreate(config *models.VMConfig, flag bool) *models.ZMessage {
	return models.NewZMessage(false, "NCCreate", "该方法需要在具体实现类中重写")
}

func (a *AbstractBaseServer) NCUpdate(newConfig, oldConfig *models.VMConfig) *models.ZMessage {
	return models.NewZMessage(false, "NCUpdate", "该方法需要在具体实现类中重写")
}

func (a *AbstractBaseServer) PortsMap(mapInfo *models.PortData, flag bool) *models.ZMessage {
	return models.NewZMessage(false, "PortsMap", "该方法需要在具体实现类中重写")
}

func (a *AbstractBaseServer) ProxyMap(webInfo *models.WebProxy, proxyMgr interface{}, flag bool) *models.ZMessage {
	return models.NewZMessage(false, "ProxyMap", "该方法需要在具体实现类中重写")
}