// Package driver USB 存储挂载：Proxmox 与 ESXi 平台实现。
//
// 这两个平台的 USB 存储挂载方式与 USB 设备直通相同（以 host=vid:pid 形式
// 追加 USB 控制器设备），因此这里以设备直通原语实现 MountUSB，
// 与 Python 版「USB 设备直通即挂载」的语义保持一致。
package driver

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	proxmox "github.com/luthermonson/go-proxmox"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// ============================================================
// Proxmox
// ============================================================

// MountUSB 挂载 / 卸载 USB 存储（以 usbN: host=vid:pid 形式下发）
//
// usbKey 需形如 "046d:c52b"（VID:PID），由 GetUSBList 返回。
func (d *ProxmoxDriver) MountUSB(vmUUID string, usbKey string, mount bool) (*model.ZMessage, error) {
	key := strings.TrimSpace(usbKey)
	if key == "" {
		return &model.ZMessage{Success: false, Action: "USBMount", Message: "USB设备标识不能为空"}, nil
	}
	parts := strings.SplitN(strings.TrimPrefix(key, "0x"), ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return &model.ZMessage{Success: false, Action: "USBMount",
			Message: "USB设备标识需为 VID:PID 形式，当前为: " + key}, nil
	}
	usbVal := fmt.Sprintf("host=%s:%s", strings.ToLower(parts[0]), strings.ToLower(parts[1]))

	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "USBMount", Message: err.Error()}, nil
	}

	if mount {
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "usb0",
			Value: usbVal,
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "USBMount",
				Message: fmt.Sprintf("挂载USB存储失败: %v", err)}, nil
		}
		d.mu.Lock()
		if cfg, ok := d.VmSaving[vmUUID]; ok {
			if cfg.UsbAll == nil {
				cfg.UsbAll = make(map[string]model.USBInfos)
			}
			cfg.UsbAll[key] = model.USBInfos{
				UsbKey: key, VidUUID: parts[0], PidUUID: parts[1], UsbHint: "USB存储",
			}
		}
		d.mu.Unlock()
		return &model.ZMessage{Success: true, Action: "USBMount", Message: "USB存储挂载成功"}, nil
	}

	if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
		Name:  "delete",
		Value: "usb0",
	}); err != nil {
		return &model.ZMessage{Success: false, Action: "USBMount",
			Message: fmt.Sprintf("卸载USB存储失败: %v", err)}, nil
	}
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		delete(cfg.UsbAll, key)
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "USBMount", Message: "USB存储卸载成功"}, nil
}

// ============================================================
// ESXi / vSphere
// ============================================================

// MountUSB 挂载 / 卸载 USB 存储（以 VirtualUSB 设备形式下发）
func (d *ESXiDriver) MountUSB(vmUUID string, usbKey string, mount bool) (*model.ZMessage, error) {
	key := strings.TrimSpace(usbKey)
	if key == "" {
		return &model.ZMessage{Success: false, Action: "USBMount", Message: "USB设备标识不能为空"}, nil
	}
	parts := strings.SplitN(strings.TrimPrefix(key, "0x"), ":", 2)
	if len(parts) != 2 {
		return &model.ZMessage{Success: false, Action: "USBMount",
			Message: "USB设备标识需为 VID:PID 形式，当前为: " + key}, nil
	}
	vendor, err := strconv.ParseInt(parts[0], 16, 32)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "USBMount",
			Message: "VID 解析失败: " + parts[0]}, nil
	}
	product, err := strconv.ParseInt(parts[1], 16, 32)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "USBMount",
			Message: "PID 解析失败: " + parts[1]}, nil
	}

	vm, err := d.findVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "USBMount", Message: err.Error()}, nil
	}

	if mount {
		usbDevice := &types.VirtualUSB{
			VirtualDevice: types.VirtualDevice{
				Backing: &types.VirtualUSBUSBBackingInfo{},
			},
			Connected: true,
			Vendor:    int32(vendor),
			Product:   int32(product),
		}
		spec := types.VirtualMachineConfigSpec{
			DeviceChange: []types.BaseVirtualDeviceConfigSpec{
				&types.VirtualDeviceConfigSpec{
					Operation: types.VirtualDeviceConfigSpecOperationAdd,
					Device:    usbDevice,
				},
			},
		}
		if out, err := d.reconfigure(vm, spec); err != nil {
			return &model.ZMessage{Success: false, Action: "USBMount",
				Message: "挂载USB存储失败: " + out}, nil
		}
		d.mu.Lock()
		if cfg, ok := d.VmSaving[vmUUID]; ok {
			if cfg.UsbAll == nil {
				cfg.UsbAll = make(map[string]model.USBInfos)
			}
			cfg.UsbAll[key] = model.USBInfos{
				UsbKey: key, VidUUID: parts[0], PidUUID: parts[1], UsbHint: "USB存储",
			}
		}
		d.mu.Unlock()
		return &model.ZMessage{Success: true, Action: "USBMount", Message: "USB存储挂载成功"}, nil
	}

	target, err := d.findUSBDevice(vm, int32(vendor), int32(product))
	if err != nil {
		return &model.ZMessage{Success: false, Action: "USBMount", Message: err.Error()}, nil
	}
	spec := types.VirtualMachineConfigSpec{
		DeviceChange: []types.BaseVirtualDeviceConfigSpec{
			&types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationRemove,
				Device:    target,
			},
		},
	}
	if out, err := d.reconfigure(vm, spec); err != nil {
		return &model.ZMessage{Success: false, Action: "USBMount",
			Message: "卸载USB存储失败: " + out}, nil
	}
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		delete(cfg.UsbAll, key)
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "USBMount", Message: "USB存储卸载成功"}, nil
}
