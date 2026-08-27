// Package driver 虚拟化平台驱动接口定义
package driver

import (
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// HostDriver 虚拟化平台驱动接口
// 所有虚拟化平台驱动必须实现此接口
type HostDriver interface {
	// GetVMList 获取虚拟机列表
	GetVMList() (map[string]*model.VMConfig, error)

	// GetVMDetail 获取虚拟机详情
	GetVMDetail(vmUUID string) (*model.VMConfig, error)

	// CreateVM 创建虚拟机
	CreateVM(cfg *model.VMConfig) (*model.ZMessage, error)

	// ReinstallVM 重装虚拟机系统
	ReinstallVM(cfg *model.VMConfig) (*model.ZMessage, error)

	// UpdateVM 更新虚拟机配置
	UpdateVM(cfg *model.VMConfig, oldCfg *model.VMConfig) (*model.ZMessage, error)

	// DeleteVM 删除虚拟机
	DeleteVM(vmUUID string, removeBackup bool) (*model.ZMessage, error)

	// PowerControl 电源控制
	PowerControl(vmUUID string, action model.VMPowers) (*model.ZMessage, error)

	// GetVNCInfo 获取VNC连接信息
	GetVNCInfo(vmUUID string, ipAddr string) (*model.ZMessage, error)

	// GetScreenshot 获取虚拟机截图（返回base64）
	GetScreenshot(vmUUID string) (string, error)

	// GetStatus 获取主机状态
	GetStatus() (*model.HWStatus, error)

	// GetPower 获取虚拟机电源状态（返回中文状态字符串）
	GetPower(vmUUID string) string

	// ScanVMs 扫描并同步虚拟机列表
	ScanVMs() (*model.ZMessage, error)

	// GetOSImages 获取可用OS镜像列表
	GetOSImages() (map[string]interface{}, error)

	// GetGPUList 获取GPU设备列表
	GetGPUList() (map[string]*model.VFConfig, error)

	// GetPCIList 获取PCI设备列表
	GetPCIList() (map[string]*model.VFConfig, error)

	// GetUSBList 获取USB设备列表
	GetUSBList() (map[string]*model.USBInfos, error)

	// SetupPCI PCI设备直通操作
	SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error)

	// SetupUSB USB设备直通操作
	SetupUSB(vmUUID string, info *model.USBInfos, usbKey string, add bool) (*model.ZMessage, error)

	// MountHDD 挂载/卸载数据盘
	MountHDD(vmUUID string, hdd *model.SDConfig, mount bool) (*model.ZMessage, error)

	// DeleteHDD 删除数据盘
	DeleteHDD(vmUUID string, hddName string) (*model.ZMessage, error)

	// TransferHDD 移交数据盘
	TransferHDD(vmUUID string, hdd *model.SDConfig, targetVM string) (*model.ZMessage, error)

	// MountISO 挂载/卸载ISO
	MountISO(vmUUID string, iso *model.IMConfig, mount bool) (*model.ZMessage, error)

	// MountUSB 挂载/卸载USB存储
	MountUSB(vmUUID string, usbKey string, mount bool) (*model.ZMessage, error)

	// CreateBackup 创建备份
	CreateBackup(vmUUID string, tips string) (*model.ZMessage, error)

	// RestoreBackup 还原备份
	RestoreBackup(vmUUID string, backupName string) (*model.ZMessage, error)

	// DeleteBackup 删除备份
	DeleteBackup(vmUUID string, backupName string) (*model.ZMessage, error)

	// ScanBackups 扫描备份文件
	ScanBackups() (*model.ZMessage, error)

	// SetPassword 设置虚拟机密码
	SetPassword(vmUUID string, password string) (*model.ZMessage, error)

	// GetVMSaving 获取内存中的虚拟机配置map
	GetVMSaving() map[string]*model.VMConfig

	// SetVMSaving 设置虚拟机配置（用于从数据库恢复）
	SetVMSaving(vms map[string]*model.VMConfig)

	// GetConfig 获取主机配置
	GetConfig() *model.HSConfig

	// Crontab 执行定时任务（状态同步）
	Crontab() bool
}

// NewDriver 根据主机类型创建对应驱动
func NewDriver(cfg *model.HSConfig) HostDriver {
	switch cfg.ServerType {
	case "PromoxSetup":
		return NewProxmoxDriver(cfg)
	case "vSphereESXi":
		return NewVSphereDriver(cfg)
	case "LxContainer":
		return NewLXDDriver(cfg)
	case "OCInterface":
		return NewDockerDriver(cfg)
	case "HyperVSetup":
		return NewHyperVDriver(cfg)
	case "QEMUServer":
		return NewQEMUDriver(cfg)
	case "VMWareSetup":
		return NewWorkstationDriver(cfg)
	case "VirtualBoxs":
		return NewVirtualBoxDriver(cfg)
	case "MemuAndroid":
		return NewMemuDriver(cfg)
	case "QingzhouYun":
		return NewQingzhouDriver(cfg)
	case "SmolVM":
		return NewSmolVMDriver(cfg)
	default:
		return nil
	}
}

// PowerStatusMap 电源状态中文映射
var PowerStatusMap = map[string]model.VMPowers{
	"运行中": model.VMPowerStarted,
	"已关机": model.VMPowerStopped,
	"已停止": model.VMPowerStopped,
	"已暂停": model.VMPowerSuspend,
	"未知":  model.VMPowerUnknown,
	"":    model.VMPowerUnknown,
}

// PowerActionMap 电源操作到中间状态映射
var PowerActionMap = map[model.VMPowers]model.VMPowers{
	model.VMPowerSStart: model.VMPowerOnOpen,
	model.VMPowerSClose: model.VMPowerOnStop,
	model.VMPowerHClose: model.VMPowerStopped,
	model.VMPowerSReset: model.VMPowerOnStop,
	model.VMPowerHReset: model.VMPowerOnStop,
	model.VMPowerAPause: model.VMPowerOnSave,
	model.VMPowerAWaked: model.VMPowerOnWake,
}
