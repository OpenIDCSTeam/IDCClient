// Package driver MEmu Android 驱动生命周期实现（memuc 命令行）。
//
// 与 Python 版 HostServer/MemuAndroid.py 严格对齐，重点修正两处语义错误：
//
//  1. 虚拟机标识必须是 "memu_<index>" 形式（Python 统一约定），
//     调用 memuc 时再剥离前缀取纯数字索引；
//     原实现直接把裸 index 当 VmUUID，导致与历史数据/前端不一致。
//  2. 创建实例后必须回读 memuc 返回的索引并重命名，
//     原实现用「遍历 map 取最后一项」来定位索引，顺序随机、几乎必然错位。
package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/security"
)

// memuKeyPrefix 虚拟机标识前缀（与 Python 版 f"memu_{index}" 一致）
const memuKeyPrefix = "memu_"

// memucBin 解析 memuc 可执行文件路径（优先取 launch_path 配置，与 Python 一致）
func (d *MemuDriver) memucBin() string {
	launch := strings.TrimSpace(d.Config.LaunchPath)
	if launch != "" {
		if info, err := os.Stat(launch); err == nil {
			if info.IsDir() {
				for _, name := range []string{"memuc.exe", "memuc"} {
					candidate := filepath.Join(launch, name)
					if _, err := os.Stat(candidate); err == nil {
						return candidate
					}
				}
			} else {
				return launch
			}
		}
	}
	return "memuc"
}

// execMemuc 执行 memuc 命令
func (d *MemuDriver) execMemuc(args ...string) (string, error) {
	full := append([]string{d.memucBin()}, args...)
	out, _, err := security.SafeExecWithTimeout(full, 2*time.Minute)
	return out, err
}

// extractIndex 从 vm_uuid（memu_N 或纯数字 N）提取纯数字索引
func extractMemuIndex(vmUUID string) string {
	if strings.HasPrefix(vmUUID, memuKeyPrefix) {
		return vmUUID[len(memuKeyPrefix):]
	}
	return vmUUID
}

// vmIndex 返回某虚拟机对应的 memuc 索引
func (d *MemuDriver) vmIndex(vmUUID string) string {
	return extractMemuIndex(vmUUID)
}

// adb 执行 ADB 命令（memuc adb -i <index> -- <args>）
func (d *MemuDriver) adb(vmIndex string, args ...string) (string, error) {
	full := append([]string{"adb", "-i", vmIndex, "--"}, args...)
	return d.execMemuc(full...)
}

// memuInstance memuc listvms 条目
type memuInstance struct {
	Index string
	Title string
}

// listInstances 解析 memuc listvms 输出
// 格式: index,title,top-level window handle,attach window handle,pid,disk usage
func (d *MemuDriver) listInstances() []memuInstance {
	var result []memuInstance
	out, err := d.execMemuc("listvms")
	if err != nil {
		return result
	}
	for _, line := range splitLines(out) {
		if strings.HasPrefix(line, "index") {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 2 {
			continue
		}
		result = append(result, memuInstance{
			Index: strings.TrimSpace(fields[0]),
			Title: strings.TrimSpace(fields[1]),
		})
	}
	return result
}

// GetPower 通过 memuc isvmrunning 获取模拟器状态
func (d *MemuDriver) GetPower(vmUUID string) string {
	idx := d.vmIndex(vmUUID)
	if idx == "" {
		return "未知"
	}
	out, err := d.execMemuc("isvmrunning", "-i", idx)
	if err != nil {
		return "未知"
	}
	text := strings.ToLower(strings.TrimSpace(out))
	switch {
	case strings.Contains(text, "not running"):
		return "已关机"
	case strings.Contains(text, "running"):
		return "运行中"
	default:
		return "未知"
	}
}

// memuPowerFlag 将 MEmu 状态文本转为枚举
func memuPowerFlag(status string) model.VMPowers {
	switch status {
	case "运行中", "Running", "running":
		return model.VMPowerStarted
	case "已关机", "已关闭", "Stopped", "stopped":
		return model.VMPowerStopped
	default:
		return model.VMPowerUnknown
	}
}

// GetVMList 获取 Android 实例列表（键为 memu_<index>）
func (d *MemuDriver) GetVMList() (map[string]*model.VMConfig, error) {
	result := d.GetVMSaving()
	seen := make(map[string]bool)

	for _, inst := range d.listInstances() {
		if inst.Index == "" {
			continue
		}
		key := memuKeyPrefix + inst.Index
		seen[key] = true
		flag := memuPowerFlag(d.GetPower(key))

		if cfg, ok := result[key]; ok {
			cfg.VmFlag = flag
			if inst.Title != "" {
				cfg.OsName = inst.Title
			}
		} else {
			cfg := model.NewVMConfig()
			cfg.VmUUID = key
			cfg.OsName = inst.Title
			cfg.VmFlag = flag
			result[key] = cfg
		}
	}
	_ = seen
	return result, nil
}

// ScanVMs 扫描并同步实例列表（按 filter_name 前缀过滤标题）
func (d *MemuDriver) ScanVMs() (*model.ZMessage, error) {
	instances := d.listInstances()
	prefix := d.Config.FilterName

	d.mu.Lock()
	added := 0
	for _, inst := range instances {
		if inst.Index == "" {
			continue
		}
		if prefix != "" && !strings.HasPrefix(inst.Title, prefix) {
			continue
		}
		key := memuKeyPrefix + inst.Index
		flag := memuPowerFlag(d.GetPower(key))
		if cfg, ok := d.VmSaving[key]; ok {
			cfg.VmFlag = flag
			if inst.Title != "" {
				cfg.OsName = inst.Title
			}
		} else {
			cfg := model.NewVMConfig()
			cfg.VmUUID = key
			cfg.OsName = inst.Title
			cfg.VmFlag = flag
			d.VmSaving[key] = cfg
			added++
		}
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDetect",
		Message: fmt.Sprintf("扫描完成：共 %d 个，新增 %d 个", len(instances), added)}, nil
}

// PowerControl 电源控制
func (d *MemuDriver) PowerControl(vmUUID string, action model.VMPowers) (*model.ZMessage, error) {
	idx := d.vmIndex(vmUUID)
	if idx == "" {
		return &model.ZMessage{Success: false, Action: "VMPowers", Message: "模拟器不存在"}, nil
	}

	var args []string
	switch action {
	case model.VMPowerSStart:
		args = []string{"start", "-i", idx, "-b"}
	case model.VMPowerSClose, model.VMPowerHClose:
		args = []string{"stop", "-i", idx}
	case model.VMPowerSReset, model.VMPowerHReset:
		args = []string{"reboot", "-i", idx}
	case model.VMPowerAPause:
		return &model.ZMessage{Success: false, Action: "VMPowers",
			Message: "逍遥模拟器不支持暂停操作"}, nil
	case model.VMPowerAWaked:
		return &model.ZMessage{Success: false, Action: "VMPowers",
			Message: "逍遥模拟器不支持恢复操作"}, nil
	default:
		return &model.ZMessage{Success: false, Action: "VMPowers",
			Message: fmt.Sprintf("不支持的电源操作: %s", action)}, nil
	}

	if out, err := d.execMemuc(args...); err != nil {
		return &model.ZMessage{Success: false, Action: "VMPowers",
			Message: fmt.Sprintf("电源操作失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	if cfg, exists := d.VmSaving[vmUUID]; exists {
		if state, ok := PowerStatusMap[d.GetPower(vmUUID)]; ok {
			cfg.VmFlag = state
		}
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "VMPowers", Message: "电源操作已执行"}, nil
}

// androidVersion 把系统名映射为 memuc 的安卓版本号（与 Python 版一致）
func androidVersion(osName string) string {
	key := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(osName), " ", ""))
	verMap := map[string]string{
		"android4.4": "44", "android5.1": "51",
		"android7.1": "71", "android7.1_64": "76",
		"44": "44", "51": "51", "71": "71", "76": "76",
	}
	if v, ok := verMap[key]; ok {
		return v
	}
	return "71"
}

// CreateVM 创建 Android 实例
func (d *MemuDriver) CreateVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	androidVer := androidVersion(cfg.OsName)

	out, err := d.execMemuc("create", androidVer)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: fmt.Sprintf("创建模拟器失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	// memuc create 返回新建实例的索引；不可解析时回读列表取最后一项
	newIndex := strings.TrimSpace(out)
	if _, convErr := strconv.Atoi(newIndex); convErr != nil {
		newIndex = ""
		if instances := d.listInstances(); len(instances) > 0 {
			newIndex = instances[len(instances)-1].Index
		}
	}
	if newIndex == "" {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: "无法获取新建模拟器索引"}, nil
	}

	key := memuKeyPrefix + newIndex
	cfg.VmUUID = key

	// CPU / 内存
	if cfg.CpuNum > 0 {
		_, _ = d.execMemuc("setconfigex", "-i", newIndex, "cpus", strconv.Itoa(cfg.CpuNum))
	}
	if cfg.MemNum > 0 {
		_, _ = d.execMemuc("setconfigex", "-i", newIndex, "memory", strconv.Itoa(cfg.MemNum))
	}
	// 扩展参数（与 Python 从 extend_data 读取一致）
	extend := cfg.ExtData
	if extend != nil {
		for _, k := range []string{"graphics_render_mode", "enable_su", "fps"} {
			if v, ok := extend[k]; ok {
				_, _ = d.execMemuc("setconfigex", "-i", newIndex, k, fmt.Sprintf("%v", v))
			}
		}
	}
	// 重命名（标题）
	if title := strings.TrimSpace(cfg.OsName); title != "" {
		_, _ = d.execMemuc("rename", "-i", newIndex, title)
	}
	// 启动
	_, _ = d.execMemuc("start", "-i", newIndex, "-b")

	cfg.VmFlag = model.VMPowerStarted
	d.mu.Lock()
	d.VmSaving[key] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMCreate", Message: "模拟器创建成功"}, nil
}

// DeleteVM 删除 Android 实例
func (d *MemuDriver) DeleteVM(vmUUID string, removeBackup bool) (*model.ZMessage, error) {
	idx := d.vmIndex(vmUUID)
	if idx == "" {
		return &model.ZMessage{Success: false, Action: "VMDelete", Message: "模拟器不存在"}, nil
	}

	_, _ = d.execMemuc("stop", "-i", idx)
	time.Sleep(2 * time.Second)

	if out, err := d.execMemuc("remove", "-i", idx); err != nil {
		return &model.ZMessage{Success: false, Action: "VMDelete",
			Message: fmt.Sprintf("删除失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	delete(d.VmSaving, vmUUID)
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDelete", Message: "模拟器已删除"}, nil
}

// UpdateVM 更新实例配置
func (d *MemuDriver) UpdateVM(cfg *model.VMConfig, oldCfg *model.VMConfig) (*model.ZMessage, error) {
	idx := d.vmIndex(cfg.VmUUID)
	if idx == "" {
		return &model.ZMessage{Success: false, Action: "VMUpdate", Message: "模拟器不存在"}, nil
	}

	_, _ = d.execMemuc("stop", "-i", idx)
	time.Sleep(2 * time.Second)

	if cfg.CpuNum != oldCfg.CpuNum && cfg.CpuNum > 0 {
		_, _ = d.execMemuc("setconfigex", "-i", idx, "cpus", strconv.Itoa(cfg.CpuNum))
	}
	if cfg.MemNum != oldCfg.MemNum && cfg.MemNum > 0 {
		_, _ = d.execMemuc("setconfigex", "-i", idx, "memory", strconv.Itoa(cfg.MemNum))
	}
	if title := strings.TrimSpace(cfg.OsName); title != "" && title != oldCfg.OsName {
		_, _ = d.execMemuc("rename", "-i", idx, title)
	}
	_, _ = d.execMemuc("start", "-i", idx, "-b")

	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMUpdate", Message: "模拟器配置更新成功"}, nil
}

// ReinstallVM 重装实例（删除后重建）
func (d *MemuDriver) ReinstallVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	idx := d.vmIndex(cfg.VmUUID)
	if idx != "" {
		_, _ = d.execMemuc("stop", "-i", idx)
		time.Sleep(2 * time.Second)
		if out, err := d.execMemuc("remove", "-i", idx); err != nil {
			return &model.ZMessage{Success: false, Action: "VMSetups",
				Message: fmt.Sprintf("删除旧实例失败: %v (%s)", err, strings.TrimSpace(out))}, nil
		}
		d.mu.Lock()
		delete(d.VmSaving, cfg.VmUUID)
		d.mu.Unlock()
	}
	return d.CreateVM(cfg)
}

// CreateBackup 创建备份（memuc export 导出 OVA）
func (d *MemuDriver) CreateBackup(vmUUID string, tips string) (*model.ZMessage, error) {
	idx := d.vmIndex(vmUUID)
	if idx == "" {
		return &model.ZMessage{Success: false, Action: "VMBackup", Message: "模拟器不存在"}, nil
	}

	backupDir := strings.TrimSpace(d.Config.BackupPath)
	if backupDir == "" {
		backupDir = "./DataSaving/backups"
	}
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: "创建备份目录失败: " + err.Error()}, nil
	}

	fileName := fmt.Sprintf("%s_%s.ova", vmUUID, time.Now().Format("20060102_150405"))
	backupFile := filepath.Join(backupDir, fileName)

	if out, err := d.execMemuc("export", "-i", idx, backupFile); err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: fmt.Sprintf("备份失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		cfg.Backups = append(cfg.Backups, model.VMBackup{
			BackupName: fileName, BackupTime: time.Now(), BackupTips: tips,
		})
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMBackup",
		Message: "备份成功: " + fileName,
		Results: map[string]interface{}{"backup_name": fileName}}, nil
}

// RestoreBackup 还原备份（memuc import 导入 OVA）
func (d *MemuDriver) RestoreBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	backupDir := strings.TrimSpace(d.Config.BackupPath)
	if backupDir == "" {
		backupDir = "./DataSaving/backups"
	}
	name, err := sanitizeFileName(backupName)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "Restores", Message: "备份名称无效"}, nil
	}
	backupFile := filepath.Join(backupDir, name)
	if _, statErr := os.Stat(backupFile); statErr != nil {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "备份文件不存在: " + name}, nil
	}

	if out, err := d.execMemuc("import", backupFile); err != nil {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: fmt.Sprintf("恢复失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}
	return &model.ZMessage{Success: true, Action: "Restores",
		Message: "恢复成功，请重新扫描模拟器"}, nil
}

// MountHDD 逍遥模拟器不支持挂载额外硬盘（与 Python 版一致）
func (d *MemuDriver) MountHDD(vmUUID string, hdd *model.SDConfig, mount bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "HDDMount",
		Message: "逍遥模拟器不支持挂载额外硬盘"}, nil
}

// MountISO 通过 ADB 安装 / 卸载 APK（替代 ISO 挂载，与 Python 版一致）
func (d *MemuDriver) MountISO(vmUUID string, iso *model.IMConfig, mount bool) (*model.ZMessage, error) {
	idx := d.vmIndex(vmUUID)
	if idx == "" {
		return &model.ZMessage{Success: false, Action: "ISOMount", Message: "模拟器不存在"}, nil
	}

	if mount {
		apkPath := iso.IsoPath
		if apkPath == "" {
			apkPath = filepath.Join(d.Config.DvdromPath, iso.IsoFile)
		}
		if _, err := os.Stat(apkPath); err != nil {
			return &model.ZMessage{Success: false, Action: "ISOMount",
				Message: "APK文件不存在: " + apkPath}, nil
		}
		if out, err := d.adb(idx, "install", "-r", apkPath); err != nil {
			return &model.ZMessage{Success: false, Action: "ISOMount",
				Message: fmt.Sprintf("APK安装失败: %v (%s)", err, strings.TrimSpace(out))}, nil
		}
		d.mu.Lock()
		if cfg, ok := d.VmSaving[vmUUID]; ok {
			if cfg.IsoAll == nil {
				cfg.IsoAll = make(map[string]model.IMConfig)
			}
			cfg.IsoAll[iso.IsoName] = *iso
		}
		d.mu.Unlock()
		return &model.ZMessage{Success: true, Action: "ISOMount", Message: "APK安装成功"}, nil
	}

	// 卸载：以 iso_name 作为包名
	pkg := iso.IsoName
	if out, err := d.adb(idx, "uninstall", pkg); err != nil {
		return &model.ZMessage{Success: false, Action: "ISOMount",
			Message: fmt.Sprintf("APK卸载失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		delete(cfg.IsoAll, iso.IsoName)
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "ISOMount", Message: "APK卸载成功"}, nil
}

// SetPassword 修改 Android 锁屏 PIN（fr ADB locksettings）
func (d *MemuDriver) SetPassword(vmUUID string, password string) (*model.ZMessage, error) {
	idx := d.vmIndex(vmUUID)
	if idx == "" {
		return &model.ZMessage{Success: false, Action: "VMPasswd", Message: "模拟器不存在"}, nil
	}

	if out, err := d.adb(idx, "shell", "locksettings", "set-pin", password); err != nil {
		return &model.ZMessage{Success: false, Action: "VMPasswd",
			Message: fmt.Sprintf("密码修改失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		cfg.OsPass = password
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "VMPasswd", Message: "锁屏密码修改成功"}, nil
}

// GetVNCInfo 获取 ADB 连接信息（逍遥模拟器通过 ADB 连接）
func (d *MemuDriver) GetVNCInfo(vmUUID string, ipAddr string) (*model.ZMessage, error) {
	idx := d.vmIndex(vmUUID)
	if idx == "" {
		return &model.ZMessage{Success: false, Action: "VNCGets", Message: "模拟器不存在"}, nil
	}

	host := d.Config.ServerAddr
	if host == "" {
		host = "127.0.0.1"
	}
	// 逍遥模拟器 ADB 端口规则：第 N 个 = 21503 + N*10
	port := 21503
	if n, err := strconv.Atoi(idx); err == nil {
		port = 21503 + n*10
	}

	return &model.ZMessage{Success: true, Action: "VNCGets", Message: "ADB连接信息获取成功",
		Results: map[string]interface{}{
			"host": host, "port": port, "type": "adb",
			"connect": fmt.Sprintf("adb connect %s:%d", host, port),
		}}, nil
}

// GetUSBList 列举已连接的 ADB 设备（与 Python 版 USBShows 一致）
func (d *MemuDriver) GetUSBList() (map[string]*model.USBInfos, error) {
	result := make(map[string]*model.USBInfos)
	out, err := d.execMemuc("adb", "--", "devices")
	if err != nil {
		return result, nil
	}
	for _, line := range splitLines(out) {
		if strings.HasPrefix(line, "List") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == "device" {
			serial := parts[0]
			result[serial] = &model.USBInfos{
				UsbKey:  serial,
				VidUUID: serial,
				UsbHint: "ADB设备: " + serial,
			}
		}
	}
	return result, nil
}

// GetPCIList 逍遥模拟器无 PCI 直通能力
func (d *MemuDriver) GetPCIList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// GetGPUList 逍遥模拟器无 GPU 直通能力
func (d *MemuDriver) GetGPUList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// SetupPCI 逍遥模拟器不支持 PCI 直通，明确拒绝
func (d *MemuDriver) SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "PCISetup",
		Message: "逍遥模拟器不支持 PCI 设备直通"}, nil
}

// SetupUSB 逍遥模拟器 USB 直通请通过 ADB 管理（与 Python 版实现一致）
func (d *MemuDriver) SetupUSB(vmUUID string, info *model.USBInfos, usbKey string, add bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "USBSetup",
		Message: "逍遥模拟器USB直通请通过ADB管理"}, nil
}

// MountUSB 逍遥模拟器不支持 USB 存储挂载
func (d *MemuDriver) MountUSB(vmUUID string, usbKey string, mount bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "USBMount",
		Message: "逍遥模拟器不支持 USB 存储挂载"}, nil
}

// GetScreenshot 通过 ADB 截取模拟器屏幕
func (d *MemuDriver) GetScreenshot(vmUUID string) (string, error) {
	if _, ok := d.VmSaving[vmUUID]; !ok {
		return "", fmt.Errorf("模拟器不存在")
	}
	return d.captureMemuScreenshot(d.vmIndex(vmUUID))
}

// Crontab 定时任务：同步实例状态
func (d *MemuDriver) Crontab() bool {
	for _, inst := range d.listInstances() {
		if inst.Index == "" {
			continue
		}
		key := memuKeyPrefix + inst.Index
		flag := memuPowerFlag(d.GetPower(key))
		d.mu.Lock()
		if cfg, ok := d.VmSaving[key]; ok {
			cfg.VmFlag = flag
		}
		d.mu.Unlock()
	}
	return true
}
