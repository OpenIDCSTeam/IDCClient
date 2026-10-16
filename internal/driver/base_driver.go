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
	"os"
	"path/filepath"
	"strings"
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
	"未知":  model.VMPowerUnknown,
	"":    model.VMPowerUnknown,
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
	self     HostDriver // 最外层驱动实例（用于模拟 Python 虚方法派发）
}

// NewBaseDriver 创建基础驱动
func NewBaseDriver(cfg *model.HSConfig) *BaseDriver {
	return &BaseDriver{
		Config:   cfg,
		VmSaving: make(map[string]*model.VMConfig),
	}
}

// SetSelf 记录最外层驱动实例（由 NewDriver 在构造后调用）
// 用于基类通用实现中派发到平台覆盖的方法，模拟 Python 的虚方法调用
func (b *BaseDriver) SetSelf(d HostDriver) {
	b.mu.Lock()
	b.self = d
	b.mu.Unlock()
}

// SelfDriver 返回最外层驱动实例（可能为 nil）
func (b *BaseDriver) SelfDriver() HostDriver {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.self
}

// GetConfig 获取主机配置
func (b *BaseDriver) GetConfig() *model.HSConfig {
	return b.Config
}

// CloneVMConfig 深复制虚拟机配置。
//
// 仅复制外层 map 是不安全的：调用方拿到内部指针后会在锁外修改
// VmFlag、NicAll、NatAll、OwnAll 等字段，与调度器采集并发时
// 会产生数据竞争、丢更新，甚至并发读写 map 的运行时错误。
func CloneVMConfig(src *model.VMConfig) *model.VMConfig {
	if src == nil {
		return nil
	}
	dst := *src

	if src.FluRst != nil {
		dst.FluRst = append([]int(nil), src.FluRst...)
	}
	if src.NicAll != nil {
		dst.NicAll = make(map[string]model.NCConfig, len(src.NicAll))
		for k, v := range src.NicAll {
			dst.NicAll[k] = v
		}
	}
	if src.HddAll != nil {
		dst.HddAll = make(map[string]model.SDConfig, len(src.HddAll))
		for k, v := range src.HddAll {
			dst.HddAll[k] = v
		}
	}
	if src.IsoAll != nil {
		dst.IsoAll = make(map[string]model.IMConfig, len(src.IsoAll))
		for k, v := range src.IsoAll {
			dst.IsoAll[k] = v
		}
	}
	if src.PciAll != nil {
		dst.PciAll = make(map[string]model.VFConfig, len(src.PciAll))
		for k, v := range src.PciAll {
			dst.PciAll[k] = v
		}
	}
	if src.UsbAll != nil {
		dst.UsbAll = make(map[string]model.USBInfos, len(src.UsbAll))
		for k, v := range src.UsbAll {
			dst.UsbAll[k] = v
		}
	}
	if src.OwnAll != nil {
		dst.OwnAll = make(map[string]model.UserMask, len(src.OwnAll))
		for k, v := range src.OwnAll {
			dst.OwnAll[k] = v
		}
	}
	if src.EfiAll != nil {
		dst.EfiAll = append([]model.BootOpts(nil), src.EfiAll...)
	}
	if src.NatAll != nil {
		dst.NatAll = append([]model.PortData(nil), src.NatAll...)
	}
	if src.WebAll != nil {
		dst.WebAll = append([]model.WebProxy(nil), src.WebAll...)
	}
	if src.Backups != nil {
		dst.Backups = append([]model.VMBackup(nil), src.Backups...)
	}
	if src.VmCmd != nil {
		dst.VmCmd = make(map[string]interface{}, len(src.VmCmd))
		for k, v := range src.VmCmd {
			dst.VmCmd[k] = v
		}
	}
	if src.VmCmdResult != nil {
		dst.VmCmdResult = make(map[string]interface{}, len(src.VmCmdResult))
		for k, v := range src.VmCmdResult {
			dst.VmCmdResult[k] = v
		}
	}
	if src.ExtData != nil {
		dst.ExtData = make(map[string]interface{}, len(src.ExtData))
		for k, v := range src.ExtData {
			dst.ExtData[k] = v
		}
	}
	return &dst
}

// GetVMSaving 获取虚拟机配置快照（返回深复制，可安全在锁外读写）
func (b *BaseDriver) GetVMSaving() map[string]*model.VMConfig {
	b.mu.RLock()
	defer b.mu.RUnlock()
	result := make(map[string]*model.VMConfig, len(b.VmSaving))
	for k, v := range b.VmSaving {
		result[k] = CloneVMConfig(v)
	}
	return result
}

// RemoveVMSaving 在锁内移除一条虚拟机内存记录。
//
// 只移除本机（HostAgent）侧的纳管记录，不做任何平台操作，
// 供「清理不匹配 filter_name 的历史记录」「仅移除记录」等场景使用。
func (b *BaseDriver) RemoveVMSaving(vmUUID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.VmSaving[vmUUID]; !ok {
		return false
	}
	delete(b.VmSaving, vmUUID)
	return true
}

// UpsertVMSaving 在锁内写入/替换单条虚拟机记录。
//
// 供「合并重复记录」等维护操作把合并结果回写到驱动内存，
// 避免后续列表接口用内存中的旧对象覆盖数据库里的合并结果。
func (b *BaseDriver) UpsertVMSaving(vmUUID string, cfg *model.VMConfig) {
	if vmUUID == "" || cfg == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.VmSaving[vmUUID] = cfg
}

// MatchFilterPrefix 判断一条虚拟机记录是否匹配主机配置的名称前缀（filter_name）。
//
// 前缀为空表示不过滤（与各驱动 ScanVMs 的语义一致）。
//
// 各平台存放名称的字段不同：ESXi/Hyper-V/Workstation 等写入 OsName，
// 而 Proxmox 驱动直接把名称（"-" 已替换为 "_"）当作 VmUUID，
// 因此这里对两个字段都做匹配，并把 "_" 与 "-" 视为等价，
// 避免清理历史记录时误删本应保留的虚拟机。
func MatchFilterPrefix(prefix string, cfg *model.VMConfig) bool {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return true
	}
	if cfg == nil {
		return false
	}
	normalize := func(s string) string { return strings.ReplaceAll(s, "_", "-") }
	target := normalize(prefix)
	for _, name := range []string{cfg.OsName, cfg.VmUUID} {
		if name == "" {
			continue
		}
		if strings.HasPrefix(normalize(name), target) {
			return true
		}
	}
	return false
}

// SetVMDeleted 在锁内更新虚拟机的扫描软删除标记。
//
// 扫描时平台侧已不存在的虚拟机只标记不删除，
// 保留所有者、端口映射等业务字段，平台侧重新出现时可恢复。
func (b *BaseDriver) SetVMDeleted(vmUUID string, deleted bool) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	cfg, ok := b.VmSaving[vmUUID]
	if !ok {
		return false
	}
	cfg.VmDeleted = deleted
	return true
}

// SetVMPowerState 在锁内更新虚拟机电源状态。
//
// 调度器只应通过该方法刷新状态，而不是修改快照对象：
// 快照是复制品，直接修改不会写回驱动，也会造成状态长期不更新。
func (b *BaseDriver) SetVMPowerState(vmUUID string, state model.VMPowers) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	cfg, ok := b.VmSaving[vmUUID]
	if !ok {
		return false
	}
	cfg.VmFlag = state
	return true
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

// GetVMDetail 获取虚拟机详情（返回深复制，调用方可自由修改后经 UpdateVM 写回）
func (b *BaseDriver) GetVMDetail(vmUUID string) (*model.VMConfig, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	cfg, ok := b.VmSaving[vmUUID]
	if !ok {
		return nil, fmt.Errorf("虚拟机 %s 不存在", vmUUID)
	}
	return CloneVMConfig(cfg), nil
}

// GetPower 获取虚拟机电源状态（默认返回未知，子类覆盖）
func (b *BaseDriver) GetPower(vmUUID string) string {
	return "未知"
}

// GetStatus 获取主机状态。
//
// 默认采集本机状态，与 Python 版 BasicServer.HSStatus() → local_get_hw_status() 一致。
// 原实现只返回时间戳，导致未覆盖该方法的平台（Hyper-V / QEMU / VirtualBox /
// MEmu / VMware Workstation 等）主机状态恒为全 0，前端显示不出任何数据。
//
// 远程平台（Proxmox / ESXi / 青州云）必须覆写本方法，使用平台 API 采集，
// 否则会把 IDCClient 本机资源误报为远程主机状态。
func (b *BaseDriver) GetStatus() (*model.HWStatus, error) {
	return localHWStatus(), nil
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

// SetupPCI PCI直通（默认实现：记录到配置，需关机）
// 与 Python 版 BasicServer.PCISetup 语义一致：校验关机状态后增删 pci_all
func (b *BaseDriver) SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	vmCfg, ok := b.VmSaving[vmUUID]
	if !ok {
		return &model.ZMessage{Success: false, Action: "PCISetup", Message: "虚拟机不存在"}, nil
	}
	// PCI 直通需要先关机
	switch vmCfg.VmFlag {
	case model.VMPowerStopped, model.VMPowerOnStop, model.VMPowerUnknown:
		// 允许
	default:
		return &model.ZMessage{Success: false, Action: "PCISetup", Message: "PCI直通需要先关闭虚拟机"}, nil
	}
	if vmCfg.PciAll == nil {
		vmCfg.PciAll = make(map[string]model.VFConfig)
	}

	if add {
		if _, exists := vmCfg.PciAll[pciKey]; exists {
			return &model.ZMessage{Success: false, Action: "PCISetup", Message: "PCI设备已存在"}, nil
		}
		vmCfg.PciAll[pciKey] = *cfg
	} else {
		if _, exists := vmCfg.PciAll[pciKey]; !exists {
			return &model.ZMessage{Success: false, Action: "PCISetup", Message: "PCI设备不存在"}, nil
		}
		delete(vmCfg.PciAll, pciKey)
	}

	action := "添加"
	if !add {
		action = "移除"
	}
	return &model.ZMessage{Success: true, Action: "PCISetup",
		Message: "PCI设备" + action + "成功"}, nil
}

// SetupUSB USB直通（默认实现：记录到配置，无需关机）
// 与 Python 版 BasicServer.USBSetup 语义一致：增删 usb_all
func (b *BaseDriver) SetupUSB(vmUUID string, info *model.USBInfos, usbKey string, add bool) (*model.ZMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	vmCfg, ok := b.VmSaving[vmUUID]
	if !ok {
		return &model.ZMessage{Success: false, Action: "USBMount", Message: "虚拟机不存在"}, nil
	}
	if vmCfg.UsbAll == nil {
		vmCfg.UsbAll = make(map[string]model.USBInfos)
	}

	if add {
		if _, exists := vmCfg.UsbAll[usbKey]; exists {
			return &model.ZMessage{Success: false, Action: "USBMount", Message: "USB设备KEY已存在"}, nil
		}
		vmCfg.UsbAll[usbKey] = *info
	} else {
		if _, exists := vmCfg.UsbAll[usbKey]; !exists {
			return &model.ZMessage{Success: false, Action: "USBMount", Message: "USB设备不存在"}, nil
		}
		delete(vmCfg.UsbAll, usbKey)
	}

	action := "挂载"
	if !add {
		action = "卸载"
	}
	return &model.ZMessage{Success: true, Action: "USBMount",
		Message: "USB设备" + action + "成功"}, nil
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

// hddFilePath 返回数据盘在宿主文件系统中的路径。
//
// 与 Python 版 BasicServer.RMMounts / HDDTrans 的命名约定一致：
//
//	<system_path>/<vm_uuid>/<vm_uuid>-<hdd_name>.vmdk
//
// 平台驱动若使用其它存储（qcow2 / vdi / 平台卷）应自行覆写 DeleteHDD / TransferHDD。
func (b *BaseDriver) hddFilePath(vmUUID, hddName string) string {
	return filepath.Join(b.Config.SystemPath, vmUUID, vmUUID+"-"+hddName+".vmdk")
}

// HDDCheck 数据盘移交前置校验。
//
// 与 Python 版 BasicServer.HDDCheck 完全一致：
//  1. 源虚拟机必须存在
//  2. 目标虚拟机必须存在
//  3. 待移交磁盘必须存在于源虚拟机
//  4. 磁盘必须处于「未挂载」状态（已挂载需先卸载）
func (b *BaseDriver) HDDCheck(vmUUID string, hdd *model.SDConfig, targetVM string) (*model.ZMessage, error) {
	if hdd == nil || strings.TrimSpace(hdd.HddName) == "" {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "磁盘信息不完整"}, nil
	}

	b.mu.RLock()
	defer b.mu.RUnlock()

	src, ok := b.VmSaving[vmUUID]
	if !ok {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "原始虚拟机不存在"}, nil
	}
	if _, ok := b.VmSaving[targetVM]; !ok {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "目标虚拟机不存在"}, nil
	}
	conf, ok := src.HddAll[hdd.HddName]
	if !ok {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "待移交磁盘不存在"}, nil
	}
	if conf.HddFlag == 1 {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "请先卸载此磁盘"}, nil
	}
	return &model.ZMessage{Success: true, Action: "HDDTrans", Message: "磁盘可以移交"}, nil
}

// DeleteHDD 删除数据盘（默认实现，与 Python 版 BasicServer.RMMounts 一致）。
//
// 步骤：已挂载则先卸载 → 从配置移除 → 删除宿主机上的真实磁盘文件。
// 平台驱动若使用非文件型存储（Proxmox 卷、云 API 磁盘）应覆写本方法。
func (b *BaseDriver) DeleteHDD(vmUUID string, hddName string) (*model.ZMessage, error) {
	if strings.TrimSpace(hddName) == "" {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: "磁盘名称不能为空"}, nil
	}

	b.mu.RLock()
	cfg, ok := b.VmSaving[vmUUID]
	var hdd model.SDConfig
	if ok {
		hdd = cfg.HddAll[hddName]
	}
	b.mu.RUnlock()

	if !ok {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: "虚拟机不存在"}, nil
	}
	if hdd.HddName == "" {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: "虚拟盘不存在"}, nil
	}

	// 已挂载则先卸载
	if hdd.HddFlag == 1 {
		self := b.SelfDriver()
		if self == nil {
			return &model.ZMessage{Success: false, Action: "RMMounts",
				Message: "驱动未初始化，无法卸载磁盘"}, nil
		}
		if msg, err := self.MountHDD(vmUUID, &hdd, false); err != nil {
			return &model.ZMessage{Success: false, Action: "RMMounts",
				Message: "卸载磁盘失败: " + err.Error()}, nil
		} else if msg != nil && !msg.Success {
			return &model.ZMessage{Success: false, Action: "RMMounts",
				Message: "卸载磁盘失败: " + msg.Message}, nil
		}
	}

	// 从配置移除
	b.mu.Lock()
	delete(cfg.HddAll, hddName)
	b.mu.Unlock()

	// 删除真实磁盘文件（仅文件型存储）
	path := hdd.HddPath
	if path == "" {
		path = b.hddFilePath(vmUUID, hdd.HddName)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return &model.ZMessage{Success: false, Action: "RMMounts",
			Message: "删除磁盘文件失败: " + err.Error()}, nil
	}

	return &model.ZMessage{Success: true, Action: "RMMounts", Message: "磁盘删除成功"}, nil
}

// TransferHDD 移交数据盘（默认实现，与 Python 版 BasicServer.HDDTrans 一致）。
//
// 步骤：HDDCheck 校验 → 把磁盘文件从源虚拟机的系统目录移动到目标虚拟机的系统目录
// → 源配置移除、目标配置新增（保持未挂载状态）。
// 平台驱动若使用非文件型存储应覆写本方法。
func (b *BaseDriver) TransferHDD(vmUUID string, hdd *model.SDConfig, targetVM string) (*model.ZMessage, error) {
	check, err := b.HDDCheck(vmUUID, hdd, targetVM)
	if err != nil || check == nil || !check.Success {
		return check, err
	}
	if vmUUID == targetVM {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "源与目标虚拟机相同"}, nil
	}

	oldPath := b.hddFilePath(vmUUID, hdd.HddName)
	newPath := b.hddFilePath(targetVM, hdd.HddName)

	// 移动物理文件（源文件不存在时仅迁移配置，与 Python 的告警行为一致）
	if _, statErr := os.Stat(oldPath); statErr == nil {
		if err := os.MkdirAll(filepath.Dir(newPath), 0755); err != nil {
			return &model.ZMessage{Success: false, Action: "HDDTrans",
				Message: "创建目标目录失败: " + err.Error()}, nil
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			return &model.ZMessage{Success: false, Action: "HDDTrans",
				Message: "移动磁盘文件失败: " + err.Error()}, nil
		}
	}

	b.mu.Lock()
	if src, ok := b.VmSaving[vmUUID]; ok {
		delete(src.HddAll, hdd.HddName)
	}
	if dst, ok := b.VmSaving[targetVM]; ok {
		if dst.HddAll == nil {
			dst.HddAll = make(map[string]model.SDConfig)
		}
		moved := *hdd
		moved.HddFlag = 0
		moved.HddPath = newPath
		dst.HddAll[hdd.HddName] = moved
	}
	b.mu.Unlock()

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
