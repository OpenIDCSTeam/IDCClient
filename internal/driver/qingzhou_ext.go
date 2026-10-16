// Package driver 青州云 驱动扩展实现。
//
// 补齐 Python 版 HostServer/QingzhouYun.py 中存在、而 Go 版此前缺失的能力：
//   - 数据盘挂载 / 卸载（vm/{pid}/disk/mount | disk/unmount）
//   - ISO 挂载 / 卸载（vm/{pid}/iso/mount | iso/unmount）
//   - 屏幕截图（vm/{pid}/screenshot）
//
// 说明：青州云为云端平台，不暴露 PCI / USB 直通能力（Python 版 PCIShows /
// USBShows 亦返回空），相关接口统一返回空或明确失败。
package driver

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// MountHDD 挂载 / 卸载数据盘
func (d *QingzhouDriver) MountHDD(vmUUID string, hdd *model.SDConfig, mount bool) (*model.ZMessage, error) {
	if hdd == nil || strings.TrimSpace(hdd.HddName) == "" {
		return &model.ZMessage{Success: false, Action: "HDDMount", Message: "磁盘信息不完整"}, nil
	}
	pid := d.platformID(vmUUID)
	act := "挂载"
	if !mount {
		act = "卸载"
	}

	if mount {
		if _, err := d.qzRequest(http.MethodPost, fmt.Sprintf("vm/%s/disk/mount", pid),
			map[string]interface{}{
				"disk_name": hdd.HddName,
				"disk_size": hdd.HddSize,
			}); err != nil {
			return &model.ZMessage{Success: false, Action: "HDDMount",
				Message: fmt.Sprintf("硬盘%s失败: %v", act, err)}, nil
		}
		d.mu.Lock()
		if cfg, ok := d.VmSaving[vmUUID]; ok {
			if cfg.HddAll == nil {
				cfg.HddAll = make(map[string]model.SDConfig)
			}
			mounted := *hdd
			mounted.HddFlag = 1
			cfg.HddAll[hdd.HddName] = mounted
		}
		d.mu.Unlock()
		return &model.ZMessage{Success: true, Action: "HDDMount", Message: "硬盘挂载成功"}, nil
	}

	if _, err := d.qzRequest(http.MethodPost, fmt.Sprintf("vm/%s/disk/unmount", pid),
		map[string]interface{}{"disk_name": hdd.HddName}); err != nil {
		return &model.ZMessage{Success: false, Action: "HDDMount",
			Message: fmt.Sprintf("硬盘%s失败: %v", act, err)}, nil
	}
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		if h, exists := cfg.HddAll[hdd.HddName]; exists {
			h.HddFlag = 0
			cfg.HddAll[hdd.HddName] = h
		}
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "HDDMount", Message: "硬盘卸载成功"}, nil
}

// MountISO 挂载 / 卸载 ISO 镜像
func (d *QingzhouDriver) MountISO(vmUUID string, iso *model.IMConfig, mount bool) (*model.ZMessage, error) {
	if iso == nil || strings.TrimSpace(iso.IsoName) == "" {
		return &model.ZMessage{Success: false, Action: "ISOMount", Message: "镜像信息不完整"}, nil
	}
	pid := d.platformID(vmUUID)
	act := "挂载"
	if !mount {
		act = "卸载"
	}

	if mount {
		if _, err := d.qzRequest(http.MethodPost, fmt.Sprintf("vm/%s/iso/mount", pid),
			map[string]interface{}{
				"iso_file": iso.IsoFile,
				"iso_name": iso.IsoName,
			}); err != nil {
			return &model.ZMessage{Success: false, Action: "ISOMount",
				Message: fmt.Sprintf("ISO%s失败: %v", act, err)}, nil
		}
		d.mu.Lock()
		if cfg, ok := d.VmSaving[vmUUID]; ok {
			if cfg.IsoAll == nil {
				cfg.IsoAll = make(map[string]model.IMConfig)
			}
			cfg.IsoAll[iso.IsoName] = *iso
		}
		d.mu.Unlock()
		return &model.ZMessage{Success: true, Action: "ISOMount", Message: "ISO挂载成功"}, nil
	}

	if _, err := d.qzRequest(http.MethodPost, fmt.Sprintf("vm/%s/iso/unmount", pid),
		map[string]interface{}{"iso_name": iso.IsoName}); err != nil {
		return &model.ZMessage{Success: false, Action: "ISOMount",
			Message: fmt.Sprintf("ISO%s失败: %v", act, err)}, nil
	}
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		delete(cfg.IsoAll, iso.IsoName)
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "ISOMount", Message: "ISO卸载成功"}, nil
}

// GetScreenshot 获取虚拟机屏幕截图（返回 data:image/png;base64,...）
func (d *QingzhouDriver) GetScreenshot(vmUUID string) (string, error) {
	pid := d.platformID(vmUUID)
	resp, err := d.qzRequest(http.MethodGet, fmt.Sprintf("vm/%s/screenshot", pid), nil)
	if err != nil {
		return "", fmt.Errorf("获取截图失败: %w", err)
	}
	data := normalizeQzMap(resp["data"])
	image := strVal(data["image"])
	if image == "" {
		return "", fmt.Errorf("平台未返回截图数据")
	}
	// 平台通常已返回 data URL；若为裸 base64 则补齐前缀
	if strings.HasPrefix(image, "data:image") {
		return image, nil
	}
	return "data:image/png;base64," + image, nil
}

// DeleteHDD 删除数据盘：先卸载再删除（青州云未提供独立删除接口）
func (d *QingzhouDriver) DeleteHDD(vmUUID string, hddName string) (*model.ZMessage, error) {
	if strings.TrimSpace(hddName) == "" {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: "磁盘名称不能为空"}, nil
	}
	pid := d.platformID(vmUUID)

	d.mu.RLock()
	var hdd model.SDConfig
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		hdd = cfg.HddAll[hddName]
	}
	d.mu.RUnlock()
	if hdd.HddName == "" {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: "虚拟盘不存在"}, nil
	}

	// 已挂载则先卸载
	if hdd.HddFlag == 1 {
		if _, err := d.qzRequest(http.MethodPost, fmt.Sprintf("vm/%s/disk/unmount", pid),
			map[string]interface{}{"disk_name": hddName}); err != nil {
			return &model.ZMessage{Success: false, Action: "RMMounts",
				Message: "卸载磁盘失败: " + err.Error()}, nil
		}
	}
	// 尝试调用平台的磁盘删除接口（部分版本支持）
	if _, err := d.qzRequest(http.MethodPost, fmt.Sprintf("vm/%s/disk/delete", pid),
		map[string]interface{}{"disk_name": hddName}); err != nil {
		return &model.ZMessage{Success: false, Action: "RMMounts",
			Message: "删除磁盘失败: " + err.Error()}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		delete(cfg.HddAll, hddName)
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "RMMounts", Message: "数据盘删除成功"}, nil
}

// GetPCIList 青州云不暴露 PCI 设备（与 Python 版一致）
func (d *QingzhouDriver) GetPCIList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// GetGPUList 青州云不暴露 GPU 直通设备
func (d *QingzhouDriver) GetGPUList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// GetUSBList 青州云不暴露 USB 设备（与 Python 版一致）
func (d *QingzhouDriver) GetUSBList() (map[string]*model.USBInfos, error) {
	return make(map[string]*model.USBInfos), nil
}

// SetupPCI 青州云不支持 PCI 直通，明确拒绝
func (d *QingzhouDriver) SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "PCISetup",
		Message: "青州云不支持 PCI 设备直通"}, nil
}

// SetupUSB 青州云不支持 USB 直通，明确拒绝
func (d *QingzhouDriver) SetupUSB(vmUUID string, info *model.USBInfos, usbKey string, add bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "USBSetup",
		Message: "青州云不支持 USB 设备直通"}, nil
}

// MountUSB 青州云不支持 USB 存储挂载
func (d *QingzhouDriver) MountUSB(vmUUID string, usbKey string, mount bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "USBMount",
		Message: "青州云不支持 USB 存储挂载"}, nil
}
