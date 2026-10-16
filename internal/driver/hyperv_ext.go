// Package driver Hyper-V 驱动扩展实现。
//
// 补齐 Python 版 HostServer/Win64HyperV.py 中存在、而 Go 版此前缺失的能力：
//   - 数据盘删除 / 移交（使用 VHDX 路径）
//   - PCI / GPU 设备枚举（Get-PnpDevice，含 DDA 位置路径）
//   - USB 设备枚举
//   - USB 存储挂载（宿主物理磁盘直通）
//   - 屏幕截图（Hyper-V WMI 缩略图接口）
package driver

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// hvDataDiskPath 返回数据盘的 VHDX 路径（与 MountHDD 保持一致）
func (d *HyperVDriver) hvDataDiskPath(hdd *model.SDConfig) string {
	if hdd != nil && strings.TrimSpace(hdd.HddPath) != "" {
		return hdd.HddPath
	}
	name := ""
	if hdd != nil {
		name = hdd.HddName
	}
	return filepath.Join(d.hvDiskDir(), name+".vhdx")
}

// DeleteHDD 删除数据盘：移除虚拟磁盘挂载 → 删除 VHDX 文件
func (d *HyperVDriver) DeleteHDD(vmUUID string, hddName string) (*model.ZMessage, error) {
	if strings.TrimSpace(hddName) == "" {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: "磁盘名称不能为空"}, nil
	}

	d.mu.RLock()
	cfg, ok := d.VmSaving[vmUUID]
	var hdd model.SDConfig
	if ok {
		hdd = cfg.HddAll[hddName]
	}
	d.mu.RUnlock()
	if !ok {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: "虚拟机不存在"}, nil
	}
	if hdd.HddName == "" {
		hdd.HddName = hddName
	}

	path := d.hvDataDiskPath(&hdd)

	// 1) 移除挂在虚拟机上的该磁盘
	detach := fmt.Sprintf(
		"Get-VMHardDiskDrive -VMName '%s' | Where-Object { $_.Path -like '*%s*' } | "+
			"Remove-VMHardDiskDrive -ErrorAction SilentlyContinue",
		psEscape(vmUUID), psEscape(hdd.HddName))
	if out, err := d.execPS(detach); err != nil {
		return &model.ZMessage{Success: false, Action: "RMMounts",
			Message: fmt.Sprintf("卸载数据盘失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	// 2) 删除真实 VHDX 文件
	if path != "" {
		remove := fmt.Sprintf("Remove-Item -LiteralPath '%s' -Force -ErrorAction SilentlyContinue", psEscape(path))
		if out, err := d.execPS(remove); err != nil {
			return &model.ZMessage{Success: false, Action: "RMMounts",
				Message: fmt.Sprintf("删除磁盘文件失败: %v (%s)", err, strings.TrimSpace(out))}, nil
		}
	}

	d.mu.Lock()
	delete(cfg.HddAll, hddName)
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "RMMounts", Message: "数据盘删除成功"}, nil
}

// TransferHDD 移交数据盘：从源移除并挂到目标虚拟机
func (d *HyperVDriver) TransferHDD(vmUUID string, hdd *model.SDConfig, targetVM string) (*model.ZMessage, error) {
	if hdd == nil || strings.TrimSpace(hdd.HddName) == "" {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "磁盘信息不完整"}, nil
	}
	if vmUUID == targetVM {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "源与目标虚拟机相同"}, nil
	}

	d.mu.RLock()
	srcCfg, srcOK := d.VmSaving[vmUUID]
	dstCfg, dstOK := d.VmSaving[targetVM]
	d.mu.RUnlock()
	if !srcOK {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "原始虚拟机不存在"}, nil
	}
	if !dstOK {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "目标虚拟机不存在"}, nil
	}

	path := d.hvDataDiskPath(hdd)
	if path == "" {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "无法定位磁盘路径"}, nil
	}

	// 1) 源移除
	detach := fmt.Sprintf(
		"Get-VMHardDiskDrive -VMName '%s' | Where-Object { $_.Path -like '*%s*' } | "+
			"Remove-VMHardDiskDrive -ErrorAction SilentlyContinue",
		psEscape(vmUUID), psEscape(hdd.HddName))
	if out, err := d.execPS(detach); err != nil {
		return &model.ZMessage{Success: false, Action: "HDDTrans",
			Message: fmt.Sprintf("源虚拟机卸载失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	// 2) 目标挂载
	attach := fmt.Sprintf("Add-VMHardDiskDrive -VMName '%s' -Path '%s' -ErrorAction Stop",
		psEscape(targetVM), psEscape(path))
	if out, err := d.execPS(attach); err != nil {
		// 回滚
		rollback := fmt.Sprintf("Add-VMHardDiskDrive -VMName '%s' -Path '%s' -ErrorAction SilentlyContinue",
			psEscape(vmUUID), psEscape(path))
		_, _ = d.execPS(rollback)
		return &model.ZMessage{Success: false, Action: "HDDTrans",
			Message: fmt.Sprintf("目标虚拟机挂载失败（已回滚）: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	delete(srcCfg.HddAll, hdd.HddName)
	moved := *hdd
	moved.HddFlag = 1
	moved.HddPath = path
	if dstCfg.HddAll == nil {
		dstCfg.HddAll = make(map[string]model.SDConfig)
	}
	dstCfg.HddAll[hdd.HddName] = moved
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "HDDTrans", Message: "数据盘移交成功"}, nil
}

// GetPCIList 枚举可 DDA 直通的 PCI 设备（Get-PnpDevice + 位置路径）
func (d *HyperVDriver) GetPCIList() (map[string]*model.VFConfig, error) {
	result := make(map[string]*model.VFConfig)

	script := `$ErrorActionPreference='SilentlyContinue'; ` +
		`Get-PnpDevice | Where-Object { $_.InstanceId -like 'PCI\*' -and $_.Status -eq 'OK' } | ` +
		`ForEach-Object { ` +
		`$lp = (Get-PnpDeviceProperty -InstanceId $_.InstanceId -KeyName 'DEVPKEY_Device_LocationPaths').Data; ` +
		`if ($lp) { [pscustomobject]@{ id=$_.InstanceId; lp=$lp[0]; name=$_.FriendlyName } } ` +
		`} | ConvertTo-Json -Compress`

	out, err := d.execPS(script)
	if err != nil {
		return result, nil
	}
	items := parsePnpJSON(out)
	for _, it := range items {
		if it.LocationPath == "" {
			continue
		}
		result[it.LocationPath] = &model.VFConfig{
			PciKey:  it.LocationPath,
			GpuUUID: it.LocationPath,
			GpuMdev: "DDA",
			GpuHint: it.Name,
		}
	}
	return result, nil
}

// GetGPUList 枚举显卡设备（可用于 GPU-PV 或 DDA）
func (d *HyperVDriver) GetGPUList() (map[string]*model.VFConfig, error) {
	result := make(map[string]*model.VFConfig)

	script := `$ErrorActionPreference='SilentlyContinue'; ` +
		`Get-PnpDevice -Class Display | Where-Object { $_.Status -eq 'OK' } | ` +
		`ForEach-Object { ` +
		`$lp = (Get-PnpDeviceProperty -InstanceId $_.InstanceId -KeyName 'DEVPKEY_Device_LocationPaths').Data; ` +
		`[pscustomobject]@{ id=$_.InstanceId; lp=$(if($lp){$lp[0]}else{''}); name=$_.FriendlyName } ` +
		`} | ConvertTo-Json -Compress`

	out, err := d.execPS(script)
	if err != nil {
		return result, nil
	}
	for _, it := range parsePnpJSON(out) {
		key := it.LocationPath
		if key == "" {
			key = it.InstanceID
		}
		if key == "" {
			continue
		}
		result[key] = &model.VFConfig{
			PciKey:  key,
			GpuUUID: it.LocationPath,
			GpuMdev: "PV",
			GpuHint: it.Name,
		}
	}
	return result, nil
}

// pnpDevice PowerShell 输出的设备条目
type pnpDevice struct {
	InstanceID   string `json:"id"`
	LocationPath string `json:"lp"`
	Name         string `json:"name"`
}

// parsePnpJSON 解析 ConvertTo-Json 的输出（兼容单对象与数组）
func parsePnpJSON(out string) []pnpDevice {
	body := strings.TrimSpace(out)
	if body == "" {
		return nil
	}
	if strings.HasPrefix(body, "[") {
		var arr []pnpDevice
		if err := jsonUnmarshalLoose(body, &arr); err == nil {
			return arr
		}
		return nil
	}
	var one pnpDevice
	if err := jsonUnmarshalLoose(body, &one); err == nil {
		return []pnpDevice{one}
	}
	return nil
}

// GetUSBList 枚举宿主机 USB 设备
func (d *HyperVDriver) GetUSBList() (map[string]*model.USBInfos, error) {
	result := make(map[string]*model.USBInfos)

	script := `$ErrorActionPreference='SilentlyContinue'; ` +
		`Get-PnpDevice -Class USB | Where-Object { $_.Status -eq 'OK' } | ` +
		`ForEach-Object { ` +
		`$vid = (Get-PnpDeviceProperty -InstanceId $_.InstanceId -KeyName 'DEVPKEY_Device_HardwareIds').Data; ` +
		`[pscustomobject]@{ id=$_.InstanceId; name=$_.FriendlyName; hw=($vid -join ',') } ` +
		`} | ConvertTo-Json -Compress`

	out, err := d.execPS(script)
	if err != nil {
		return result, nil
	}

	var items []struct {
		InstanceID string `json:"id"`
		Name       string `json:"name"`
		Hardware   string `json:"hw"`
	}
	body := strings.TrimSpace(out)
	if body == "" {
		return result, nil
	}
	if strings.HasPrefix(body, "[") {
		if err := jsonUnmarshalLoose(body, &items); err != nil {
			return result, nil
		}
	} else {
		var one struct {
			InstanceID string `json:"id"`
			Name       string `json:"name"`
			Hardware   string `json:"hw"`
		}
		if err := jsonUnmarshalLoose(body, &one); err != nil {
			return result, nil
		}
		items = append(items, one)
	}

	for _, it := range items {
		vid, pid := extractVidPid(it.Hardware)
		if vid == "" || pid == "" {
			continue
		}
		key := strings.ToLower(vid + ":" + pid)
		result[key] = &model.USBInfos{
			UsbKey:  key,
			VidUUID: strings.ToLower(vid),
			PidUUID: strings.ToLower(pid),
			UsbHint: it.Name,
		}
	}
	return result, nil
}

// extractVidPid 从 HardwareIds 字符串中解析 VID/PID（形如 USB\VID_046D&PID_C52B）
func extractVidPid(hw string) (string, string) {
	upper := strings.ToUpper(hw)
	vid, pid := "", ""
	if i := strings.Index(upper, "VID_"); i >= 0 && i+8 <= len(upper) {
		vid = upper[i+4 : i+8]
	}
	if i := strings.Index(upper, "PID_"); i >= 0 && i+8 <= len(upper) {
		pid = upper[i+4 : i+8]
	}
	return vid, pid
}

// MountUSB 挂载 / 卸载 USB 存储（以宿主物理磁盘直通方式附加）
func (d *HyperVDriver) MountUSB(vmUUID string, usbKey string, mount bool) (*model.ZMessage, error) {
	if strings.TrimSpace(usbKey) == "" {
		return &model.ZMessage{Success: false, Action: "USBMount", Message: "USB设备标识不能为空"}, nil
	}

	if !mount {
		script := fmt.Sprintf(
			"Get-VMHardDiskDrive -VMName '%s' | Where-Object { $_.Path -like '*%s*' } | "+
				"Remove-VMHardDiskDrive -ErrorAction SilentlyContinue",
			psEscape(vmUUID), psEscape(usbKey))
		_, _ = d.execPS(script)
		return &model.ZMessage{Success: true, Action: "USBMount", Message: "USB存储卸载成功"}, nil
	}

	// usbKey 为目标磁盘路径，例如 \\.\PhysicalDrive2
	script := fmt.Sprintf("Add-VMHardDiskDrive -VMName '%s' -Path '%s' -ErrorAction Stop",
		psEscape(vmUUID), psEscape(usbKey))
	if out, err := d.execPS(script); err != nil {
		return &model.ZMessage{Success: false, Action: "USBMount",
			Message: fmt.Sprintf("挂载失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}
	return &model.ZMessage{Success: true, Action: "USBMount", Message: "USB存储挂载成功"}, nil
}

// GetScreenshot 截取虚拟机屏幕（Hyper-V WMI 缩略图接口）
func (d *HyperVDriver) GetScreenshot(vmUUID string) (string, error) {
	if d.GetPower(vmUUID) != "运行中" {
		return "", fmt.Errorf("虚拟机未运行，无法截图")
	}
	return d.captureHyperVScreenshot(vmUUID)
}
