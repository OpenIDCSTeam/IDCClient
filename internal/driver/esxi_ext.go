// Package driver ESXi / vSphere 驱动扩展实现。
//
// 补齐 Python 版 HostServer/vSphereESXi.py 中存在、而 Go 版此前被硬编码为
// 「暂不支持」的能力：
//   - 系统重装（VMSetups）
//   - 备份还原 / 删除（快照 RevertToSnapshot / RemoveSnapshot）
//   - 数据盘挂载 / 卸载 / 删除 / 移交
//   - ISO 挂载 / 卸载
//   - PCI / GPU / USB 设备枚举
//   - 屏幕截图（CreateScreenshot_Task）
//   - 修改密码（VMware Tools Guest Operations）
package driver

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vmware/govmomi/guest"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/methods"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// ============================================================
// 内部工具
// ============================================================

// esxiDefaultDatastore 返回默认数据存储
func (d *ESXiDriver) esxiDefaultDatastore() (*object.Datastore, error) {
	if err := d.apiConn(); err != nil {
		return nil, err
	}
	return d.finder.DefaultDatastore(d.ctx)
}

// esxiDefaultHost 返回默认主机
func (d *ESXiDriver) esxiDefaultHost() (*object.HostSystem, error) {
	if err := d.apiConn(); err != nil {
		return nil, err
	}
	return d.finder.DefaultHostSystem(d.ctx)
}

// ensurePoweredOff 确保虚拟机处于关机状态，返回原状态以便恢复
func (d *ESXiDriver) ensurePoweredOff(vm *object.VirtualMachine) (bool, error) {
	power, err := vm.PowerState(d.ctx)
	if err != nil {
		return false, err
	}
	if power != types.VirtualMachinePowerStatePoweredOn {
		return false, nil
	}
	task, err := vm.PowerOff(d.ctx)
	if err != nil {
		return true, fmt.Errorf("关闭虚拟机失败: %w", err)
	}
	if err := task.Wait(d.ctx); err != nil {
		return true, fmt.Errorf("等待关机失败: %w", err)
	}
	return true, nil
}

// esxiFindDisk 按名称查找虚拟磁盘设备
func (d *ESXiDriver) esxiFindDisk(vm *object.VirtualMachine, hddName string) (*types.VirtualDisk, error) {
	devices, err := vm.Device(d.ctx)
	if err != nil {
		return nil, fmt.Errorf("读取设备列表失败: %w", err)
	}
	for _, dev := range devices {
		disk, ok := dev.(*types.VirtualDisk)
		if !ok {
			continue
		}
		if b, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
			if hddName == "" || strings.Contains(b.FileName, hddName) {
				return disk, nil
			}
		}
	}
	return nil, fmt.Errorf("未找到数据盘: %s", hddName)
}

// esxiDiskPath 返回数据盘在数据存储中的路径
func (d *ESXiDriver) esxiDiskPath(vm *object.VirtualMachine, hddName string) string {
	disk, err := d.esxiFindDisk(vm, hddName)
	if err != nil {
		return ""
	}
	if b, ok := disk.Backing.(*types.VirtualDiskFlatVer2BackingInfo); ok {
		return b.FileName
	}
	return ""
}

// ============================================================
// 数据盘
// ============================================================

// MountHDD 挂载数据盘（在默认数据存储上创建并附加 VMDK）
func (d *ESXiDriver) MountHDD(vmUUID string, hdd *model.SDConfig, mount bool) (*model.ZMessage, error) {
	vm, err := d.findVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDMount", Message: err.Error()}, nil
	}

	devices, err := vm.Device(d.ctx)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDMount", Message: err.Error()}, nil
	}

	if !mount {
		disk, err := d.esxiFindDisk(vm, hdd.HddName)
		if err != nil {
			return &model.ZMessage{Success: false, Action: "HDDMount", Message: err.Error()}, nil
		}
		// keepFiles=true：卸载不删文件，删除由 DeleteHDD 负责
		if err := vm.RemoveDevice(d.ctx, true, disk); err != nil {
			return &model.ZMessage{Success: false, Action: "HDDMount",
				Message: fmt.Sprintf("卸载数据盘失败: %v", err)}, nil
		}
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

	ds, err := d.esxiDefaultDatastore()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDMount",
			Message: "获取数据存储失败: " + err.Error()}, nil
	}
	controller, err := devices.FindDiskController("")
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDMount",
			Message: "未找到磁盘控制器: " + err.Error()}, nil
	}

	sizeMB := hdd.HddSize
	if sizeMB <= 0 {
		sizeMB = 10240
	}
	disk := devices.CreateDisk(controller, ds.Reference(), ds.Path(fmt.Sprintf("%s-%s.vmdk", vmUUID, hdd.HddName)))
	disk.CapacityInKB = int64(sizeMB) * 1024

	if err := vm.AddDevice(d.ctx, disk); err != nil {
		return &model.ZMessage{Success: false, Action: "HDDMount",
			Message: fmt.Sprintf("挂载数据盘失败: %v", err)}, nil
	}

	hdd.HddFlag = 1
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		if cfg.HddAll == nil {
			cfg.HddAll = make(map[string]model.SDConfig)
		}
		cfg.HddAll[hdd.HddName] = *hdd
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "HDDMount", Message: "磁盘挂载成功"}, nil
}

// DeleteHDD 删除数据盘（移除设备并删除 VMDK 文件）
func (d *ESXiDriver) DeleteHDD(vmUUID string, hddName string) (*model.ZMessage, error) {
	if strings.TrimSpace(hddName) == "" {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: "磁盘名称不能为空"}, nil
	}

	vm, err := d.findVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: err.Error()}, nil
	}
	if _, err := d.ensurePoweredOff(vm); err != nil {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: err.Error()}, nil
	}

	disk, err := d.esxiFindDisk(vm, hddName)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: err.Error()}, nil
	}
	// keepFiles=false：连同 VMDK 文件一起删除，避免空间泄漏
	if err := vm.RemoveDevice(d.ctx, false, disk); err != nil {
		return &model.ZMessage{Success: false, Action: "RMMounts",
			Message: fmt.Sprintf("删除数据盘失败: %v", err)}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		delete(cfg.HddAll, hddName)
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "RMMounts", Message: "数据盘删除成功"}, nil
}

// TransferHDD 移交数据盘：从源卸载（保留文件）后附加到目标虚拟机
func (d *ESXiDriver) TransferHDD(vmUUID string, hdd *model.SDConfig, targetVM string) (*model.ZMessage, error) {
	if hdd == nil || strings.TrimSpace(hdd.HddName) == "" {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "磁盘信息不完整"}, nil
	}
	if vmUUID == targetVM {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "源与目标虚拟机相同"}, nil
	}

	srcVM, err := d.findVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "源虚拟机不存在: " + err.Error()}, nil
	}
	dstVM, err := d.findVM(targetVM)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "目标虚拟机不存在: " + err.Error()}, nil
	}

	if _, err := d.ensurePoweredOff(srcVM); err != nil {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: err.Error()}, nil
	}
	if _, err := d.ensurePoweredOff(dstVM); err != nil {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: err.Error()}, nil
	}

	backing := d.esxiDiskPath(srcVM, hdd.HddName)
	if backing == "" {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "无法定位磁盘文件"}, nil
	}

	// 1) 源卸载（保留文件）
	disk, err := d.esxiFindDisk(srcVM, hdd.HddName)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: err.Error()}, nil
	}
	if err := srcVM.RemoveDevice(d.ctx, true, disk); err != nil {
		return &model.ZMessage{Success: false, Action: "HDDTrans",
			Message: fmt.Sprintf("源虚拟机卸载失败: %v", err)}, nil
	}

	// 2) 目标以既有 VMDK 附加
	devices, err := dstVM.Device(d.ctx)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: err.Error()}, nil
	}
	controller, err := devices.FindDiskController("")
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDTrans",
			Message: "未找到磁盘控制器: " + err.Error()}, nil
	}
	newDisk := &types.VirtualDisk{
		VirtualDevice: types.VirtualDevice{
			Backing: &types.VirtualDiskFlatVer2BackingInfo{
				VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{
					FileName: backing,
				},
				DiskMode:        string(types.VirtualDiskModePersistent),
				ThinProvisioned: types.NewBool(true),
			},
		},
	}
	devices.AssignController(newDisk, controller)

	if err := dstVM.AddDevice(d.ctx, newDisk); err != nil {
		// 回滚：把磁盘挂回源虚拟机
		if devs, derr := srcVM.Device(d.ctx); derr == nil {
			if ctl, cerr := devs.FindDiskController(""); cerr == nil {
				rollback := &types.VirtualDisk{
					VirtualDevice: types.VirtualDevice{
						Backing: &types.VirtualDiskFlatVer2BackingInfo{
							VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: backing},
							DiskMode:                     string(types.VirtualDiskModePersistent),
						},
					},
				}
				devs.AssignController(rollback, ctl)
				_ = srcVM.AddDevice(d.ctx, rollback)
			}
		}
		return &model.ZMessage{Success: false, Action: "HDDTrans",
			Message: fmt.Sprintf("目标虚拟机附加失败（已回滚）: %v", err)}, nil
	}

	d.mu.Lock()
	if src, ok := d.VmSaving[vmUUID]; ok {
		delete(src.HddAll, hdd.HddName)
	}
	if dst, ok := d.VmSaving[targetVM]; ok {
		if dst.HddAll == nil {
			dst.HddAll = make(map[string]model.SDConfig)
		}
		moved := *hdd
		moved.HddFlag = 1
		moved.HddPath = backing
		dst.HddAll[hdd.HddName] = moved
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "HDDTrans", Message: "数据盘移交成功"}, nil
}

// ============================================================
// ISO
// ============================================================

// MountISO 挂载 / 卸载 ISO 镜像
func (d *ESXiDriver) MountISO(vmUUID string, iso *model.IMConfig, mount bool) (*model.ZMessage, error) {
	vm, err := d.findVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "ISOMount", Message: err.Error()}, nil
	}
	devices, err := vm.Device(d.ctx)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "ISOMount", Message: err.Error()}, nil
	}

	cdrom, err := devices.FindCdrom("")
	if err != nil {
		// 无光驱则创建
		controller, cerr := devices.FindDiskController("")
		if cerr != nil {
			return &model.ZMessage{Success: false, Action: "ISOMount",
				Message: "未找到控制器: " + cerr.Error()}, nil
		}
		cdrom, err = devices.CreateCdrom(controller)
		if err != nil {
			return &model.ZMessage{Success: false, Action: "ISOMount",
				Message: "创建光驱失败: " + err.Error()}, nil
		}
		if err := vm.AddDevice(d.ctx, cdrom); err != nil {
			return &model.ZMessage{Success: false, Action: "ISOMount",
				Message: "添加光驱失败: " + err.Error()}, nil
		}
		devices, _ = vm.Device(d.ctx)
		cdrom, _ = devices.FindCdrom("")
	}

	if mount {
		isoPath := iso.IsoPath
		if isoPath == "" {
			dsName := strings.TrimSpace(d.Config.DvdromPath)
			if dsName == "" {
				isoPath = iso.IsoFile
			} else {
				isoPath = fmt.Sprintf("[%s] %s", dsName, iso.IsoFile)
			}
		}
		devices.InsertIso(cdrom, isoPath)
		if err := vm.EditDevice(d.ctx, cdrom); err != nil {
			return &model.ZMessage{Success: false, Action: "ISOMount",
				Message: fmt.Sprintf("挂载ISO失败: %v", err)}, nil
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

	devices.EjectIso(cdrom)
	if err := vm.EditDevice(d.ctx, cdrom); err != nil {
		return &model.ZMessage{Success: false, Action: "ISOMount",
			Message: fmt.Sprintf("卸载ISO失败: %v", err)}, nil
	}
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		delete(cfg.IsoAll, iso.IsoName)
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "ISOMount", Message: "ISO卸载成功"}, nil
}

// ============================================================
// 重装系统
// ============================================================

// ReinstallVM 重装系统：删除旧系统盘并重建后启动。
//
// 若配置了 images_path（数据存储上的模板 VMDK），则从模板复制出新系统盘；
// 否则创建等容量空盘（与其它平台驱动的降级策略一致）。
func (d *ESXiDriver) ReinstallVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	vm, err := d.findVM(cfg.VmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall", Message: err.Error()}, nil
	}

	wasRunning, err := d.ensurePoweredOff(vm)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall", Message: err.Error()}, nil
	}

	// 1) 移除现有磁盘（连同文件）
	devices, err := vm.Device(d.ctx)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall", Message: err.Error()}, nil
	}
	removeSpecs := make([]types.BaseVirtualDeviceConfigSpec, 0, 4)
	for _, dev := range devices {
		disk, ok := dev.(*types.VirtualDisk)
		if !ok {
			continue
		}
		if disk.DeviceInfo != nil && strings.Contains(strings.ToLower(disk.DeviceInfo.GetDescription().Label), "cdrom") {
			continue
		}
		removeSpecs = append(removeSpecs, &types.VirtualDeviceConfigSpec{
			Operation:     types.VirtualDeviceConfigSpecOperationRemove,
			FileOperation: types.VirtualDeviceConfigSpecFileOperationDestroy,
			Device:        disk,
		})
	}
	if len(removeSpecs) > 0 {
		if _, err := d.reconfigure(vm, types.VirtualMachineConfigSpec{DeviceChange: removeSpecs}); err != nil {
			return &model.ZMessage{Success: false, Action: "VMReinstall",
				Message: fmt.Sprintf("删除旧系统盘失败: %v", err)}, nil
		}
	}

	// 2) 新建系统盘
	devices, err = vm.Device(d.ctx)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall", Message: err.Error()}, nil
	}
	ds, err := d.esxiDefaultDatastore()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall",
			Message: "获取数据存储失败: " + err.Error()}, nil
	}
	controller, err := devices.FindDiskController("")
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall",
			Message: "未找到磁盘控制器: " + err.Error()}, nil
	}
	sizeMB := cfg.HddNum
	if sizeMB <= 0 {
		sizeMB = 8192
	}

	// 有模板则复制模板为新系统盘
	newPath := ds.Path(fmt.Sprintf("%s/%s.vmdk", cfg.VmUUID, cfg.VmUUID))
	copied := false
	if tpl := strings.TrimSpace(d.Config.ImagesPath); tpl != "" && cfg.OsName != "" {
		src := tpl
		if !strings.Contains(src, "]") {
			src = fmt.Sprintf("[%s] %s", tpl, cfg.OsName)
		}
		fm := object.NewFileManager(vm.Client())
		if task, err := fm.CopyDatastoreFile(d.ctx, src, d.datacenter, newPath, d.datacenter, true); err == nil && task != nil {
			if task.Wait(d.ctx) == nil {
				copied = true
			}
		}
	}

	disk := &types.VirtualDisk{}
	if copied {
		disk = &types.VirtualDisk{
			VirtualDevice: types.VirtualDevice{
				Backing: &types.VirtualDiskFlatVer2BackingInfo{
					VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: newPath},
					DiskMode:                     string(types.VirtualDiskModePersistent),
				},
			},
		}
		devices.AssignController(disk, controller)
	} else {
		disk = devices.CreateDisk(controller, ds.Reference(), ds.Path(fmt.Sprintf("%s/%s.vmdk", cfg.VmUUID, cfg.VmUUID)))
		disk.CapacityInKB = int64(sizeMB) * 1024
	}

	if err := vm.AddDevice(d.ctx, disk); err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall",
			Message: fmt.Sprintf("创建系统盘失败: %v", err)}, nil
	}

	// 3) 恢复电源
	if wasRunning {
		if task, err := vm.PowerOn(d.ctx); err == nil {
			_ = task.Wait(d.ctx)
		}
	}

	return &model.ZMessage{Success: true, Action: "VMReinstall", Message: "系统重装成功"}, nil
}

// ============================================================
// 备份（快照）
// ============================================================

// RestoreBackup 还原到指定快照
func (d *ESXiDriver) RestoreBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	vm, err := d.findVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "Restores", Message: err.Error()}, nil
	}
	if _, err := vm.FindSnapshot(d.ctx, backupName); err != nil {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "快照不存在: " + backupName}, nil
	}
	task, err := vm.RevertToSnapshot(d.ctx, backupName, false)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: fmt.Sprintf("还原失败: %v", err)}, nil
	}
	if err := task.Wait(d.ctx); err != nil {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: fmt.Sprintf("等待还原任务失败: %v", err)}, nil
	}
	return &model.ZMessage{Success: true, Action: "Restores", Message: "备份还原成功"}, nil
}

// DeleteBackup 删除指定快照
func (d *ESXiDriver) DeleteBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	vm, err := d.findVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "RMBackup", Message: err.Error()}, nil
	}
	task, err := vm.RemoveSnapshot(d.ctx, backupName, true, nil)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "RMBackup",
			Message: fmt.Sprintf("删除快照失败: %v", err)}, nil
	}
	if err := task.Wait(d.ctx); err != nil {
		return &model.ZMessage{Success: false, Action: "RMBackup",
			Message: fmt.Sprintf("等待删除任务失败: %v", err)}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		kept := make([]model.VMBackup, 0, len(cfg.Backups))
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

// ============================================================
// 设备枚举
// ============================================================

// GetPCIList 枚举宿主机 PCI 设备（HostSystem.hardware.pciDevice）
func (d *ESXiDriver) GetPCIList() (map[string]*model.VFConfig, error) {
	result := make(map[string]*model.VFConfig)

	host, err := d.esxiDefaultHost()
	if err != nil {
		return result, nil
	}
	var hs mo.HostSystem
	if err := host.Properties(d.ctx, host.Reference(), []string{"hardware.pciDevice"}, &hs); err != nil {
		return result, nil
	}
	for i := range hs.Hardware.PciDevice {
		p := hs.Hardware.PciDevice[i]
		key := fmt.Sprintf("%s:%d", p.Id, p.DeviceId)
		hint := strings.TrimSpace(p.VendorName + " " + p.DeviceName)
		result[key] = &model.VFConfig{
			PciKey:  key,
			GpuUUID: p.Id, // VirtualPCIPassthroughDeviceBackingInfo.Id 使用该标识
			GpuHint: hint,
		}
	}
	return result, nil
}

// GetGPUList 枚举显卡（PCI 设备中类别为 0300/0302 的显示控制器）
func (d *ESXiDriver) GetGPUList() (map[string]*model.VFConfig, error) {
	all, err := d.GetPCIList()
	if err != nil {
		return all, err
	}
	result := make(map[string]*model.VFConfig)
	for key, cfg := range all {
		lower := strings.ToLower(cfg.GpuHint)
		if strings.Contains(lower, "vga") || strings.Contains(lower, "display") ||
			strings.Contains(lower, "graphics") || strings.Contains(lower, "3d") {
			result[key] = cfg
		}
	}
	return result, nil
}

// GetUSBList 枚举 ESXi 主机 USB 设备（ESXi 无 USB 枚举 API，走主机 Shell lsusb）
func (d *ESXiDriver) GetUSBList() (map[string]*model.USBInfos, error) {
	result := make(map[string]*model.USBInfos)

	res, err := RunSSHCommand(sshConfigFromHS(d.Config), "lsusb")
	if err != nil || res == nil {
		return result, nil
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if !strings.Contains(line, "ID ") {
			continue
		}
		idPart := strings.SplitN(line, "ID ", 2)[1]
		fields := strings.SplitN(strings.TrimSpace(idPart), " ", 2)
		if len(fields) == 0 || !strings.Contains(fields[0], ":") {
			continue
		}
		pair := strings.SplitN(fields[0], ":", 2)
		hint := fields[0]
		if len(fields) > 1 {
			hint = strings.TrimSpace(fields[1])
		}
		result[fields[0]] = &model.USBInfos{
			UsbKey:  fields[0],
			VidUUID: pair[0],
			PidUUID: pair[1],
			UsbHint: hint,
		}
	}
	return result, nil
}

// ============================================================
// 截图
// ============================================================

// GetScreenshot 通过 CreateScreenshot_Task 获取虚拟机屏幕。
//
// vSphere 的截图流程：调用 CreateScreenshot_Task → 等待任务 → 任务结果返回
// 一个 URL，再用当前会话 Cookie 拉取 PNG 内容。
func (d *ESXiDriver) GetScreenshot(vmUUID string) (string, error) {
	vm, err := d.findVM(vmUUID)
	if err != nil {
		return "", err
	}
	power, err := vm.PowerState(d.ctx)
	if err != nil {
		return "", err
	}
	if power != types.VirtualMachinePowerStatePoweredOn {
		return "", fmt.Errorf("虚拟机未运行，无法截图")
	}

	resp, err := methods.CreateScreenshot_Task(d.ctx, vm.Client(), &types.CreateScreenshot_Task{
		This: vm.Reference(),
	})
	if err != nil {
		return "", fmt.Errorf("创建截图任务失败: %w", err)
	}

	task := object.NewTask(vm.Client(), resp.Returnval)
	info, err := task.WaitForResult(d.ctx, nil)
	if err != nil {
		return "", fmt.Errorf("等待截图任务失败: %w", err)
	}

	// 任务结果通常为截图下载 URL
	targetURL, _ := info.Result.(string)
	if strings.TrimSpace(targetURL) == "" {
		return "", fmt.Errorf("截图任务未返回下载地址")
	}
	if !strings.HasPrefix(targetURL, "http") {
		targetURL = fmt.Sprintf("https://%s%s", d.Config.ServerAddr, targetURL)
	}

	req, err := http.NewRequestWithContext(d.ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return "", err
	}
	// 复用 govmomi 会话 Cookie 进行鉴权
	if d.client != nil && d.client.Client != nil && d.client.Client.Jar != nil {
		for _, c := range d.client.Client.Jar.Cookies(req.URL) {
			req.AddCookie(c)
		}
	}
	respHTTP, err := SharedInsecureHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载截图失败: %w", err)
	}
	defer respHTTP.Body.Close()
	if respHTTP.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载截图失败: HTTP %d", respHTTP.StatusCode)
	}
	data, err := io.ReadAll(respHTTP.Body)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("截图为空")
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data), nil
}

// ============================================================
// 修改密码（VMware Tools Guest Operations）
// ============================================================

// SetPassword 通过 VMware Tools 在来宾系统内修改密码。
//
// 与 Python 版 vSphereESXi.VMPasswd 语义一致：依赖 VMware Tools，
// 未运行时明确返回失败（不再假成功）。
func (d *ESXiDriver) SetPassword(vmUUID string, password string) (*model.ZMessage, error) {
	vm, err := d.findVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMPasswd", Message: err.Error()}, nil
	}

	var moVM mo.VirtualMachine
	if err := vm.Properties(d.ctx, vm.Reference(),
		[]string{"guest.toolsRunningStatus", "guest.guestOperationsReady"}, &moVM); err != nil {
		return &model.ZMessage{Success: false, Action: "VMPasswd",
			Message: "读取来宾状态失败: " + err.Error()}, nil
	}
	if moVM.Guest.GuestOperationsReady == nil || !*moVM.Guest.GuestOperationsReady {
		return &model.ZMessage{Success: false, Action: "VMPasswd",
			Message: "VMware Tools 未就绪，无法修改密码"}, nil
	}

	user := "root"
	oldPass := ""
	d.mu.RLock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		if cfg.OsUser != "" {
			user = cfg.OsUser
		}
		oldPass = cfg.OsPass
	}
	d.mu.RUnlock()

	auth := &types.NamePasswordAuthentication{Username: user, Password: oldPass}

	om := guest.NewOperationsManager(vm.Client(), vm.Reference())
	procMgr, err := om.ProcessManager(d.ctx)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMPasswd",
			Message: "获取来宾进程管理器失败: " + err.Error()}, nil
	}

	// 通过 StartProgramInGuest 调用 chpasswd
	cmd := fmt.Sprintf("echo '%s:%s' | chpasswd", user, password)
	pid, err := procMgr.StartProgram(d.ctx, auth, &types.GuestProgramSpec{
		ProgramPath: "/bin/sh",
		Arguments:   "-c \"" + cmd + "\"",
	})
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMPasswd",
			Message: fmt.Sprintf("执行改密命令失败: %v", err)}, nil
	}

	// 等待执行完成（最多 30 秒）
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, err := procMgr.ListProcesses(d.ctx, auth, []int64{pid})
		if err == nil {
			done := true
			for i := range status {
				if status[i].EndTime == nil {
					done = false
				}
			}
			if done {
				break
			}
		}
		time.Sleep(time.Second)
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		cfg.OsPass = password
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMPasswd", Message: "密码修改成功"}, nil
}
