// Package driver Proxmox VE 驱动
// 使用 github.com/luthermonson/go-proxmox 库
package driver

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	proxmox "github.com/luthermonson/go-proxmox"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// insecureHTTPClient 返回跳过TLS验证的HTTP客户端
func insecureHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
}

// ProxmoxDriver Proxmox VE 驱动
type ProxmoxDriver struct {
	*BaseDriver
	client   *proxmox.Client
	clientMu sync.Mutex
}

// NewProxmoxDriver 创建Proxmox驱动
func NewProxmoxDriver(cfg *model.HSConfig) *ProxmoxDriver {
	return &ProxmoxDriver{BaseDriver: NewBaseDriver(cfg)}
}

// ============================================================
// 连接管理
// ============================================================

// apiConn 获取Proxmox API连接（懒加载+复用）
func (d *ProxmoxDriver) apiConn() (*proxmox.Client, error) {
	d.clientMu.Lock()
	defer d.clientMu.Unlock()

	if d.client != nil {
		// 测试连接是否仍然有效
		_, err := d.client.Version(context.Background())
		if err == nil {
			return d.client, nil
		}
		d.client = nil
	}

	host := d.Config.ServerAddr
	if !strings.Contains(host, ":") {
		host = host + ":8006"
	}
	user := d.Config.ServerUser
	if user == "" {
		user = "root"
	}
	if !strings.Contains(user, "@") {
		user = user + "@pam"
	}

	client := proxmox.NewClient(
		"https://"+host,
		proxmox.WithCredentials(&proxmox.Credentials{
			Username: user,
			Password: d.Config.ServerPass,
		}),
		proxmox.WithHTTPClient(insecureHTTPClient()),
	)

	// 测试连接
	if _, err := client.Version(context.Background()); err != nil {
		return nil, fmt.Errorf("连接Proxmox失败: %w", err)
	}

	d.client = client
	return d.client, nil
}

// nodeName 获取PVE节点名
func (d *ProxmoxDriver) nodeName() string {
	if d.Config.LaunchPath != "" {
		return d.Config.LaunchPath
	}
	return "pve"
}

// getNode 获取节点对象
func (d *ProxmoxDriver) getNode() (*proxmox.Node, error) {
	client, err := d.apiConn()
	if err != nil {
		return nil, err
	}
	node, err := client.Node(context.Background(), d.nodeName())
	if err != nil {
		return nil, fmt.Errorf("获取节点失败: %w", err)
	}
	return node, nil
}

// getVMID 根据vm_uuid查找VMID（先查缓存，再查API）
func (d *ProxmoxDriver) getVMID(vmUUID string) (int64, error) {
	d.mu.RLock()
	cfg, ok := d.VmSaving[vmUUID]
	d.mu.RUnlock()

	if ok {
		if vmData, ok2 := cfg.ExtData["vmid"]; ok2 {
			if id, ok3 := vmData.(float64); ok3 && id > 0 {
				return int64(id), nil
			}
		}
	}

	// 从API查找
	node, err := d.getNode()
	if err != nil {
		return 0, err
	}

	vms, err := node.VirtualMachines(context.Background())
	if err != nil {
		return 0, fmt.Errorf("获取VM列表失败: %w", err)
	}

	// Proxmox中vm_uuid对应VM名称（下划线转横线）
	vmName := strings.ReplaceAll(vmUUID, "_", "-")
	for _, vm := range vms {
		if vm.Name == vmName {
			// 缓存VMID
			d.mu.Lock()
			if cfg, ok := d.VmSaving[vmUUID]; ok {
				if cfg.ExtData == nil {
					cfg.ExtData = make(map[string]interface{})
				}
				cfg.ExtData["vmid"] = float64(vm.VMID)
			}
			d.mu.Unlock()
			return int64(vm.VMID), nil
		}
	}
	return 0, fmt.Errorf("未找到虚拟机 %s 的VMID", vmUUID)
}

// getVM 获取VM对象
func (d *ProxmoxDriver) getVM(vmUUID string) (*proxmox.VirtualMachine, error) {
	node, err := d.getNode()
	if err != nil {
		return nil, err
	}
	vmid, err := d.getVMID(vmUUID)
	if err != nil {
		return nil, err
	}
	vm, err := node.VirtualMachine(context.Background(), int(vmid))
	if err != nil {
		return nil, fmt.Errorf("获取VM对象失败: %w", err)
	}
	return vm, nil
}

// newVMID 分配新的VMID
func (d *ProxmoxDriver) newVMID() (int, error) {
	node, err := d.getNode()
	if err != nil {
		return 100, nil
	}
	vms, err := node.VirtualMachines(context.Background())
	if err != nil {
		return 100, nil
	}
	used := make(map[int]bool)
	for _, vm := range vms {
		used[int(vm.VMID)] = true
	}
	for id := 100; id < 9999; id++ {
		if !used[id] {
			return id, nil
		}
	}
	return 100, nil
}

// ============================================================
// 电源状态
// ============================================================

// GetPower 获取虚拟机电源状态
func (d *ProxmoxDriver) GetPower(vmUUID string) string {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return "未知"
	}
	switch vm.Status {
	case "running":
		return "运行中"
	case "stopped":
		return "已关机"
	case "paused":
		return "已暂停"
	default:
		return "未知"
	}
}

// GetStatus 获取主机状态
func (d *ProxmoxDriver) GetStatus() (*model.HWStatus, error) {
	node, err := d.getNode()
	if err != nil {
		return d.BaseDriver.GetStatus()
	}

	if err := node.Status(context.Background()); err != nil {
		return d.BaseDriver.GetStatus()
	}

	hw := &model.HWStatus{
		OnUpdate: time.Now().Unix(),
	}

	if node.CPU != 0 {
		hw.CpuUsage = int(node.CPU * 100)
	}
	if node.Memory.Total > 0 {
		hw.MemTotal = int(node.Memory.Total / 1024 / 1024)
		hw.MemUsage = int(node.Memory.Used / 1024 / 1024)
	}
	if node.RootFS.Total > 0 {
		hw.HddTotal = int(node.RootFS.Total / 1024 / 1024)
		hw.HddUsage = int(node.RootFS.Used / 1024 / 1024)
	}
	hw.CpuTotal = int(node.CPUInfo.CPUs)
	hw.CpuModel = node.CPUInfo.Model

	return hw, nil
}

// ============================================================
// 虚拟机生命周期
// ============================================================

// ScanVMs 扫描并同步虚拟机列表
func (d *ProxmoxDriver) ScanVMs() (*model.ZMessage, error) {
	node, err := d.getNode()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMDetect", Message: err.Error()}, nil
	}

	vms, err := node.VirtualMachines(context.Background())
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMDetect", Message: err.Error()}, nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	for _, vm := range vms {
		vmUUID := strings.ReplaceAll(vm.Name, "-", "_")
		if _, exists := d.VmSaving[vmUUID]; !exists {
			cfg := model.NewVMConfig()
			cfg.VmUUID = vmUUID
			if cfg.ExtData == nil {
				cfg.ExtData = make(map[string]interface{})
			}
			cfg.ExtData["vmid"] = float64(vm.VMID)
			d.VmSaving[vmUUID] = cfg
		} else {
			if d.VmSaving[vmUUID].ExtData == nil {
				d.VmSaving[vmUUID].ExtData = make(map[string]interface{})
			}
			d.VmSaving[vmUUID].ExtData["vmid"] = float64(vm.VMID)
		}
	}

	return &model.ZMessage{
		Success: true, Action: "VMDetect",
		Message: fmt.Sprintf("扫描完成，共发现 %d 台虚拟机", len(vms)),
	}, nil
}

// CreateVM 创建虚拟机
func (d *ProxmoxDriver) CreateVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	node, err := d.getNode()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate", Message: err.Error()}, nil
	}

	vmid, err := d.newVMID()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate", Message: err.Error()}, nil
	}

	vmName := strings.ReplaceAll(cfg.VmUUID, "_", "-")
	memMB := cfg.MemNum
	if memMB <= 0 {
		memMB = 512
	}
	cpuNum := cfg.CpuNum
	if cpuNum <= 0 {
		cpuNum = 1
	}
	hddGB := cfg.HddNum / 1024
	if hddGB <= 0 {
		hddGB = 8
	}

	// 获取存储名称
	storage := "local-lvm"
	if d.Config.SystemPath != "" {
		storage = d.Config.SystemPath
	}

	task, err := node.NewVirtualMachine(context.Background(), vmid, proxmox.VirtualMachineOption{
		Name:  "name",
		Value: vmName,
	}, proxmox.VirtualMachineOption{
		Name:  "memory",
		Value: strconv.Itoa(memMB),
	}, proxmox.VirtualMachineOption{
		Name:  "cores",
		Value: strconv.Itoa(cpuNum),
	}, proxmox.VirtualMachineOption{
		Name:  "scsi0",
		Value: fmt.Sprintf("%s:%d,format=qcow2", storage, hddGB),
	}, proxmox.VirtualMachineOption{
		Name:  "net0",
		Value: "virtio,bridge=vmbr0",
	}, proxmox.VirtualMachineOption{
		Name:  "ostype",
		Value: "l26",
	}, proxmox.VirtualMachineOption{
		Name:  "scsihw",
		Value: "virtio-scsi-pci",
	})
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate", Message: err.Error()}, nil
	}

	if err := task.WaitFor(context.Background(), 60); err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate", Message: "等待创建任务超时: " + err.Error()}, nil
	}

	// 缓存VMID
	if cfg.ExtData == nil {
		cfg.ExtData = make(map[string]interface{})
	}
	cfg.ExtData["vmid"] = float64(vmid)

	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMCreate", Message: "虚拟机创建成功"}, nil
}

// UpdateVM 更新虚拟机配置
func (d *ProxmoxDriver) UpdateVM(cfg *model.VMConfig, oldCfg *model.VMConfig) (*model.ZMessage, error) {
	vm, err := d.getVM(cfg.VmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMUpdate", Message: err.Error()}, nil
	}

	var opts []proxmox.VirtualMachineOption

	if cfg.CpuNum > 0 && cfg.CpuNum != oldCfg.CpuNum {
		opts = append(opts, proxmox.VirtualMachineOption{Name: "cores", Value: strconv.Itoa(cfg.CpuNum)})
	}
	if cfg.MemNum > 0 && cfg.MemNum != oldCfg.MemNum {
		opts = append(opts, proxmox.VirtualMachineOption{Name: "memory", Value: strconv.Itoa(cfg.MemNum)})
	}

	if len(opts) > 0 {
		if _, err := vm.Config(context.Background(), opts...); err != nil {
			return &model.ZMessage{Success: false, Action: "VMUpdate", Message: err.Error()}, nil
		}
	}

	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMUpdate", Message: "虚拟机配置已更新"}, nil
}

// DeleteVM 删除虚拟机
func (d *ProxmoxDriver) DeleteVM(vmUUID string, removeBackup bool) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMDelete", Message: err.Error()}, nil
	}

	// 先关机
	if vm.Status == "running" {
		task, err := vm.Stop(context.Background())
		if err == nil {
			_ = task.WaitFor(context.Background(), 30)
		}
	}

	task, err := vm.Delete(context.Background())
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMDelete", Message: err.Error()}, nil
	}

	if err := task.WaitFor(context.Background(), 60); err != nil {
		return &model.ZMessage{Success: false, Action: "VMDelete", Message: "等待删除任务超时: " + err.Error()}, nil
	}

	d.mu.Lock()
	delete(d.VmSaving, vmUUID)
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDelete", Message: "虚拟机已删除"}, nil
}

// PowerControl 电源控制
func (d *ProxmoxDriver) PowerControl(vmUUID string, action model.VMPowers) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMPowers", Message: err.Error()}, nil
	}

	var task *proxmox.Task
	var taskErr error

	switch action {
	case model.VMPowerSStart:
		task, taskErr = vm.Start(context.Background())
	case model.VMPowerSClose:
		task, taskErr = vm.Shutdown(context.Background())
	case model.VMPowerHClose:
		task, taskErr = vm.Stop(context.Background())
	case model.VMPowerSReset, model.VMPowerHReset:
		task, taskErr = vm.Reset(context.Background())
	case model.VMPowerAPause:
		task, taskErr = vm.Pause(context.Background())
	case model.VMPowerAWaked:
		task, taskErr = vm.Resume(context.Background())
	default:
		return &model.ZMessage{Success: false, Action: "VMPowers", Message: "不支持的电源操作"}, nil
	}

	if taskErr != nil {
		return &model.ZMessage{Success: false, Action: "VMPowers", Message: taskErr.Error()}, nil
	}

	if task != nil {
		if err := task.WaitFor(context.Background(), 30); err != nil {
			return &model.ZMessage{Success: false, Action: "VMPowers", Message: "等待电源操作超时"}, nil
		}
	}

	// 更新内存状态
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		if newState, ok2 := PowerActionMap[action]; ok2 {
			cfg.VmFlag = newState
		}
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMPowers", Message: "电源操作已执行"}, nil
}

// ============================================================
// VNC / 远程控制
// ============================================================

// GetVNCInfo 获取VNC连接信息
func (d *ProxmoxDriver) GetVNCInfo(vmUUID string, ipAddr string) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VCRemote", Message: err.Error()}, nil
	}

	vnc, err := vm.TermProxy(context.Background())
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VCRemote", Message: err.Error()}, nil
	}

	hostAddr := d.Config.ServerAddr
	if !strings.Contains(hostAddr, ":") {
		hostAddr = hostAddr + ":8006"
	}

	return &model.ZMessage{
		Success: true,
		Action:  "VCRemote",
		Message: "VNC信息获取成功",
		Results: map[string]interface{}{
			"type":     "proxmox",
			"host":     hostAddr,
			"ticket":   vnc.Ticket,
			"port":     vnc.Port,
			"password": vnc.Ticket,
		},
	}, nil
}

// GetScreenshot 获取虚拟机截图
func (d *ProxmoxDriver) GetScreenshot(vmUUID string) (string, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return "", fmt.Errorf("获取虚拟机失败: %w", err)
	}

	// 检查虚拟机是否运行
	if vm.Status != "running" {
		return "", fmt.Errorf("虚拟机未运行，无法截图")
	}

	// 使用 Proxmox 的 vncproxy 和截图 API
	// Proxmox 7.0+ 支持通过 spiceproxy 获取截图
	// 我们使用 HTTP API 直接获取截图
	client, err := d.apiConn()
	if err != nil {
		return "", fmt.Errorf("连接 Proxmox API 失败: %w", err)
	}

	// 构建截图 API 路径
	// GET /api2/json/nodes/{node}/qemu/{vmid}/agent/exec-status
	// 或使用 vncproxy 方式获取截图
	nodeName := d.nodeName()
	vmid, err := d.getVMID(vmUUID)
	if err != nil {
		return "", fmt.Errorf("获取 VM ID 失败: %w", err)
	}

	// 方案1: 尝试使用 QEMU Guest Agent 截图（需要虚拟机安装 qemu-guest-agent）
	screenshotPath := fmt.Sprintf("/nodes/%s/qemu/%d/agent/exec", nodeName, vmid)
	screenshotCmd := map[string]interface{}{
		"command": []string{
			"sh", "-c",
			"DISPLAY=:0 import -window root png:- | base64",
		},
	}

	ctx := context.Background()
	var result map[string]interface{}
	
	// 如果 Guest Agent 不可用，使用备用方案
	err = client.Post(ctx, screenshotPath, screenshotCmd, &result)
	if err != nil {
		// 方案2: 使用 Proxmox VNC Screenshot API (需要 Proxmox 7.2+)
		// 这个功能可能需要额外配置或不是所有版本都支持
		return "", fmt.Errorf("截图功能暂不可用: Proxmox 虚拟机需要安装 qemu-guest-agent 并配置 X11 显示，或升级到 Proxmox VE 7.2+ 使用内置截图 API")
	}

	// 等待命令执行完成并获取输出
	pid, ok := result["pid"].(float64)
	if !ok {
		return "", fmt.Errorf("执行截图命令失败")
	}

	// 轮询获取执行结果（最多等待10秒）
	for i := 0; i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		
		statusPath := fmt.Sprintf("/nodes/%s/qemu/%d/agent/exec-status?pid=%.0f", nodeName, vmid, pid)
		var statusResult map[string]interface{}
		err = client.Get(ctx, statusPath, &statusResult)
		if err != nil {
			continue
		}

		data, ok := statusResult["data"].(map[string]interface{})
		if !ok {
			continue
		}

		exited, ok := data["exited"].(bool)
		if !ok || !exited {
			continue
		}

		// 获取输出
		outData, ok := data["out-data"].(string)
		if ok && outData != "" {
			// out-data 已经是 base64 编码的 PNG
			return "data:image/png;base64," + outData, nil
		}
	}

	return "", fmt.Errorf("截图超时")
}

// ============================================================
// 备份管理
// ============================================================

// CreateBackup 创建备份
func (d *ProxmoxDriver) CreateBackup(vmUUID string, tips string) (*model.ZMessage, error) {
	node, err := d.getNode()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup", Message: err.Error()}, nil
	}

	vmid, err := d.getVMID(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup", Message: err.Error()}, nil
	}

	storage := "local"
	if d.Config.BackupPath != "" {
		storage = d.Config.BackupPath
	}

	task, err := node.Vzdump(context.Background(), &proxmox.VirtualMachineBackupOptions{
		VMID:     uint64(vmid),
		Storage:  storage,
		Compress: proxmox.VirtualMachineBackupCompressZstd,
	})
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup", Message: err.Error()}, nil
	}

	if err := task.WaitFor(context.Background(), 600); err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup", Message: "备份超时: " + err.Error()}, nil
	}

	// 更新备份列表
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		backupName := fmt.Sprintf("vzdump-qemu-%d-%s.vma.zst", vmid, time.Now().Format("2006_01_02-15_04_05"))
		cfg.Backups = append(cfg.Backups, model.VMBackup{
			BackupName: backupName,
			BackupTime: time.Now(),
			BackupTips: tips,
		})
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMBackup", Message: "备份创建成功"}, nil
}

// RestoreBackup 还原备份
func (d *ProxmoxDriver) RestoreBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	node, err := d.getNode()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "Restores", Message: err.Error()}, nil
	}

	vmid, err := d.getVMID(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "Restores", Message: err.Error()}, nil
	}

	storage := "local"
	if d.Config.BackupPath != "" {
		storage = d.Config.BackupPath
	}

	bkpStorage, err := node.Storage(context.Background(), storage)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "Restores", Message: err.Error()}, nil
	}

	bkp, err := bkpStorage.Backup(context.Background(), backupName)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "Restores", Message: err.Error()}, nil
	}

	_ = vmid
	_ = bkp
	return &model.ZMessage{Success: false, Action: "Restores", Message: "Proxmox驱动暂不支持备份还原"}, nil
}

// DeleteBackup 删除备份
func (d *ProxmoxDriver) DeleteBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	storage := "local"
	if d.Config.BackupPath != "" {
		storage = d.Config.BackupPath
	}

	node, err := d.getNode()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "RMBackup", Message: err.Error()}, nil
	}
	bkpStorage, err := node.Storage(context.Background(), storage)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "RMBackup", Message: err.Error()}, nil
	}
	volID := fmt.Sprintf("%s:backup/%s", storage, backupName)
	delTask, err := bkpStorage.DeleteContent(context.Background(), volID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "RMBackup", Message: err.Error()}, nil
	}
	if delTask != nil {
		_ = delTask.WaitFor(context.Background(), 60)
	}

	// 从备份列表中移除
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		newBackups := cfg.Backups[:0]
		for _, b := range cfg.Backups {
			if b.BackupName != backupName {
				newBackups = append(newBackups, b)
			}
		}
		cfg.Backups = newBackups
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "RMBackup", Message: "备份删除成功"}, nil
}

// ScanBackups 扫描备份文件
func (d *ProxmoxDriver) ScanBackups() (*model.ZMessage, error) {
	storage := "local"
	if d.Config.BackupPath != "" {
		storage = d.Config.BackupPath
	}

	node, err := d.getNode()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "LDBackup", Message: err.Error()}, nil
	}
	bkpStorage, err := node.Storage(context.Background(), storage)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "LDBackup", Message: err.Error()}, nil
	}
	contents, err := bkpStorage.GetContent(context.Background())
	if err != nil {
		return &model.ZMessage{Success: false, Action: "LDBackup", Message: err.Error()}, nil
	}

	// 按vmid分组备份（通过volid中包含backup判断）
	backupsByVM := make(map[int64][]*proxmox.StorageContent)
	for _, c := range contents {
		if strings.Contains(c.Volid, "backup") {
			backupsByVM[int64(c.VMID)] = append(backupsByVM[int64(c.VMID)], c)
		}
	}

	// 更新各VM的备份列表
	d.mu.Lock()
	for vmUUID, cfg := range d.VmSaving {
		vmid, _ := d.getVMIDNoLock(vmUUID)
		if vmid == 0 {
			continue
		}
		if bkps, ok := backupsByVM[vmid]; ok {
			cfg.Backups = make([]model.VMBackup, 0, len(bkps))
			for _, b := range bkps {
				cfg.Backups = append(cfg.Backups, model.VMBackup{
					BackupName: b.Volid,
					BackupTime: time.Unix(int64(b.Ctime), 0),
					BackupTips: b.Notes,
				})
			}
		}
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "LDBackup", Message: "备份扫描完成"}, nil
}

// getVMIDNoLock 不加锁获取VMID（内部使用，调用方已持有锁）
func (d *ProxmoxDriver) getVMIDNoLock(vmUUID string) (int64, error) {
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		if vmData, ok2 := cfg.ExtData["vmid"]; ok2 {
			if id, ok3 := vmData.(float64); ok3 && id > 0 {
				return int64(id), nil
			}
		}
	}
	return 0, fmt.Errorf("未找到VMID")
}

// ============================================================
// PCI / GPU 直通
// ============================================================

// GetGPUList 获取GPU设备列表
func (d *ProxmoxDriver) GetGPUList() (map[string]*model.VFConfig, error) {
	return d.GetPCIList()
}

// GetPCIList 获取PCI设备列表
func (d *ProxmoxDriver) GetPCIList() (map[string]*model.VFConfig, error) {
	// go-proxmox v0.4.1 不提供 PCIDevices API，返回空列表
	return make(map[string]*model.VFConfig), nil
}

// SetupPCI PCI设备直通操作
func (d *ProxmoxDriver) SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "PCISetup", Message: err.Error()}, nil
	}

	if add {
		// 添加PCI设备
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "hostpci0",
			Value: pciKey + ",pcie=1",
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "PCISetup", Message: err.Error()}, nil
		}
		// 更新内存配置
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			vmCfg.PciAll[pciKey] = *cfg
		}
		d.mu.Unlock()
	} else {
		// 移除PCI设备
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "delete",
			Value: "hostpci0",
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "PCISetup", Message: err.Error()}, nil
		}
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			delete(vmCfg.PciAll, pciKey)
		}
		d.mu.Unlock()
	}

	action := "添加"
	if !add {
		action = "移除"
	}
	return &model.ZMessage{Success: true, Action: "PCISetup", Message: "PCI设备" + action + "成功"}, nil
}

// ============================================================
// USB 直通
// ============================================================

// GetUSBList 获取USB设备列表
func (d *ProxmoxDriver) GetUSBList() (map[string]*model.USBInfos, error) {
	// go-proxmox v0.4.1 不提供 USBDevices API，返回空列表
	return make(map[string]*model.USBInfos), nil
}

// SetupUSB USB设备直通操作
func (d *ProxmoxDriver) SetupUSB(vmUUID string, info *model.USBInfos, usbKey string, add bool) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "USBSetup", Message: err.Error()}, nil
	}

	if add {
		usbVal := fmt.Sprintf("host=%s:%s", info.VidUUID, info.PidUUID)
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "usb0",
			Value: usbVal,
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "USBSetup", Message: err.Error()}, nil
		}
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			vmCfg.UsbAll[usbKey] = *info
		}
		d.mu.Unlock()
	} else {
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "delete",
			Value: "usb0",
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "USBSetup", Message: err.Error()}, nil
		}
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			delete(vmCfg.UsbAll, usbKey)
		}
		d.mu.Unlock()
	}

	action := "添加"
	if !add {
		action = "移除"
	}
	return &model.ZMessage{Success: true, Action: "USBSetup", Message: "USB设备" + action + "成功"}, nil
}

// ============================================================
// 磁盘管理
// ============================================================

// MountHDD 挂载/卸载数据盘
func (d *ProxmoxDriver) MountHDD(vmUUID string, hdd *model.SDConfig, mount bool) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDMount", Message: err.Error()}, nil
	}

	storage := "local-lvm"
	if d.Config.ExternPath != "" {
		storage = d.Config.ExternPath
	}

	if mount {
		// 创建并挂载数据盘
		sizeGB := hdd.HddSize / 1024
		if sizeGB <= 0 {
			sizeGB = 10
		}
		diskVal := fmt.Sprintf("%s:%d,format=qcow2", storage, sizeGB)
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "scsi1",
			Value: diskVal,
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "HDDMount", Message: err.Error()}, nil
		}
		hdd.HddFlag = 1
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			vmCfg.HddAll[hdd.HddName] = *hdd
		}
		d.mu.Unlock()
	} else {
		// 卸载数据盘
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "delete",
			Value: "scsi1",
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "HDDMount", Message: err.Error()}, nil
		}
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			if h, exists := vmCfg.HddAll[hdd.HddName]; exists {
				h.HddFlag = 0
				vmCfg.HddAll[hdd.HddName] = h
			}
		}
		d.mu.Unlock()
	}

	action := "挂载"
	if !mount {
		action = "卸载"
	}
	return &model.ZMessage{Success: true, Action: "HDDMount", Message: "磁盘" + action + "成功"}, nil
}

// MountISO 挂载/卸载ISO
func (d *ProxmoxDriver) MountISO(vmUUID string, iso *model.IMConfig, mount bool) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "ISOMount", Message: err.Error()}, nil
	}

	if mount {
		dvdStorage := "local"
		if d.Config.DvdromPath != "" {
			dvdStorage = d.Config.DvdromPath
		}
		isoVal := fmt.Sprintf("%s:iso/%s,media=cdrom", dvdStorage, iso.IsoFile)
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "ide2",
			Value: isoVal,
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "ISOMount", Message: err.Error()}, nil
		}
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			vmCfg.IsoAll[iso.IsoName] = *iso
		}
		d.mu.Unlock()
	} else {
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "ide2",
			Value: "none,media=cdrom",
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "ISOMount", Message: err.Error()}, nil
		}
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			delete(vmCfg.IsoAll, iso.IsoName)
		}
		d.mu.Unlock()
	}

	action := "挂载"
	if !mount {
		action = "卸载"
	}
	return &model.ZMessage{Success: true, Action: "ISOMount", Message: "ISO" + action + "成功"}, nil
}

// ============================================================
// 密码设置
// ============================================================

// SetPassword 设置虚拟机密码（通过QEMU guest agent）
func (d *ProxmoxDriver) SetPassword(vmUUID string, password string) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "Password", Message: err.Error()}, nil
	}

	// 通过QEMU guest agent设置密码
	if err := vm.AgentSetUserPassword(context.Background(), "root", password); err != nil {
		// 如果guest agent不可用，只更新内存中的密码
		d.mu.Lock()
		if cfg, ok := d.VmSaving[vmUUID]; ok {
			cfg.OsPass = password
		}
		d.mu.Unlock()
		return &model.ZMessage{Success: true, Action: "Password", Message: "密码已记录（guest agent不可用）"}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		cfg.OsPass = password
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "Password", Message: "密码设置成功"}, nil
}

// ============================================================
// 定时任务
// ============================================================

// Crontab 执行定时任务（同步VM状态）
func (d *ProxmoxDriver) Crontab() bool {
	node, err := d.getNode()
	if err != nil {
		return false
	}

	vms, err := node.VirtualMachines(context.Background())
	if err != nil {
		return false
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	for _, vm := range vms {
		vmUUID := strings.ReplaceAll(vm.Name, "-", "_")
		if cfg, ok := d.VmSaving[vmUUID]; ok {
			switch vm.Status {
			case "running":
				cfg.VmFlag = model.VMPowerStarted
			case "stopped":
				cfg.VmFlag = model.VMPowerStopped
			case "paused":
				cfg.VmFlag = model.VMPowerSuspend
			default:
				cfg.VmFlag = model.VMPowerUnknown
			}
		}
	}

	return true
}