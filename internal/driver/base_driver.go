// Package driver 基础驱动实现（提供默认方法，各平台驱动嵌入此结构体）
package driver

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// ============================================================
// 全局 HTTP 连接池（复用 TCP 连接，避免频繁握手）
// ============================================================

var (
	// sharedTransport 共享的 HTTP Transport，支持连接池复用
	sharedTransport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	// sharedInsecureTransport 跳过 TLS 验证的共享 Transport（用于自签名证书环境）
	sharedInsecureTransport = &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
	}

	// SharedHTTPClient 共享的 HTTP 客户端（30s 超时）
	SharedHTTPClient = &http.Client{
		Transport: sharedTransport,
		Timeout:   30 * time.Second,
	}

	// SharedInsecureHTTPClient 跳过 TLS 验证的共享 HTTP 客户端
	SharedInsecureHTTPClient = &http.Client{
		Transport: sharedInsecureTransport,
		Timeout:   30 * time.Second,
	}
)

// ============================================================
// 重试与超时工具
// ============================================================

// RetryConfig 重试配置
type RetryConfig struct {
	MaxRetries int           // 最大重试次数（默认3）
	BaseDelay  time.Duration // 基础延迟（默认1s）
	MaxDelay   time.Duration // 最大延迟（默认30s）
}

// DefaultRetryConfig 默认重试配置：3次重试，指数退避1s→2s→4s
var DefaultRetryConfig = RetryConfig{
	MaxRetries: 3,
	BaseDelay:  1 * time.Second,
	MaxDelay:   30 * time.Second,
}

// RetryWithBackoff 带指数退避的重试执行器
// 适用于网络调用、API请求等可能暂时失败的场景
func RetryWithBackoff(ctx context.Context, cfg RetryConfig, fn func() error) error {
	var lastErr error
	for i := 0; i <= cfg.MaxRetries; i++ {
		if err := fn(); err != nil {
			lastErr = err
			if i == cfg.MaxRetries {
				break
			}
			delay := cfg.BaseDelay * time.Duration(1<<uint(i))
			if delay > cfg.MaxDelay {
				delay = cfg.MaxDelay
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("重试 %d 次后仍失败: %w", cfg.MaxRetries, lastErr)
}

// WithTimeout 创建带超时的 context，便捷包装
func WithTimeout(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

// ============================================================
// 状态归一化映射
// ============================================================

// NormalizePowerState 将各平台电源状态统一归一化为中文状态字符串
// 各驱动在 GetPower() 中应返回此映射中的键
var NormalizePowerState = map[string]model.VMPowers{
	"运行中": model.VMPowerStarted,
	"已关机": model.VMPowerStopped,
	"已停止": model.VMPowerStopped,
	"已暂停": model.VMPowerSuspend,
	"已挂起": model.VMPowerSuspend,
	"启动中": model.VMPowerOnOpen,
	"关闭中": model.VMPowerOnStop,
	"保存中": model.VMPowerOnSave,
	"恢复中": model.VMPowerOnWake,
	"未知":   model.VMPowerUnknown,
	"":     model.VMPowerUnknown,
}

// VMPowerToString 将 VMPowers 枚举转为中文状态字符串
func VMPowerToString(p model.VMPowers) string {
	switch p {
	case model.VMPowerStarted:
		return "运行中"
	case model.VMPowerStopped:
		return "已关机"
	case model.VMPowerSuspend:
		return "已暂停"
	case model.VMPowerOnOpen:
		return "启动中"
	case model.VMPowerOnStop:
		return "关闭中"
	case model.VMPowerOnSave:
		return "保存中"
	case model.VMPowerOnWake:
		return "恢复中"
	default:
		return "未知"
	}
}

// BaseDriver 基础驱动，提供通用实现
// 各平台驱动嵌入此结构体，只需覆盖平台特定方法
type BaseDriver struct {
	Config   *model.HSConfig
	VmSaving map[string]*model.VMConfig
	mu       sync.RWMutex
}

// NewBaseDriver 创建基础驱动
func NewBaseDriver(cfg *model.HSConfig) *BaseDriver {
	return &BaseDriver{
		Config:   cfg,
		VmSaving: make(map[string]*model.VMConfig),
	}
}

// GetConfig 获取主机配置
func (b *BaseDriver) GetConfig() *model.HSConfig {
	return b.Config
}

// GetVMSaving 获取虚拟机配置map
func (b *BaseDriver) GetVMSaving() map[string]*model.VMConfig {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make(map[string]*model.VMConfig, len(b.VmSaving))
	for k, v := range b.VmSaving {
		result[k] = v
	}
	return result
}

// SetVMSaving 设置虚拟机配置（从数据库恢复时调用）
func (b *BaseDriver) SetVMSaving(vms map[string]*model.VMConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.VmSaving = vms
}

// GetVMList 获取虚拟机列表（默认返回内存中的配置）
func (b *BaseDriver) GetVMList() (map[string]*model.VMConfig, error) {
	return b.GetVMSaving(), nil
}

// GetVMDetail 获取虚拟机详情
func (b *BaseDriver) GetVMDetail(vmUUID string) (*model.VMConfig, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	cfg, ok := b.VmSaving[vmUUID]
	if !ok {
		return nil, fmt.Errorf("虚拟机 %s 不存在", vmUUID)
	}
	return cfg, nil
}

// GetPower 获取虚拟机电源状态（默认返回未知，子类覆盖）
func (b *BaseDriver) GetPower(vmUUID string) string {
	return "未知"
}

// GetStatus 获取主机状态（默认返回空状态，子类覆盖）
func (b *BaseDriver) GetStatus() (*model.HWStatus, error) {
	return &model.HWStatus{OnUpdate: time.Now().Unix()}, nil
}

// GetScreenshot 获取截图（默认返回空，子类覆盖）
func (b *BaseDriver) GetScreenshot(vmUUID string) (string, error) {
	return "", nil
}

// GetOSImages 获取OS镜像列表（默认返回主机配置中的光盘镜像列表）
func (b *BaseDriver) GetOSImages() (map[string]interface{}, error) {
	result := make(map[string]interface{})
	for _, item := range b.Config.ImagesMaps {
		if !item.SysFlag {
			continue
		}
		result[item.SysName] = item.SysFile
	}
	return result, nil
}

// GetGPUList 获取GPU列表（默认返回空，子类覆盖）
func (b *BaseDriver) GetGPUList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// GetPCIList 获取PCI设备列表（默认返回空，子类覆盖）
func (b *BaseDriver) GetPCIList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// GetUSBList 获取USB设备列表（默认返回空，子类覆盖）
func (b *BaseDriver) GetUSBList() (map[string]*model.USBInfos, error) {
	return make(map[string]*model.USBInfos), nil
}

// SetupPCI PCI直通（默认实现，子类覆盖）
func (b *BaseDriver) SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "PCISetup", Message: "当前平台不支持PCI直通"}, nil
}

// SetupUSB USB直通（默认实现，子类覆盖）
func (b *BaseDriver) SetupUSB(vmUUID string, info *model.USBInfos, usbKey string, add bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "USBSetup", Message: "当前平台不支持USB直通"}, nil
}

// MountHDD 挂载数据盘（默认实现）
func (b *BaseDriver) MountHDD(vmUUID string, hdd *model.SDConfig, mount bool) (*model.ZMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cfg, ok := b.VmSaving[vmUUID]
	if !ok {
		return &model.ZMessage{Success: false, Action: "HDDMount", Message: "虚拟机不存在"}, nil
	}
	if mount {
		hdd.HddFlag = 1
		cfg.HddAll[hdd.HddName] = *hdd
	} else {
		if h, exists := cfg.HddAll[hdd.HddName]; exists {
			h.HddFlag = 0
			cfg.HddAll[hdd.HddName] = h
		}
	}
	action := "挂载"
	if !mount {
		action = "卸载"
	}
	return &model.ZMessage{Success: true, Action: "HDDMount", Message: "磁盘" + action + "成功"}, nil
}

// DeleteHDD 删除数据盘（默认实现）
func (b *BaseDriver) DeleteHDD(vmUUID string, hddName string) (*model.ZMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cfg, ok := b.VmSaving[vmUUID]
	if !ok {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: "虚拟机不存在"}, nil
	}
	delete(cfg.HddAll, hddName)
	return &model.ZMessage{Success: true, Action: "RMMounts", Message: "磁盘删除成功"}, nil
}

// TransferHDD 移交数据盘（默认实现）
func (b *BaseDriver) TransferHDD(vmUUID string, hdd *model.SDConfig, targetVM string) (*model.ZMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	src, ok := b.VmSaving[vmUUID]
	if !ok {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "原始虚拟机不存在"}, nil
	}
	dst, ok := b.VmSaving[targetVM]
	if !ok {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "目标虚拟机不存在"}, nil
	}
	delete(src.HddAll, hdd.HddName)
	hdd.HddFlag = 0
	dst.HddAll[hdd.HddName] = *hdd
	return &model.ZMessage{Success: true, Action: "HDDTrans", Message: "磁盘移交成功"}, nil
}

// MountISO 挂载/卸载ISO（默认实现）
func (b *BaseDriver) MountISO(vmUUID string, iso *model.IMConfig, mount bool) (*model.ZMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cfg, ok := b.VmSaving[vmUUID]
	if !ok {
		return &model.ZMessage{Success: false, Action: "ISOMount", Message: "虚拟机不存在"}, nil
	}
	if mount {
		cfg.IsoAll[iso.IsoName] = *iso
	} else {
		delete(cfg.IsoAll, iso.IsoName)
	}
	action := "挂载"
	if !mount {
		action = "卸载"
	}
	return &model.ZMessage{Success: true, Action: "ISOMount", Message: "ISO" + action + "成功"}, nil
}

// MountUSB 挂载/卸载USB存储（默认实现）
func (b *BaseDriver) MountUSB(vmUUID string, usbKey string, mount bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "USBMount", Message: "当前平台不支持USB存储"}, nil
}

// CreateBackup 创建备份（默认实现）
func (b *BaseDriver) CreateBackup(vmUUID string, tips string) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "VMBackup", Message: "当前平台不支持备份"}, nil
}

// RestoreBackup 还原备份（默认实现）
func (b *BaseDriver) RestoreBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "Restores", Message: "当前平台不支持备份还原"}, nil
}

// DeleteBackup 删除备份（默认实现）
func (b *BaseDriver) DeleteBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "RMBackup", Message: "当前平台不支持备份删除"}, nil
}

// ScanBackups 扫描备份（默认实现）
func (b *BaseDriver) ScanBackups() (*model.ZMessage, error) {
	return &model.ZMessage{Success: true, Action: "LDBackup", Message: "扫描完成"}, nil
}

// SetPassword 设置密码（默认实现，子类覆盖）
func (b *BaseDriver) SetPassword(vmUUID string, password string) (*model.ZMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cfg, ok := b.VmSaving[vmUUID]
	if !ok {
		return &model.ZMessage{Success: false, Action: "Password", Message: "虚拟机不存在"}, nil
	}
	cfg.OsPass = password
	return &model.ZMessage{Success: true, Action: "Password", Message: "密码已更新"}, nil
}

// ScanVMs 扫描虚拟机（默认实现，子类覆盖）
func (b *BaseDriver) ScanVMs() (*model.ZMessage, error) {
	return &model.ZMessage{Success: true, Action: "VMDetect", Message: "扫描完成"}, nil
}

// CreateVM 创建虚拟机（默认实现，子类覆盖）
func (b *BaseDriver) CreateVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.VmSaving[cfg.VmUUID] = cfg
	return &model.ZMessage{Success: true, Action: "VMCreate", Message: "虚拟机创建成功"}, nil
}

// ReinstallVM 重装虚拟机系统（默认实现，子类覆盖）
func (b *BaseDriver) ReinstallVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "VMReinstall", Message: "当前平台不支持系统重装"}, nil
}

// UpdateVM 更新虚拟机（默认实现，子类覆盖）
func (b *BaseDriver) UpdateVM(cfg *model.VMConfig, oldCfg *model.VMConfig) (*model.ZMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.VmSaving[cfg.VmUUID] = cfg
	return &model.ZMessage{Success: true, Action: "VMUpdate", Message: "虚拟机配置已更新"}, nil
}

// DeleteVM 删除虚拟机（默认实现，子类覆盖）
func (b *BaseDriver) DeleteVM(vmUUID string, removeBackup bool) (*model.ZMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.VmSaving, vmUUID)
	return &model.ZMessage{Success: true, Action: "VMDelete", Message: "虚拟机已删除"}, nil
}

// PowerControl 电源控制（默认实现，子类覆盖）
func (b *BaseDriver) PowerControl(vmUUID string, action model.VMPowers) (*model.ZMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cfg, ok := b.VmSaving[vmUUID]
	if !ok {
		return &model.ZMessage{Success: false, Action: "VMPowers", Message: "虚拟机不存在"}, nil
	}
	if newState, ok := PowerActionMap[action]; ok {
		cfg.VmFlag = newState
	}
	return &model.ZMessage{Success: true, Action: "VMPowers", Message: "电源操作已执行"}, nil
}

// GetVNCInfo 获取VNC信息（默认实现，子类覆盖）
func (b *BaseDriver) GetVNCInfo(vmUUID string, ipAddr string) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "VCRemote", Message: "当前平台不支持VNC"}, nil
}

// Crontab 定时任务（默认实现）
func (b *BaseDriver) Crontab() bool {
	return true
}

// ============================================================
// 辅助函数
// ============================================================

// RandomVNCPort 生成随机VNC端口 5900-6999
func RandomVNCPort() string {
	return fmt.Sprintf("%d", 5900+rand.Intn(1100))
}

// MarshalVMConfig 序列化VMConfig为JSON字符串
func MarshalVMConfig(cfg *model.VMConfig) (string, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("序列化VMConfig失败: %w", err)
	}
	return string(data), nil
}

func init() {
	rand.Seed(time.Now().UnixNano())
}
