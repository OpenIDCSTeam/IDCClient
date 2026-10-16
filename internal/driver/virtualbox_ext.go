// Package driver VirtualBox 驱动扩展实现。
//
// 补齐 Python 版 HostServer/VirtualBoxs.py 中存在、而 Go 版此前缺失的能力：
//   - 数据盘删除 / 移交（使用 VDI 路径，而非基类的通用命名）
//   - USB 设备枚举（VBoxManage list usbhost）
//   - USB 存储挂载
//   - 屏幕截图（VBoxManage controlvm screenshotpng）
package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// vboxDataDiskPath 返回数据盘的 VDI 路径（与 MountHDD 保持一致）
func (d *VirtualBoxDriver) vboxDataDiskPath(hdd *model.SDConfig) string {
	if hdd != nil && strings.TrimSpace(hdd.HddPath) != "" {
		return hdd.HddPath
	}
	name := ""
	if hdd != nil {
		name = hdd.HddName
	}
	return filepath.Join(d.vboxDiskDir(), name+".vdi")
}

// DeleteHDD 删除数据盘：卸载 SATA 端口 → 关闭并删除 VDI 介质。
func (d *VirtualBoxDriver) DeleteHDD(vmUUID string, hddName string) (*model.ZMessage, error) {
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

	// 1) 卸载（SATA port 1）
	_, _ = d.execVBox("storageattach", vmUUID, "--storagectl", "SATA",
		"--port", "1", "--device", "0", "--type", "hdd", "--medium", "none")

	// 2) 删除真实介质文件
	path := d.vboxDataDiskPath(&hdd)
	if path != "" && !strings.Contains(path, "://") {
		if out, err := d.execVBox("closemedium", "disk", path, "--delete"); err != nil {
			// 介质可能已被删除或未注册，仅在文件仍存在时视为失败
			if _, statErr := os.Stat(path); statErr == nil {
				return &model.ZMessage{Success: false, Action: "RMMounts",
					Message: fmt.Sprintf("删除磁盘介质失败: %v (%s)", err, strings.TrimSpace(out))}, nil
			}
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return &model.ZMessage{Success: false, Action: "RMMounts",
				Message: "删除磁盘文件失败: " + err.Error()}, nil
		}
	}

	d.mu.Lock()
	delete(cfg.HddAll, hddName)
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "RMMounts", Message: "数据盘删除成功"}, nil
}

// TransferHDD 移交数据盘：从源卸载并挂到目标虚拟机的 SATA 端口。
func (d *VirtualBoxDriver) TransferHDD(vmUUID string, hdd *model.SDConfig, targetVM string) (*model.ZMessage, error) {
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

	path := d.vboxDataDiskPath(hdd)
	if path == "" {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "无法定位磁盘路径"}, nil
	}

	// 1) 源卸载
	if out, err := d.execVBox("storageattach", vmUUID, "--storagectl", "SATA",
		"--port", "1", "--device", "0", "--type", "hdd", "--medium", "none"); err != nil {
		return &model.ZMessage{Success: false, Action: "HDDTrans",
			Message: fmt.Sprintf("源虚拟机卸载失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	// 2) 目标挂载（端口 1）
	if out, err := d.execVBox("storageattach", targetVM, "--storagectl", "SATA",
		"--port", "1", "--device", "0", "--type", "hdd", "--medium", path); err != nil {
		// 回滚
		_, _ = d.execVBox("storageattach", vmUUID, "--storagectl", "SATA",
			"--port", "1", "--device", "0", "--type", "hdd", "--medium", path)
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

// GetUSBList 枚举宿主机 USB 设备（VBoxManage list usbhost）
func (d *VirtualBoxDriver) GetUSBList() (map[string]*model.USBInfos, error) {
	result := make(map[string]*model.USBInfos)
	out, err := d.execVBox("list", "usbhost")
	if err != nil {
		return result, nil
	}

	var cur model.USBInfos
	flush := func() {
		if cur.VidUUID != "" && cur.PidUUID != "" {
			key := fmt.Sprintf("%s:%s", strings.ToLower(cur.VidUUID), strings.ToLower(cur.PidUUID))
			entry := cur
			entry.UsbKey = key
			result[key] = &entry
		}
		cur = model.USBInfos{}
	}

	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "UUID:"):
			flush()
		case strings.HasPrefix(trimmed, "VendorId:"):
			cur.VidUUID = strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(trimmed, "VendorId:")), "0x")
		case strings.HasPrefix(trimmed, "ProductId:"):
			cur.PidUUID = strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(trimmed, "ProductId:")), "0x")
		case strings.HasPrefix(trimmed, "Product:"):
			cur.UsbHint = strings.TrimSpace(strings.TrimPrefix(trimmed, "Product:"))
		}
	}
	flush()
	return result, nil
}

// GetPCIList VirtualBox 不支持 PCI 直通，返回空列表（与 Python 版语义一致）
func (d *VirtualBoxDriver) GetPCIList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// GetGPUList VirtualBox 不支持 GPU 直通，返回空列表
func (d *VirtualBoxDriver) GetGPUList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// SetupPCI VirtualBox 无 PCI 直通能力，明确拒绝而非假成功
func (d *VirtualBoxDriver) SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "PCISetup",
		Message: "VirtualBox 不支持 PCI 设备直通"}, nil
}

// MountUSB 挂载 / 卸载 USB 存储设备（以宿主块设备形式附加到 SATA 端口 2）
func (d *VirtualBoxDriver) MountUSB(vmUUID string, usbKey string, mount bool) (*model.ZMessage, error) {
	if strings.TrimSpace(usbKey) == "" {
		return &model.ZMessage{Success: false, Action: "USBMount", Message: "USB设备标识不能为空"}, nil
	}

	if !mount {
		if out, err := d.execVBox("storageattach", vmUUID, "--storagectl", "SATA",
			"--port", "2", "--device", "0", "--type", "hdd", "--medium", "none"); err != nil {
			return &model.ZMessage{Success: false, Action: "USBMount",
				Message: fmt.Sprintf("卸载失败: %v (%s)", err, strings.TrimSpace(out))}, nil
		}
		return &model.ZMessage{Success: true, Action: "USBMount", Message: "USB存储卸载成功"}, nil
	}

	// usbKey 为宿主机块设备路径（如 /dev/sdb 或 \\.\PhysicalDrive1）
	if _, err := os.Stat(usbKey); err != nil {
		return &model.ZMessage{Success: false, Action: "USBMount",
			Message: "USB设备路径不存在: " + usbKey}, nil
	}
	if out, err := d.execVBox("storageattach", vmUUID, "--storagectl", "SATA",
		"--port", "2", "--device", "0", "--type", "hdd", "--medium", usbKey); err != nil {
		return &model.ZMessage{Success: false, Action: "USBMount",
			Message: fmt.Sprintf("挂载失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}
	return &model.ZMessage{Success: true, Action: "USBMount", Message: "USB存储挂载成功"}, nil
}

// GetScreenshot 截取虚拟机屏幕（VBoxManage controlvm screenshotpng）
func (d *VirtualBoxDriver) GetScreenshot(vmUUID string) (string, error) {
	if d.GetPower(vmUUID) != "运行中" {
		return "", fmt.Errorf("虚拟机未运行，无法截图")
	}
	return d.captureVirtualBoxScreenshot(vmUUID)
}
