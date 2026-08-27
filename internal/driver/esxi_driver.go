// Package driver ESXi/vSphere 驱动实现
package driver

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/find"
	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// ESXiDriver VMware ESXi/vSphere 驱动
type ESXiDriver struct {
	*BaseDriver
	client     *govmomi.Client
	ctx        context.Context
	datacenter *object.Datacenter
	finder     *find.Finder
}

// NewESXiDriver 创建ESXi驱动实例
func NewESXiDriver(cfg *model.HSConfig) *ESXiDriver {
	return &ESXiDriver{
		BaseDriver: NewBaseDriver(cfg),
		ctx:        context.Background(),
	}
}

// apiConn 连接到vCenter/ESXi
func (d *ESXiDriver) apiConn() error {
	if d.client != nil {
		return nil
	}

	u, err := url.Parse(fmt.Sprintf("https://%s/sdk", d.Config.ServerAddr))
	if err != nil {
		return err
	}
	u.User = url.UserPassword(d.Config.ServerUser, d.Config.ServerPass)

	ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
	defer cancel()

	client, err := govmomi.NewClient(ctx, u, true)
	if err != nil {
		return fmt.Errorf("连接vCenter失败: %w", err)
	}

	d.client = client
	d.finder = find.NewFinder(client.Client, true)

	// 获取默认数据中心
	dc, err := d.finder.DefaultDatacenter(d.ctx)
	if err != nil {
		return fmt.Errorf("获取数据中心失败: %w", err)
	}
	d.datacenter = dc
	d.finder.SetDatacenter(dc)

	return nil
}

// findVM 查找虚拟机对象
func (d *ESXiDriver) findVM(vmUUID string) (*object.VirtualMachine, error) {
	if err := d.apiConn(); err != nil {
		return nil, err
	}

	vm, err := d.finder.VirtualMachine(d.ctx, vmUUID)
	if err != nil {
		return nil, fmt.Errorf("未找到虚拟机: %w", err)
	}

	return vm, nil
}

// GetPower 获取虚拟机电源状态（返回中文字符串）
func (d *ESXiDriver) GetPower(vmUUID string) string {
	vm, err := d.findVM(vmUUID)
	if err != nil {
		return "未知"
	}

	powerState, err := vm.PowerState(d.ctx)
	if err != nil {
		return "未知"
	}

	switch powerState {
	case types.VirtualMachinePowerStatePoweredOn:
		return "运行中"
	case types.VirtualMachinePowerStatePoweredOff:
		return "已关机"
	case types.VirtualMachinePowerStateSuspended:
		return "已暂停"
	default:
		return "未知"
	}
}

// CreateVM 创建虚拟机
func (d *ESXiDriver) CreateVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	if err := d.apiConn(); err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMCreate",
			Message: fmt.Sprintf("连接失败: %v", err),
		}, nil
	}

	// 获取资源池和数据存储
	pool, err := d.finder.DefaultResourcePool(d.ctx)
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMCreate",
			Message: fmt.Sprintf("获取资源池失败: %v", err),
		}, nil
	}

	ds, err := d.finder.DefaultDatastore(d.ctx)
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMCreate",
			Message: fmt.Sprintf("获取数据存储失败: %v", err),
		}, nil
	}

	// 构建虚拟机配置规范
	spec := types.VirtualMachineConfigSpec{
		Name:     cfg.VmUUID,
		GuestId:  "otherLinux64Guest",
		NumCPUs:  int32(cfg.CpuNum),
		MemoryMB: int64(cfg.MemNum),
		Files: &types.VirtualMachineFileInfo{
			VmPathName: fmt.Sprintf("[%s]", ds.Name()),
		},
	}

	// 创建虚拟机目录
	folder, err := d.finder.DefaultFolder(d.ctx)
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMCreate",
			Message: fmt.Sprintf("获取VM目录失败: %v", err),
		}, nil
	}

	// 创建虚拟机任务
	task, err := folder.CreateVM(d.ctx, spec, pool, nil)
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMCreate",
			Message: fmt.Sprintf("创建VM任务失败: %v", err),
		}, nil
	}

	// 等待任务完成
	info, err := task.WaitForResult(d.ctx, nil)
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMCreate",
			Message: fmt.Sprintf("创建VM失败: %v", err),
		}, nil
	}

	vmRef := info.Result.(types.ManagedObjectReference)

	// 保存虚拟机配置到内存
	d.mu.Lock()
	cfg.VmFlag = model.VMPowerStopped
	if cfg.ExtData == nil {
		cfg.ExtData = make(map[string]interface{})
	}
	cfg.ExtData["vmRef"] = vmRef.Value
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{
		Success: true,
		Action:  "VMCreate",
		Message: "虚拟机创建成功",
		Results: map[string]interface{}{
			"vm_uuid": cfg.VmUUID,
			"vm_ref":  vmRef.Value,
		},
	}, nil
}

// DeleteVM 删除虚拟机
func (d *ESXiDriver) DeleteVM(vmUUID string, removeBackup bool) (*model.ZMessage, error) {
	vm, err := d.findVM(vmUUID)
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMDelete",
			Message: fmt.Sprintf("查找虚拟机失败: %v", err),
		}, nil
	}

	// 如果虚拟机正在运行，先关机
	powerState, err := vm.PowerState(d.ctx)
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMDelete",
			Message: fmt.Sprintf("获取电源状态失败: %v", err),
		}, nil
	}

	if powerState == types.VirtualMachinePowerStatePoweredOn {
		task, err := vm.PowerOff(d.ctx)
		if err != nil {
			return &model.ZMessage{
				Success: false,
				Action:  "VMDelete",
				Message: fmt.Sprintf("关机失败: %v", err),
			}, nil
		}
		if err := task.Wait(d.ctx); err != nil {
			return &model.ZMessage{
				Success: false,
				Action:  "VMDelete",
				Message: fmt.Sprintf("等待关机失败: %v", err),
			}, nil
		}
	}

	// 删除虚拟机
	task, err := vm.Destroy(d.ctx)
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMDelete",
			Message: fmt.Sprintf("删除失败: %v", err),
		}, nil
	}

	if err := task.Wait(d.ctx); err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMDelete",
			Message: fmt.Sprintf("等待删除失败: %v", err),
		}, nil
	}

	// 从内存中移除
	d.mu.Lock()
	delete(d.VmSaving, vmUUID)
	d.mu.Unlock()

	return &model.ZMessage{
		Success: true,
		Action:  "VMDelete",
		Message: "虚拟机删除成功",
	}, nil
}

// PowerControl 电源控制
func (d *ESXiDriver) PowerControl(vmUUID string, action model.VMPowers) (*model.ZMessage, error) {
	vm, err := d.findVM(vmUUID)
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMPowers",
			Message: fmt.Sprintf("查找虚拟机失败: %v", err),
		}, nil
	}

	var task *object.Task
	var actionName string
	switch action {
	case model.VMPowerSStart:
		task, err = vm.PowerOn(d.ctx)
		actionName = "启动"
	case model.VMPowerSClose:
		err = vm.ShutdownGuest(d.ctx)
		actionName = "软关机"
	case model.VMPowerHClose:
		task, err = vm.PowerOff(d.ctx)
		actionName = "强制关机"
	case model.VMPowerSReset:
		err = vm.RebootGuest(d.ctx)
		actionName = "软重启"
	case model.VMPowerHReset:
		task, err = vm.Reset(d.ctx)
		actionName = "强制重启"
	case model.VMPowerAPause:
		task, err = vm.Suspend(d.ctx)
		actionName = "暂停"
	default:
		return &model.ZMessage{
			Success: false,
			Action:  "VMPowers",
			Message: fmt.Sprintf("未知的电源操作: %s", action),
		}, nil
	}

	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMPowers",
			Message: fmt.Sprintf("%s失败: %v", actionName, err),
		}, nil
	}

	if task != nil {
		if err := task.Wait(d.ctx); err != nil {
			return &model.ZMessage{
				Success: false,
				Action:  "VMPowers",
				Message: fmt.Sprintf("等待%s失败: %v", actionName, err),
			}, nil
		}
	}

	// 更新内存中的状态
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		if newState, exists := PowerActionMap[action]; exists {
			cfg.VmFlag = newState
		}
	}
	d.mu.Unlock()

	return &model.ZMessage{
		Success: true,
		Action:  "VMPowers",
		Message: fmt.Sprintf("%s成功", actionName),
	}, nil
}

// GetStatus 获取主机状态
func (d *ESXiDriver) GetStatus() (*model.HWStatus, error) {
	if err := d.apiConn(); err != nil {
		return &model.HWStatus{OnUpdate: time.Now().Unix()}, nil
	}

	// 获取主机系统信息
	host, err := d.finder.DefaultHostSystem(d.ctx)
	if err != nil {
		return &model.HWStatus{OnUpdate: time.Now().Unix()}, nil
	}

	var hs mo.HostSystem
	err = host.Properties(d.ctx, host.Reference(), []string{
		"summary.hardware",
		"summary.quickStats",
		"summary.config",
	}, &hs)
	if err != nil {
		return &model.HWStatus{OnUpdate: time.Now().Unix()}, nil
	}

	hw := hs.Summary.Hardware
	qs := hs.Summary.QuickStats

	cpuUsagePercent := 0
	if hw.CpuMhz > 0 && hw.NumCpuCores > 0 {
		totalMhz := int64(hw.CpuMhz) * int64(hw.NumCpuCores)
		if totalMhz > 0 {
			cpuUsagePercent = int((int64(qs.OverallCpuUsage) * 100) / totalMhz)
		}
	}

	return &model.HWStatus{
		CpuModel: hw.CpuModel,
		CpuTotal: int(hw.NumCpuCores),
		CpuUsage: cpuUsagePercent,
		MemTotal: int(hw.MemorySize / 1024 / 1024),
		MemUsage: int(qs.OverallMemoryUsage),
		VmState:  "运行中",
		OnUpdate: time.Now().Unix(),
	}, nil
}

// GetVMList 获取虚拟机列表
func (d *ESXiDriver) GetVMList() (map[string]*model.VMConfig, error) {
	if err := d.apiConn(); err != nil {
		return d.BaseDriver.GetVMList()
	}

	vms, err := d.finder.VirtualMachineList(d.ctx, "*")
	if err != nil {
		return d.BaseDriver.GetVMList()
	}

	result := d.GetVMSaving()
	for _, vm := range vms {
		var vmObj mo.VirtualMachine
		err := vm.Properties(d.ctx, vm.Reference(), []string{
			"name",
			"config.uuid",
			"runtime.powerState",
			"config.hardware.numCPU",
			"config.hardware.memoryMB",
		}, &vmObj)
		if err != nil {
			continue
		}

		vmUUID := vmObj.Name // 使用名称作为UUID
		if vmObj.Config != nil && vmObj.Config.Uuid != "" {
			vmUUID = vmObj.Config.Uuid
		}

		var powerState model.VMPowers
		switch vmObj.Runtime.PowerState {
		case types.VirtualMachinePowerStatePoweredOn:
			powerState = model.VMPowerStarted
		case types.VirtualMachinePowerStatePoweredOff:
			powerState = model.VMPowerStopped
		case types.VirtualMachinePowerStateSuspended:
			powerState = model.VMPowerSuspend
		default:
			powerState = model.VMPowerUnknown
		}

		if cfg, ok := result[vmUUID]; ok {
			// 更新已存在的配置
			cfg.VmFlag = powerState
			if vmObj.Config != nil {
				cfg.CpuNum = int(vmObj.Config.Hardware.NumCPU)
				cfg.MemNum = int(vmObj.Config.Hardware.MemoryMB)
			}
		} else {
			// 创建新配置
			cfg := model.NewVMConfig()
			cfg.VmUUID = vmUUID
			cfg.OsName = vmObj.Name
			cfg.VmFlag = powerState
			if vmObj.Config != nil {
				cfg.CpuNum = int(vmObj.Config.Hardware.NumCPU)
				cfg.MemNum = int(vmObj.Config.Hardware.MemoryMB)
			}
			if cfg.ExtData == nil {
				cfg.ExtData = make(map[string]interface{})
			}
			cfg.ExtData["vmRef"] = vm.Reference().Value
			result[vmUUID] = cfg
		}
	}

	return result, nil
}

// ReinstallVM 重装虚拟机系统
func (d *ESXiDriver) ReinstallVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	return &model.ZMessage{
		Success: false,
		Action:  "VMReinstall",
		Message: "ESXi平台暂不支持系统重装",
	}, nil
}

// UpdateVM 更新虚拟机配置
func (d *ESXiDriver) UpdateVM(cfg *model.VMConfig, oldCfg *model.VMConfig) (*model.ZMessage, error) {
	vm, err := d.findVM(cfg.VmUUID)
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMUpdate",
			Message: fmt.Sprintf("查找虚拟机失败: %v", err),
		}, nil
	}

	// 检查虚拟机是否关机
	powerState, _ := vm.PowerState(d.ctx)
	if powerState != types.VirtualMachinePowerStatePoweredOff {
		return &model.ZMessage{
			Success: false,
			Action:  "VMUpdate",
			Message: "虚拟机必须关机后才能修改配置",
		}, nil
	}

	// 构建配置变更规范
	spec := types.VirtualMachineConfigSpec{}
	if cfg.CpuNum != oldCfg.CpuNum {
		spec.NumCPUs = int32(cfg.CpuNum)
	}
	if cfg.MemNum != oldCfg.MemNum {
		spec.MemoryMB = int64(cfg.MemNum)
	}

	task, err := vm.Reconfigure(d.ctx, spec)
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMUpdate",
			Message: fmt.Sprintf("配置更新失败: %v", err),
		}, nil
	}

	if err := task.Wait(d.ctx); err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMUpdate",
			Message: fmt.Sprintf("等待配置更新失败: %v", err),
		}, nil
	}

	// 更新内存配置
	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{
		Success: true,
		Action:  "VMUpdate",
		Message: "虚拟机配置更新成功",
	}, nil
}

// GetVMDetail 获取虚拟机详情
func (d *ESXiDriver) GetVMDetail(vmUUID string) (*model.VMConfig, error) {
	d.mu.RLock()
	cfg, ok := d.VmSaving[vmUUID]
	d.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("虚拟机 %s 不存在", vmUUID)
	}
	return cfg, nil
}

// ScanVMs 扫描并同步虚拟机列表
func (d *ESXiDriver) ScanVMs() (*model.ZMessage, error) {
	vms, err := d.GetVMList()
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMDetect",
			Message: fmt.Sprintf("扫描失败: %v", err),
		}, nil
	}

	d.mu.Lock()
	d.VmSaving = vms
	d.mu.Unlock()

	return &model.ZMessage{
		Success: true,
		Action:  "VMDetect",
		Message: fmt.Sprintf("扫描完成，共发现 %d 台虚拟机", len(vms)),
	}, nil
}

// CreateBackup 创建备份（基于快照实现）
func (d *ESXiDriver) CreateBackup(vmUUID string, tips string) (*model.ZMessage, error) {
	vm, err := d.findVM(vmUUID)
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMBackup",
			Message: fmt.Sprintf("查找虚拟机失败: %v", err),
		}, nil
	}

	snapshotName := fmt.Sprintf("backup_%d", time.Now().Unix())
	task, err := vm.CreateSnapshot(d.ctx, snapshotName, tips, false, false)
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMBackup",
			Message: fmt.Sprintf("创建快照失败: %v", err),
		}, nil
	}

	if err := task.Wait(d.ctx); err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VMBackup",
			Message: fmt.Sprintf("等待快照创建失败: %v", err),
		}, nil
	}

	// 保存备份记录到虚拟机配置
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		cfg.Backups = append(cfg.Backups, model.VMBackup{
			BackupName: snapshotName,
			BackupTime: time.Now(),
			BackupTips: tips,
		})
	}
	d.mu.Unlock()

	return &model.ZMessage{
		Success: true,
		Action:  "VMBackup",
		Message: "备份创建成功",
		Results: map[string]interface{}{
			"backup_name": snapshotName,
		},
	}, nil
}

// RestoreBackup 还原备份
func (d *ESXiDriver) RestoreBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	return &model.ZMessage{
		Success: false,
		Action:  "Restores",
		Message: "ESXi平台暂不支持快照还原",
	}, nil
}

// DeleteBackup 删除备份
func (d *ESXiDriver) DeleteBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	return &model.ZMessage{
		Success: false,
		Action:  "RMBackup",
		Message: "ESXi平台暂不支持快照删除",
	}, nil
}

// GetVNCInfo 获取VNC信息（ESXi使用VMRC）
func (d *ESXiDriver) GetVNCInfo(vmUUID string, ipAddr string) (*model.ZMessage, error) {
	vm, err := d.findVM(vmUUID)
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VCRemote",
			Message: fmt.Sprintf("查找虚拟机失败: %v", err),
		}, nil
	}

	// 获取VMRC票据
	ticket, err := vm.AcquireTicket(d.ctx, "webmks")
	if err != nil {
		return &model.ZMessage{
			Success: false,
			Action:  "VCRemote",
			Message: fmt.Sprintf("获取票据失败: %v", err),
		}, nil
	}

	return &model.ZMessage{
		Success: true,
		Action:  "VCRemote",
		Message: "VNC信息获取成功",
		Results: map[string]interface{}{
			"type":     "vmrc",
			"host":     ticket.Host,
			"port":     ticket.Port,
			"ticket":   ticket.Ticket,
			"password": ticket.Ticket,
		},
	}, nil
}

// GetScreenshot 获取虚拟机截图
func (d *ESXiDriver) GetScreenshot(vmUUID string) (string, error) {
	// ESXi平台暂不支持直接获取截图
	return "", fmt.Errorf("ESXi平台暂不支持截图功能")
}

// Crontab 定时任务（状态同步）
func (d *ESXiDriver) Crontab() bool {
	_, err := d.GetVMList()
	return err == nil
}
