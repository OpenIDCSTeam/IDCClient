// Package driver ESXi/vSphere 驱动扩展：PCI / USB 设备直通
// 与 Python 版 HostServer/vSphereESXi.py 的 PCISetup / USBSetup 语义对齐：
//   - PCI 直通需关机，通过 VirtualPCIPassthrough 设备增删
//   - USB 直通通过 VirtualUSB 设备增删（vendor/product 为 16 进制）
package driver

import (
	"fmt"
	"strconv"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// SetupPCI PCI设备直通（需关机）
func (d *ESXiDriver) SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error) {
	vm, err := d.findVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "PCISetup",
			Message: fmt.Sprintf("查找虚拟机失败: %v", err)}, nil
	}

	if power, _ := vm.PowerState(d.ctx); power != types.VirtualMachinePowerStatePoweredOff {
		return &model.ZMessage{Success: false, Action: "PCISetup",
			Message: "PCI直通需要先关闭虚拟机"}, nil
	}

	var spec types.VirtualMachineConfigSpec
	var actionText string

	if add {
		passthrough := &types.VirtualPCIPassthrough{
			VirtualDevice: types.VirtualDevice{
				Backing: &types.VirtualPCIPassthroughDeviceBackingInfo{Id: cfg.GpuUUID},
			},
		}
		spec.DeviceChange = []types.BaseVirtualDeviceConfigSpec{
			&types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationAdd,
				Device:    passthrough,
			},
		}
		actionText = "添加"
	} else {
		target, err := d.findPCIDevice(vm, cfg.GpuUUID)
		if err != nil {
			return &model.ZMessage{Success: false, Action: "PCISetup", Message: err.Error()}, nil
		}
		spec.DeviceChange = []types.BaseVirtualDeviceConfigSpec{
			&types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationRemove,
				Device:    target,
			},
		}
		actionText = "移除"
	}

	if out, err := d.reconfigure(vm, spec); err != nil {
		return &model.ZMessage{Success: false, Action: "PCISetup", Message: out}, nil
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

	return &model.ZMessage{Success: true, Action: "PCISetup",
		Message: "PCI设备" + actionText + "成功"}, nil
}

// SetupUSB USB设备直通（无需关机）
func (d *ESXiDriver) SetupUSB(vmUUID string, info *model.USBInfos, usbKey string, add bool) (*model.ZMessage, error) {
	vm, err := d.findVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "USBSetup",
			Message: fmt.Sprintf("查找虚拟机失败: %v", err)}, nil
	}

	vendor, _ := strconv.ParseInt(info.VidUUID, 16, 32)
	product, _ := strconv.ParseInt(info.PidUUID, 16, 32)

	var spec types.VirtualMachineConfigSpec
	var actionText string

	if add {
		usbDevice := &types.VirtualUSB{
			VirtualDevice: types.VirtualDevice{
				Backing: &types.VirtualUSBUSBBackingInfo{},
			},
			Connected: true,
			Vendor:    int32(vendor),
			Product:   int32(product),
		}
		spec.DeviceChange = []types.BaseVirtualDeviceConfigSpec{
			&types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationAdd,
				Device:    usbDevice,
			},
		}
		actionText = "添加"
	} else {
		target, err := d.findUSBDevice(vm, int32(vendor), int32(product))
		if err != nil {
			return &model.ZMessage{Success: false, Action: "USBSetup", Message: err.Error()}, nil
		}
		spec.DeviceChange = []types.BaseVirtualDeviceConfigSpec{
			&types.VirtualDeviceConfigSpec{
				Operation: types.VirtualDeviceConfigSpecOperationRemove,
				Device:    target,
			},
		}
		actionText = "移除"
	}

	if out, err := d.reconfigure(vm, spec); err != nil {
		return &model.ZMessage{Success: false, Action: "USBSetup", Message: out}, nil
	}

	d.mu.Lock()
	if vmCfg, ok := d.VmSaving[vmUUID]; ok {
		if vmCfg.UsbAll == nil {
			vmCfg.UsbAll = make(map[string]model.USBInfos)
		}
		if add {
			vmCfg.UsbAll[usbKey] = *info
		} else {
			delete(vmCfg.UsbAll, usbKey)
		}
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "USBSetup",
		Message: "USB设备" + actionText + "成功"}, nil
}

// findPCIDevice 查找指定 PCI 直通设备
func (d *ESXiDriver) findPCIDevice(vm *object.VirtualMachine, pciID string) (types.BaseVirtualDevice, error) {
	var moVM mo.VirtualMachine
	if err := vm.Properties(d.ctx, vm.Reference(), []string{"config.hardware.device"}, &moVM); err != nil {
		return nil, fmt.Errorf("读取设备列表失败: %v", err)
	}
	for _, dev := range moVM.Config.Hardware.Device {
		p, ok := dev.(*types.VirtualPCIPassthrough)
		if !ok {
			continue
		}
		if b, ok2 := p.Backing.(*types.VirtualPCIPassthroughDeviceBackingInfo); ok2 && b.Id == pciID {
			return dev, nil
		}
	}
	return nil, fmt.Errorf("PCI设备不存在")
}

// findUSBDevice 查找指定 USB 设备
func (d *ESXiDriver) findUSBDevice(vm *object.VirtualMachine, vendor, product int32) (types.BaseVirtualDevice, error) {
	var moVM mo.VirtualMachine
	if err := vm.Properties(d.ctx, vm.Reference(), []string{"config.hardware.device"}, &moVM); err != nil {
		return nil, fmt.Errorf("读取设备列表失败: %v", err)
	}
	for _, dev := range moVM.Config.Hardware.Device {
		usb, ok := dev.(*types.VirtualUSB)
		if !ok {
			continue
		}
		if usb.Vendor == vendor && usb.Product == product {
			return dev, nil
		}
	}
	return nil, fmt.Errorf("USB设备不存在")
}

// reconfigure 提交设备变更并等待完成
func (d *ESXiDriver) reconfigure(vm *object.VirtualMachine, spec types.VirtualMachineConfigSpec) (string, error) {
	task, err := vm.Reconfigure(d.ctx, spec)
	if err != nil {
		return fmt.Sprintf("配置失败: %v", err), err
	}
	if err := task.Wait(d.ctx); err != nil {
		return fmt.Sprintf("等待任务失败: %v", err), err
	}
	return "", nil
}
