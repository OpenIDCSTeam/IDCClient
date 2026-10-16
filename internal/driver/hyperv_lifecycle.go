// Package driver Hyper-V 驱动生命周期实现（PowerShell，仅 Windows）
// 与 Python 版 HostServer/Win64HyperV.py 的功能语义对齐。
package driver

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/security"
)

// execPS 执行 PowerShell 命令
func (d *HyperVDriver) execPS(script string) (string, error) {
	out, _, err := security.SafeExecWithTimeout(
		[]string{"powershell", "-NoProfile", "-NonInteractive", "-Command", script},
		2*time.Minute)
	return out, err
}

// hvDiskDir 返回 Hyper-V 磁盘目录
func (d *HyperVDriver) hvDiskDir() string {
	if d.Config.SystemPath != "" {
		return d.Config.SystemPath
	}
	return "C:\\Hyper-V\\Disks"
}

// GetStatus 获取 Hyper-V 主机状态。
//
// 与 Python 版 Win64HyperV.HSStatus 一致：
//  1. 先用本机采集拿到完整基础数据（磁盘、网络等）
//  2. 再通过 Hyper-V 向宿主机查询 CPU / 内存总量并覆盖（虚拟化层视角更准确）
//
// 原实现直接走基类的本机采集，缺少第 2 步的 Hyper-V 覆盖。
// 查询失败时保留本机数据，不阻断状态上报。
func (d *HyperVDriver) GetStatus() (*model.HWStatus, error) {
	hw := localHWStatus()

	// 内存单位：MemoryCapacity 为字节；CPU 取逻辑处理器数
	script := `$ErrorActionPreference='SilentlyContinue'; ` +
		`try { $h = Get-VMHost -ErrorAction Stop; ` +
		`[pscustomobject]@{ ` +
		`mem_total=[math]::Round($h.MemoryCapacity/1MB); ` +
		`cpu_total=$h.LogicalProcessorCount ` +
		`} | ConvertTo-Json -Compress } catch { '' }`

	out, err := d.execPS(script)
	if err != nil {
		return hw, nil
	}
	body := strings.TrimSpace(out)
	if body == "" {
		return hw, nil
	}
	var parsed struct {
		MemTotal int `json:"mem_total"`
		CpuTotal int `json:"cpu_total"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return hw, nil
	}
	// 仅在查询到有效值时覆盖（与 Python 一致：连接失败则用本机数据）
	if parsed.MemTotal > 0 {
		hw.MemTotal = parsed.MemTotal
		if hw.MemUsage > hw.MemTotal {
			hw.MemUsage = 0
		}
	}
	if parsed.CpuTotal > 0 {
		hw.CpuTotal = parsed.CpuTotal
	}
	return hw, nil
}

// ScanVMs 扫描并同步虚拟机列表
//
// 与 Python 版 Win64HyperV.VMDetect 对齐：使用主机配置的 filter_name
// 作为名称前缀过滤，只纳管带该前缀的虚拟机。
func (d *HyperVDriver) ScanVMs() (*model.ZMessage, error) {
	vms, err := d.GetVMList()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMDetect", Message: err.Error()}, nil
	}

	// 前缀过滤（未配置 filter_name 时不过滤）
	prefix := d.Config.FilterName
	d.mu.Lock()
	scanned := 0
	added := 0
	for id, cfg := range vms {
		if prefix != "" && (cfg == nil || !strings.HasPrefix(cfg.OsName, prefix)) {
			continue
		}
		scanned++
		if _, exists := d.VmSaving[id]; !exists {
			d.VmSaving[id] = cfg
			added++
		} else {
			d.VmSaving[id].VmFlag = cfg.VmFlag
		}
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDetect",
		Message: fmt.Sprintf("扫描完成：匹配 %d 个，新增 %d 个", scanned, added)}, nil
}

// PowerControl 电源控制
func (d *HyperVDriver) PowerControl(vmUUID string, action model.VMPowers) (*model.ZMessage, error) {
	var script string
	switch action {
	case model.VMPowerSStart:
		script = fmt.Sprintf("Start-VM -Id '%s'", psEscape(vmUUID))
	case model.VMPowerSClose:
		script = fmt.Sprintf("Stop-VM -Id '%s'", psEscape(vmUUID))
	case model.VMPowerHClose:
		script = fmt.Sprintf("Stop-VM -Id '%s' -Force -TurnOff", psEscape(vmUUID))
	case model.VMPowerSReset, model.VMPowerHReset:
		script = fmt.Sprintf("Restart-VM -Id '%s' -Force", psEscape(vmUUID))
	case model.VMPowerAPause:
		script = fmt.Sprintf("Suspend-VM -Id '%s'", psEscape(vmUUID))
	case model.VMPowerAWaked:
		script = fmt.Sprintf("Resume-VM -Id '%s'", psEscape(vmUUID))
	default:
		return d.BaseDriver.PowerControl(vmUUID, action)
	}

	if out, err := d.execPS(script); err != nil {
		return &model.ZMessage{Success: false, Action: "VMPowers",
			Message: fmt.Sprintf("电源操作失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	if state, ok := PowerStatusMap[d.GetPower(vmUUID)]; ok {
		d.mu.Lock()
		if cfg, exists := d.VmSaving[vmUUID]; exists {
			cfg.VmFlag = state
		}
		d.mu.Unlock()
	}
	return &model.ZMessage{Success: true, Action: "VMPowers", Message: "电源操作已执行"}, nil
}

// CreateVM 创建虚拟机
func (d *HyperVDriver) CreateVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	vmName := cfg.VmUUID
	mem := cfg.MemNum
	if mem <= 0 {
		mem = 1024
	}
	cpu := cfg.CpuNum
	if cpu <= 0 {
		cpu = 1
	}
	diskPath := filepath.Join(d.hvDiskDir(), vmName+".vhdx")
	sizeMB := cfg.HddNum
	if sizeMB <= 0 {
		sizeMB = 8192
	}
	sizeBytes := int64(sizeMB) * 1024 * 1024

	script := fmt.Sprintf(
		"New-VM -Name '%s' -MemoryStartupBytes %dMB -Generation 2 -ErrorAction Stop; "+
			"New-VHD -Path '%s' -SizeBytes %d -Dynamic -ErrorAction Stop; "+
			"Add-VMHardDiskDrive -VMName '%s' -Path '%s' -ErrorAction Stop; "+
			"Set-VMProcessor -VMName '%s' -Count %d; "+
			"Start-VM -Name '%s'",
		psEscape(vmName), mem, psEscape(diskPath), sizeBytes,
		psEscape(vmName), psEscape(diskPath), psEscape(vmName), cpu, psEscape(vmName))

	if out, err := d.execPS(script); err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: fmt.Sprintf("创建虚拟机失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	cfg.VmFlag = model.VMPowerStarted
	d.mu.Lock()
	d.VmSaving[vmName] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMCreate", Message: "虚拟机创建成功"}, nil
}

// DeleteVM 删除虚拟机
func (d *HyperVDriver) DeleteVM(vmUUID string, removeBackup bool) (*model.ZMessage, error) {
	script := fmt.Sprintf(
		"Stop-VM -Id '%s' -Force -TurnOff -ErrorAction SilentlyContinue; "+
			"Remove-VM -Id '%s' -Force -ErrorAction Stop",
		psEscape(vmUUID), psEscape(vmUUID))

	if out, err := d.execPS(script); err != nil {
		return &model.ZMessage{Success: false, Action: "VMDelete",
			Message: fmt.Sprintf("删除失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	delete(d.VmSaving, vmUUID)
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDelete", Message: "虚拟机删除成功"}, nil
}

// UpdateVM 更新虚拟机配置（CPU / 内存）
func (d *HyperVDriver) UpdateVM(cfg *model.VMConfig, oldCfg *model.VMConfig) (*model.ZMessage, error) {
	if d.GetPower(cfg.VmUUID) == "运行中" {
		return &model.ZMessage{Success: false, Action: "VMUpdate",
			Message: "虚拟机必须关机后才能修改配置"}, nil
	}

	var parts []string
	if cfg.CpuNum != oldCfg.CpuNum && cfg.CpuNum > 0 {
		parts = append(parts, fmt.Sprintf(
			"Set-VMProcessor -VMName '%s' -Count %d -ErrorAction Stop", psEscape(cfg.VmUUID), cfg.CpuNum))
	}
	if cfg.MemNum != oldCfg.MemNum && cfg.MemNum > 0 {
		parts = append(parts, fmt.Sprintf(
			"Set-VMMemory -VMName '%s' -StartupBytes %dMB -ErrorAction Stop", psEscape(cfg.VmUUID), cfg.MemNum))
	}
	if len(parts) > 0 {
		if out, err := d.execPS(strings.Join(parts, "; ")); err != nil {
			return &model.ZMessage{Success: false, Action: "VMUpdate",
				Message: fmt.Sprintf("更新失败: %v (%s)", err, strings.TrimSpace(out))}, nil
		}
	}

	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMUpdate", Message: "虚拟机配置更新成功"}, nil
}

// ReinstallVM 重装系统（断开旧盘、创建新盘并启动）
func (d *HyperVDriver) ReinstallVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	vmName := cfg.VmUUID
	_, _ = d.PowerControl(vmName, model.VMPowerHClose)

	diskPath := filepath.Join(d.hvDiskDir(), vmName+".vhdx")
	sizeMB := cfg.HddNum
	if sizeMB <= 0 {
		sizeMB = 8192
	}
	sizeBytes := int64(sizeMB) * 1024 * 1024

	script := fmt.Sprintf(
		"Get-VMHardDiskDrive -VMName '%s' | Remove-VMHardDiskDrive -ErrorAction SilentlyContinue; "+
			"Remove-Item -Path '%s' -Force -ErrorAction SilentlyContinue; "+
			"New-VHD -Path '%s' -SizeBytes %d -Dynamic -ErrorAction Stop; "+
			"Add-VMHardDiskDrive -VMName '%s' -Path '%s' -ErrorAction Stop; "+
			"Start-VM -Name '%s'",
		psEscape(vmName), psEscape(diskPath), psEscape(diskPath), sizeBytes,
		psEscape(vmName), psEscape(diskPath), psEscape(vmName))

	if out, err := d.execPS(script); err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall",
			Message: fmt.Sprintf("重装失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}
	return &model.ZMessage{Success: true, Action: "VMReinstall", Message: "系统重装成功"}, nil
}

// GetVNCInfo 获取远程控制台信息（Hyper-V 使用 VMConnect）
func (d *HyperVDriver) GetVNCInfo(vmUUID string, ipAddr string) (*model.ZMessage, error) {
	host := d.Config.ServerAddr
	if host == "" {
		host = "127.0.0.1"
	}
	return &model.ZMessage{Success: true, Action: "VCRemote", Message: "远程控制台信息获取成功",
		Results: map[string]interface{}{
			"type": "vmconnect", "host": host, "vm_uuid": vmUUID, "password": "",
		}}, nil
}

// MountHDD 挂载 / 卸载数据盘
func (d *HyperVDriver) MountHDD(vmUUID string, hdd *model.SDConfig, mount bool) (*model.ZMessage, error) {
	if !mount {
		script := fmt.Sprintf(
			"Get-VMHardDiskDrive -VMName '%s' | Where-Object { $_.Path -like '*%s*' } | Remove-VMHardDiskDrive",
			psEscape(vmUUID), psEscape(hdd.HddName))
		_, _ = d.execPS(script)
		d.mu.Lock()
		if cfg, ok := d.VmSaving[vmUUID]; ok {
			if h, exists := cfg.HddAll[hdd.HddName]; exists {
				h.HddFlag = 0
				cfg.HddAll[hdd.HddName] = h
			}
		}
		d.mu.Unlock()
		return &model.ZMessage{Success: true, Action: "HDDMount", Message: "磁盘卸载成功"}, nil
	}

	path := filepath.Join(d.hvDiskDir(), hdd.HddName+".vhdx")
	sizeMB := hdd.HddSize
	if sizeMB <= 0 {
		sizeMB = 10240
	}
	sizeBytes := int64(sizeMB) * 1024 * 1024

	script := fmt.Sprintf(
		"New-VHD -Path '%s' -SizeBytes %d -Dynamic -ErrorAction SilentlyContinue; "+
			"Add-VMHardDiskDrive -VMName '%s' -Path '%s' -ErrorAction Stop",
		psEscape(path), sizeBytes, psEscape(vmUUID), psEscape(path))
	if out, err := d.execPS(script); err != nil {
		return &model.ZMessage{Success: false, Action: "HDDMount",
			Message: fmt.Sprintf("挂载失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	hdd.HddFlag = 1
	hdd.HddPath = path
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		cfg.HddAll[hdd.HddName] = *hdd
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "HDDMount", Message: "磁盘挂载成功"}, nil
}

// MountISO 挂载 / 卸载 ISO
func (d *HyperVDriver) MountISO(vmUUID string, iso *model.IMConfig, mount bool) (*model.ZMessage, error) {
	if mount {
		path := iso.IsoPath
		if path == "" {
			dir := d.Config.DvdromPath
			if dir == "" {
				dir = d.Config.ImagesPath
			}
			path = filepath.Join(dir, iso.IsoFile)
		}
		script := fmt.Sprintf("Add-VMDvdDrive -VMName '%s' -Path '%s' -ErrorAction Stop",
			psEscape(vmUUID), psEscape(path))
		if out, err := d.execPS(script); err != nil {
			return &model.ZMessage{Success: false, Action: "ISOMount",
				Message: fmt.Sprintf("挂载ISO失败: %v (%s)", err, strings.TrimSpace(out))}, nil
		}
		d.mu.Lock()
		if cfg, ok := d.VmSaving[vmUUID]; ok {
			cfg.IsoAll[iso.IsoName] = *iso
		}
		d.mu.Unlock()
		return &model.ZMessage{Success: true, Action: "ISOMount", Message: "ISO挂载成功"}, nil
	}

	script := fmt.Sprintf(
		"Get-VMDvdDrive -VMName '%s' | Remove-VMDvdDrive -ErrorAction SilentlyContinue", psEscape(vmUUID))
	_, _ = d.execPS(script)
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		delete(cfg.IsoAll, iso.IsoName)
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "ISOMount", Message: "ISO卸载成功"}, nil
}

// CreateBackup 创建备份（检查点）
func (d *HyperVDriver) CreateBackup(vmUUID string, tips string) (*model.ZMessage, error) {
	name := fmt.Sprintf("backup-%s", time.Now().Format("20060102-150405"))
	script := fmt.Sprintf("Checkpoint-VM -VMName '%s' -SnapshotName '%s' -ErrorAction Stop",
		psEscape(vmUUID), psEscape(name))
	if out, err := d.execPS(script); err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: fmt.Sprintf("创建检查点失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		cfg.Backups = append(cfg.Backups, model.VMBackup{
			BackupName: name, BackupTime: time.Now(), BackupTips: tips,
		})
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMBackup", Message: "备份创建成功",
		Results: map[string]interface{}{"backup_name": name}}, nil
}

// RestoreBackup 还原备份（应用检查点）
func (d *HyperVDriver) RestoreBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	script := fmt.Sprintf(
		"Restore-VMSnapshot -VMName '%s' -Name '%s' -Confirm:$false -ErrorAction Stop",
		psEscape(vmUUID), psEscape(backupName))
	if out, err := d.execPS(script); err != nil {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: fmt.Sprintf("还原失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}
	return &model.ZMessage{Success: true, Action: "Restores", Message: "备份还原成功"}, nil
}

// DeleteBackup 删除备份（删除检查点）
func (d *HyperVDriver) DeleteBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	script := fmt.Sprintf(
		"Remove-VMSnapshot -VMName '%s' -Name '%s' -Confirm:$false -ErrorAction Stop",
		psEscape(vmUUID), psEscape(backupName))
	if out, err := d.execPS(script); err != nil {
		return &model.ZMessage{Success: false, Action: "RMBackup",
			Message: fmt.Sprintf("删除备份失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		kept := cfg.Backups[:0]
		for _, b := range cfg.Backups {
			if b.BackupName != backupName {
				kept = append(kept, b)
			}
		}
		cfg.Backups = kept
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "RMBackup", Message: "备份删除成功"}, nil
}

// SetupPCI PCI/GPU 直通（需关机）
// 与 Python 版 Win64HyperV.PCISetup 一致：
//   - PV（GPU 分区虚拟化）：Add-VMGpuPartitionAdapter + 分区资源设置
//   - DDA（离散设备分配）：Disable-PnpDevice + Dismount-VMHostAssignableDevice + Add-VMAssignableDevice
func (d *HyperVDriver) SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error) {
	if d.GetPower(vmUUID) == "运行中" {
		return &model.ZMessage{Success: false, Action: "PCISetup",
			Message: "PCI直通需要先关闭虚拟机"}, nil
	}

	deviceType := cfg.GpuMdev // "PV" 或 "DDA"
	var script string

	if add {
		switch deviceType {
		case "PV":
			script = fmt.Sprintf(
				"Add-VMGpuPartitionAdapter -VMName '%s' -ErrorAction Stop; "+
					"Set-VMGpuPartitionAdapter -VMName '%s' "+
					"-MinPartitionVRAM 1000000000 -MaxPartitionVRAM 1000000000 -OptimalPartitionVRAM 1000000000",
				psEscape(vmUUID), psEscape(vmUUID))
		case "DDA":
			script = fmt.Sprintf(
				"$lp = '%s'; "+
					"$dev = Get-PnpDevice | Where-Object { (Get-PnpDeviceProperty -InstanceId $_.InstanceId -KeyName DEVPKEY_Device_LocationPaths).Data -contains $lp }; "+
					"if ($dev) { "+
					"Disable-PnpDevice -InstanceId $dev.InstanceId -Confirm:$false -ErrorAction SilentlyContinue; "+
					"Dismount-VMHostAssignableDevice -LocationPath $lp -Force -ErrorAction Stop; "+
					"Add-VMAssignableDevice -VMName '%s' -LocationPath $lp -ErrorAction Stop "+
					"} else { Write-Error '未找到DDA设备' }",
				psEscape(cfg.GpuUUID), psEscape(vmUUID))
		default:
			return &model.ZMessage{Success: false, Action: "PCISetup",
				Message: fmt.Sprintf("未知设备类型: %s", deviceType)}, nil
		}
	} else {
		switch deviceType {
		case "PV":
			script = fmt.Sprintf(
				"Remove-VMGpuPartitionAdapter -VMName '%s' -ErrorAction Stop", psEscape(vmUUID))
		case "DDA":
			script = fmt.Sprintf(
				"Remove-VMAssignableDevice -VMName '%s' -LocationPath '%s' -ErrorAction Stop",
				psEscape(vmUUID), psEscape(cfg.GpuUUID))
		default:
			script = fmt.Sprintf(
				"Remove-VMAssignableDevice -VMName '%s' -LocationPath '%s' -ErrorAction SilentlyContinue",
				psEscape(vmUUID), psEscape(cfg.GpuUUID))
		}
	}

	if out, err := d.execPS(script); err != nil {
		return &model.ZMessage{Success: false, Action: "PCISetup",
			Message: fmt.Sprintf("PCI直通失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	if vmCfg, ok := d.VmSaving[vmUUID]; ok {
		if vmCfg.PciAll == nil {
			vmCfg.PciAll = make(map[string]model.VFConfig)
		}
		if add {
			vmCfg.PciAll[pciKey] = *cfg
		} else {
			delete(vmCfg.PciAll, pciKey)
		}
	}
	d.mu.Unlock()

	text := "添加"
	if !add {
		text = "移除"
	}
	return &model.ZMessage{Success: true, Action: "PCISetup", Message: "PCI设备" + text + "成功"}, nil
}

// SetPassword 设置虚拟机密码（PowerShell Direct + Set-LocalUser）
func (d *HyperVDriver) SetPassword(vmUUID string, password string) (*model.ZMessage, error) {
	if d.GetPower(vmUUID) != "运行中" {
		return &model.ZMessage{Success: false, Action: "VMPasswd",
			Message: "虚拟机未运行，无法设置密码"}, nil
	}

	user := "Administrator"
	d.mu.RLock()
	if cfg, ok := d.VmSaving[vmUUID]; ok && cfg.OsUser != "" {
		user = cfg.OsUser
	}
	d.mu.RUnlock()

	// 用户名与口令必须转义：两者都会拼入 -Command 脚本文本，
	// 未转义的引号可以提前闭合字面量并执行附加语句。
	script := fmt.Sprintf(
		"Invoke-Command -VMName '%s' -ScriptBlock { param($u,$p) "+
			"$s = ConvertTo-SecureString $p -AsPlainText -Force; "+
			"Get-LocalUser -Name $u | Set-LocalUser -Password $s } "+
			"-ArgumentList '%s','%s'",
		psEscape(vmUUID), psEscape(user), psEscape(password))

	if out, err := d.execPS(script); err != nil {
		return &model.ZMessage{Success: false, Action: "VMPasswd",
			Message: fmt.Sprintf("密码修改失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		cfg.OsPass = password
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "VMPasswd", Message: "密码修改成功"}, nil
}

// Crontab 定时任务：同步电源状态
func (d *HyperVDriver) Crontab() bool {
	d.mu.RLock()
	ids := make([]string, 0, len(d.VmSaving))
	for id := range d.VmSaving {
		ids = append(ids, id)
	}
	d.mu.RUnlock()

	for _, id := range ids {
		state := d.GetPower(id)
		d.mu.Lock()
		if cfg, ok := d.VmSaving[id]; ok {
			if s, ok2 := PowerStatusMap[state]; ok2 {
				cfg.VmFlag = s
			}
		}
		d.mu.Unlock()
	}
	return true
}
