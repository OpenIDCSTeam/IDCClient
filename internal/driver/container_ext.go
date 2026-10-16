// Package driver 容器类平台（LXD / Docker）扩展实现。
//
// 容器没有独立的显卡/光驱/PCI 设备，控制台通过 exec 会话访问（TTY）。
// 这里补齐 Python 版 LXContainer.py / OCInterface.py 中的差异点：
//   - GetVNCInfo：返回 TTY 控制台连接方式，而不是直接「不支持」
//   - GetPCIList / GetGPUList：容器不暴露 PCI/GPU 设备
//   - SetupPCI：明确拒绝（容器无法直通 PCI）
//   - MountISO / MountUSB：容器无光驱与 USB 设备，明确拒绝
//
// 磁盘（MountHDD / DeleteHDD / TransferHDD）沿用基类的宿主机路径映射语义，
// 与 Python 版保持一致。
package driver

import (
	"fmt"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// ============================================================
// LXD
// ============================================================

// GetVNCInfo LXD 容器控制台（lxc exec 交互式会话）
func (d *LXDDriver) GetVNCInfo(vmUUID string, ipAddr string) (*model.ZMessage, error) {
	if d.GetPower(vmUUID) != "运行中" {
		return &model.ZMessage{Success: false, Action: "VCRemote",
			Message: "容器未运行，无法连接控制台"}, nil
	}
	host := d.Config.ServerAddr
	if host == "" {
		host = "127.0.0.1"
	}
	return &model.ZMessage{Success: true, Action: "VCRemote", Message: "容器控制台信息获取成功",
		Results: map[string]interface{}{
			"type":    "tty",
			"host":    host,
			"vm_uuid": vmUUID,
			"connect": fmt.Sprintf("lxc exec %s -- bash", vmUUID),
		}}, nil
}

// GetPCIList LXD 容器不暴露 PCI 设备
func (d *LXDDriver) GetPCIList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// GetGPUList LXD 容器不暴露 GPU 直通设备
func (d *LXDDriver) GetGPUList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// GetUSBList LXD 容器不暴露 USB 设备
func (d *LXDDriver) GetUSBList() (map[string]*model.USBInfos, error) {
	return make(map[string]*model.USBInfos), nil
}

// SetupPCI 容器无法直通 PCI 设备，明确拒绝
func (d *LXDDriver) SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "PCISetup",
		Message: "容器平台不支持 PCI 设备直通"}, nil
}

// SetupUSB 容器无法直通 USB 设备，明确拒绝
func (d *LXDDriver) SetupUSB(vmUUID string, info *model.USBInfos, usbKey string, add bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "USBSetup",
		Message: "容器平台不支持 USB 设备直通"}, nil
}

// MountISO 容器无光驱设备，明确拒绝
func (d *LXDDriver) MountISO(vmUUID string, iso *model.IMConfig, mount bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "ISOMount",
		Message: "容器平台不支持 ISO 光驱"}, nil
}

// MountUSB 容器不支持 USB 存储
func (d *LXDDriver) MountUSB(vmUUID string, usbKey string, mount bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "USBMount",
		Message: "容器平台不支持 USB 存储挂载"}, nil
}

// ============================================================
// Docker / OCI
// ============================================================

// GetVNCInfo Docker 容器控制台（docker exec 交互式会话）
func (d *DockerDriver) GetVNCInfo(vmUUID string, ipAddr string) (*model.ZMessage, error) {
	if d.GetPower(vmUUID) != "运行中" {
		return &model.ZMessage{Success: false, Action: "VCRemote",
			Message: "容器未运行，无法连接控制台"}, nil
	}
	host := d.Config.ServerAddr
	if host == "" {
		host = "127.0.0.1"
	}
	return &model.ZMessage{Success: true, Action: "VCRemote", Message: "容器控制台信息获取成功",
		Results: map[string]interface{}{
			"type":    "tty",
			"host":    host,
			"vm_uuid": vmUUID,
			"connect": fmt.Sprintf("docker exec -it %s sh", vmUUID),
		}}, nil
}

// GetPCIList Docker 容器不暴露 PCI 设备
func (d *DockerDriver) GetPCIList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// GetGPUList Docker 容器不暴露 GPU 直通设备
func (d *DockerDriver) GetGPUList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// GetUSBList Docker 容器不暴露 USB 设备
func (d *DockerDriver) GetUSBList() (map[string]*model.USBInfos, error) {
	return make(map[string]*model.USBInfos), nil
}

// SetupPCI 容器无法直通 PCI 设备，明确拒绝
func (d *DockerDriver) SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "PCISetup",
		Message: "容器平台不支持 PCI 设备直通"}, nil
}

// SetupUSB 容器无法直通 USB 设备，明确拒绝
func (d *DockerDriver) SetupUSB(vmUUID string, info *model.USBInfos, usbKey string, add bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "USBSetup",
		Message: "容器平台不支持 USB 设备直通"}, nil
}

// MountISO 容器无光驱设备，明确拒绝
func (d *DockerDriver) MountISO(vmUUID string, iso *model.IMConfig, mount bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "ISOMount",
		Message: "容器平台不支持 ISO 光驱"}, nil
}

// MountUSB 容器不支持 USB 存储
func (d *DockerDriver) MountUSB(vmUUID string, usbKey string, mount bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "USBMount",
		Message: "容器平台不支持 USB 存储挂载"}, nil
}
