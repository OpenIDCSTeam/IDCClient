// Package driver VirtualBox 驱动生命周期实现（VBoxManage）
// 与 Python 版 HostServer/VirtualBoxs.py 的功能语义对齐。
package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/security"
)

// execVBox 执行 VBoxManage 命令
func (d *VirtualBoxDriver) execVBox(args ...string) (string, error) {
	full := append([]string{"VBoxManage"}, args...)
	out, _, err := security.SafeExecWithTimeout(full, 60*time.Second)
	return out, err
}

// vboxDiskDir 返回 VirtualBox 磁盘目录
func (d *VirtualBoxDriver) vboxDiskDir() string {
	if d.Config.SystemPath != "" {
		return d.Config.SystemPath
	}
	return "."
}

// GetStatus 获取 VirtualBox 主机状态。
//
// 与 Python 版 VirtualBoxs.HSStatus 一致：解析 `VBoxManage list hostinfo`
// 输出的 CPU 核数与内存信息。原实现未覆写该方法，状态恒为空。
func (d *VirtualBoxDriver) GetStatus() (*model.HWStatus, error) {
	hw := emptyHWStatus()
	out, err := d.execVBox("list", "hostinfo")
	if err != nil {
		return hw, nil
	}

	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "Processor count:"):
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(trimmed, "Processor count:")), "%d", &hw.CpuTotal)
		case strings.HasPrefix(trimmed, "Processor core count:"):
			if hw.CpuTotal == 0 {
				fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(trimmed, "Processor core count:")), "%d", &hw.CpuTotal)
			}
		case strings.HasPrefix(trimmed, "Memory size:"):
			fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(trimmed, "Memory size:")))
			if len(fields) > 0 {
				var mb int
				fmt.Sscanf(fields[0], "%d", &mb)
				hw.MemTotal = mb
				// 形如 "16384 MByte"
				if len(fields) > 1 && strings.EqualFold(fields[1], "GByte") {
					hw.MemTotal = mb * 1024
				}
			}
		}
	}
	return hw, nil
}

// ScanVMs 扫描并同步虚拟机列表
func (d *VirtualBoxDriver) ScanVMs() (*model.ZMessage, error) {
	vms, err := d.GetVMList()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMDetect", Message: err.Error()}, nil
	}

	prefix := d.Config.FilterName
	d.mu.Lock()
	added := 0
	for name, cfg := range vms {
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		if _, exists := d.VmSaving[name]; !exists {
			d.VmSaving[name] = cfg
			added++
		} else {
			d.VmSaving[name].VmFlag = cfg.VmFlag
		}
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDetect",
		Message: fmt.Sprintf("扫描完成：共 %d 个，新增 %d 个", len(vms), added)}, nil
}

// PowerControl 电源控制
func (d *VirtualBoxDriver) PowerControl(vmUUID string, action model.VMPowers) (*model.ZMessage, error) {
	var args []string
	switch action {
	case model.VMPowerSStart:
		args = []string{"startvm", vmUUID, "--type", "headless"}
	case model.VMPowerSClose:
		args = []string{"controlvm", vmUUID, "acpipowerbutton"}
	case model.VMPowerHClose:
		args = []string{"controlvm", vmUUID, "poweroff"}
	case model.VMPowerSReset, model.VMPowerHReset:
		args = []string{"controlvm", vmUUID, "reset"}
	case model.VMPowerAPause:
		args = []string{"controlvm", vmUUID, "pause"}
	case model.VMPowerAWaked:
		args = []string{"controlvm", vmUUID, "resume"}
	default:
		return d.BaseDriver.PowerControl(vmUUID, action)
	}

	if out, err := d.execVBox(args...); err != nil {
		return &model.ZMessage{Success: false, Action: "VMPowers",
			Message: fmt.Sprintf("电源操作失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	// 更新内存状态（异步操作后以查询为准）
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
func (d *VirtualBoxDriver) CreateVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	vmName := cfg.VmUUID

	if out, err := d.execVBox("createvm", "--name", vmName,
		"--ostype", "Linux26_64", "--register"); err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: fmt.Sprintf("创建虚拟机失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	mem := cfg.MemNum
	if mem <= 0 {
		mem = 1024
	}
	cpu := cfg.CpuNum
	if cpu <= 0 {
		cpu = 1
	}
	args := []string{"modifyvm", vmName,
		"--memory", fmt.Sprintf("%d", mem),
		"--cpus", fmt.Sprintf("%d", cpu),
		"--vrde", "on",
		"--vrdeport", "0",
	}
	// 网卡：桥接 + 固定 MAC（原先硬编码 nat，既无桥接也无 MAC）
	args = append(args, d.nicArgs(cfg)...)
	if out, err := d.execVBox(args...); err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: fmt.Sprintf("配置虚拟机失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	// 创建虚拟磁盘：优先从系统模板镜像克隆，避免创建出没有系统的裸盘
	diskPath := filepath.Join(d.vboxDiskDir(), vmName+".vdi")
	sizeMB := cfg.HddNum
	if sizeMB <= 0 {
		sizeMB = 8192
	}
	if err := d.createDiskFromTemplate(cfg, diskPath, sizeMB); err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: "创建磁盘失败: " + err.Error()}, nil
	}

	// 添加 SATA 控制器并挂载磁盘
	_, _ = d.execVBox("storagectl", vmName, "--name", "SATA",
		"--add", "sata", "--controller", "IntelAhci")
	if out, err := d.execVBox("storageattach", vmName,
		"--storagectl", "SATA", "--port", "0", "--device", "0",
		"--type", "hdd", "--medium", diskPath); err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: fmt.Sprintf("挂载磁盘失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	// 启动
	if _, err := d.PowerControl(vmName, model.VMPowerSStart); err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: "启动虚拟机失败: " + err.Error()}, nil
	}

	cfg.VmFlag = model.VMPowerStarted
	d.mu.Lock()
	d.VmSaving[vmName] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMCreate", Message: "虚拟机创建成功"}, nil
}

// createDiskFromTemplate 基于系统模板镜像克隆出虚拟机系统盘。
//
// 对应 Python VirtualBoxs.VMCreate：
//
//	VBoxManage clonemedium <模板> <目标.vdi> --format VDI
//
// 直接用 createmedium 建的是空盘，虚拟机没有可启动系统，
// 仅在模板缺失时回退为空盘以保证流程继续。
func (d *VirtualBoxDriver) createDiskFromTemplate(cfg *model.VMConfig, diskPath string, sizeMB int) error {
	imagesPath := strings.TrimSpace(d.Config.ImagesPath)
	if cfg != nil && strings.TrimSpace(cfg.OsName) != "" && imagesPath != "" {
		imFile := filepath.Join(imagesPath, cfg.OsName)
		if _, statErr := os.Stat(imFile); statErr == nil {
			if out, err := d.execVBox("clonemedium", imFile, diskPath, "--format", "VDI"); err == nil {
				return nil
			} else if strings.Contains(out, "already exists") {
				return nil
			}
		}
	}

	if out, err := d.execVBox("createmedium", "disk",
		"--filename", diskPath,
		"--size", fmt.Sprintf("%d", sizeMB),
		"--format", "VDI"); err != nil {
		return fmt.Errorf("%v (%s)", err, strings.TrimSpace(out))
	}
	return nil
}

// nicArgs 生成 VirtualBox 网卡参数（桥接 + 固定 MAC）。
//
// 对应 Python VirtualBoxs.VMCreate：
//
//	--nicN bridged --bridgeadapterN <网桥> --macaddressN <MAC>
//
// MAC 必须固定：路由器上的 DHCP 静态绑定与 ARP 绑定都以 MAC 为键，
// 若由 VirtualBox 随机生成，绑定记录与实际网卡对不上，虚拟机拿不到固定 IP。
func (d *VirtualBoxDriver) nicArgs(cfg *model.VMConfig) []string {
	if cfg == nil || len(cfg.NicAll) == 0 {
		// 未配置网卡时回退 NAT，保证虚拟机仍可联网
		return []string{"--nic1", "nat"}
	}

	bridge := strings.TrimSpace(d.Config.NetworkNat)
	if bridge == "" {
		bridge = strings.TrimSpace(d.Config.NetworkPub)
	}
	if bridge == "" {
		bridge = "eth0"
	}

	names := make([]string, 0, len(cfg.NicAll))
	for name := range cfg.NicAll {
		names = append(names, name)
	}
	sort.Strings(names)

	args := make([]string, 0, len(names)*4)
	idx := 1
	for _, name := range names {
		if idx > 4 { // VirtualBox 仅支持 4 张网卡
			break
		}
		nic := cfg.NicAll[name]
		args = append(args,
			fmt.Sprintf("--nic%d", idx), "bridged",
			fmt.Sprintf("--bridgeadapter%d", idx), bridge)

		if mac := strings.ReplaceAll(strings.TrimSpace(nic.MacAddr), ":", ""); mac != "" {
			args = append(args, fmt.Sprintf("--macaddress%d", idx), strings.ToUpper(mac))
		}
		idx++
	}
	return args
}

// DeleteVM 删除虚拟机
func (d *VirtualBoxDriver) DeleteVM(vmUUID string, removeBackup bool) (*model.ZMessage, error) {
	// 先关机
	_, _ = d.PowerControl(vmUUID, model.VMPowerHClose)

	args := []string{"unregistervm", vmUUID}
	if removeBackup {
		args = append(args, "--delete")
	}
	if out, err := d.execVBox(args...); err != nil {
		return &model.ZMessage{Success: false, Action: "VMDelete",
			Message: fmt.Sprintf("删除失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	delete(d.VmSaving, vmUUID)
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDelete", Message: "虚拟机删除成功"}, nil
}

// UpdateVM 更新虚拟机配置（CPU / 内存）
func (d *VirtualBoxDriver) UpdateVM(cfg *model.VMConfig, oldCfg *model.VMConfig) (*model.ZMessage, error) {
	args := []string{"modifyvm", cfg.VmUUID}
	if cfg.CpuNum != oldCfg.CpuNum && cfg.CpuNum > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%d", cfg.CpuNum))
	}
	if cfg.MemNum != oldCfg.MemNum && cfg.MemNum > 0 {
		args = append(args, "--memory", fmt.Sprintf("%d", cfg.MemNum))
	}
	if len(args) > 2 {
		if out, err := d.execVBox(args...); err != nil {
			return &model.ZMessage{Success: false, Action: "VMUpdate",
				Message: fmt.Sprintf("更新失败: %v (%s)", err, strings.TrimSpace(out))}, nil
		}
	}

	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMUpdate", Message: "虚拟机配置更新成功"}, nil
}

// ReinstallVM 重装系统（移除旧盘并挂载新盘后启动）
func (d *VirtualBoxDriver) ReinstallVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	vmName := cfg.VmUUID
	_, _ = d.PowerControl(vmName, model.VMPowerHClose)

	_, _ = d.execVBox("storageattach", vmName, "--storagectl", "SATA",
		"--port", "0", "--device", "0", "--type", "hdd", "--medium", "none")

	diskPath := filepath.Join(d.vboxDiskDir(), vmName+".vdi")
	_, _ = d.execVBox("closemedium", "disk", diskPath, "--delete")

	sizeMB := cfg.HddNum
	if sizeMB <= 0 {
		sizeMB = 8192
	}
	if out, err := d.execVBox("createmedium", "disk",
		"--filename", diskPath, "--size", fmt.Sprintf("%d", sizeMB), "--format", "VDI"); err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall",
			Message: fmt.Sprintf("创建磁盘失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}
	_, _ = d.execVBox("storageattach", vmName, "--storagectl", "SATA",
		"--port", "0", "--device", "0", "--type", "hdd", "--medium", diskPath)

	if _, err := d.PowerControl(vmName, model.VMPowerSStart); err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall",
			Message: "启动失败: " + err.Error()}, nil
	}
	return &model.ZMessage{Success: true, Action: "VMReinstall", Message: "系统重装成功"}, nil
}

// GetVNCInfo 获取 VRDE 远程桌面信息
func (d *VirtualBoxDriver) GetVNCInfo(vmUUID string, ipAddr string) (*model.ZMessage, error) {
	out, err := d.execVBox("showvminfo", vmUUID, "--machinereadable")
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VCRemote",
			Message: "获取信息失败: " + err.Error()}, nil
	}

	host := d.Config.ServerAddr
	port := 3389
	for _, line := range splitLines(out) {
		if strings.HasPrefix(line, "vrdeport=") {
			p := strings.Trim(strings.TrimPrefix(line, "vrdeport="), "\"")
			if p != "" && p != "0" {
				fmt.Sscanf(p, "%d", &port)
			}
		}
	}
	if host == "" {
		host = "127.0.0.1"
	}

	return &model.ZMessage{Success: true, Action: "VCRemote", Message: "远程桌面信息获取成功",
		Results: map[string]interface{}{"type": "rdp", "host": host, "port": port, "password": ""}}, nil
}

// MountHDD 挂载 / 卸载数据盘
func (d *VirtualBoxDriver) MountHDD(vmUUID string, hdd *model.SDConfig, mount bool) (*model.ZMessage, error) {
	if !mount {
		_, _ = d.execVBox("storageattach", vmUUID, "--storagectl", "SATA",
			"--port", "1", "--device", "0", "--type", "hdd", "--medium", "none")
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

	path := filepath.Join(d.vboxDiskDir(), hdd.HddName+".vdi")
	sizeMB := hdd.HddSize
	if sizeMB <= 0 {
		sizeMB = 10240
	}
	if out, err := d.execVBox("createmedium", "disk",
		"--filename", path, "--size", fmt.Sprintf("%d", sizeMB), "--format", "VDI"); err != nil {
		return &model.ZMessage{Success: false, Action: "HDDMount",
			Message: fmt.Sprintf("创建数据盘失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}
	if out, err := d.execVBox("storageattach", vmUUID, "--storagectl", "SATA",
		"--port", "1", "--device", "0", "--type", "hdd", "--medium", path); err != nil {
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
func (d *VirtualBoxDriver) MountISO(vmUUID string, iso *model.IMConfig, mount bool) (*model.ZMessage, error) {
	// 确保 IDE 控制器存在
	_, _ = d.execVBox("storagectl", vmUUID, "--name", "IDE", "--add", "ide")

	if mount {
		path := iso.IsoPath
		if path == "" {
			dir := d.Config.DvdromPath
			if dir == "" {
				dir = d.Config.ImagesPath
			}
			path = filepath.Join(dir, iso.IsoFile)
		}
		if out, err := d.execVBox("storageattach", vmUUID, "--storagectl", "IDE",
			"--port", "0", "--device", "0", "--type", "dvddrive", "--medium", path); err != nil {
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

	_, _ = d.execVBox("storageattach", vmUUID, "--storagectl", "IDE",
		"--port", "0", "--device", "0", "--type", "dvddrive", "--medium", "emptydrive")
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		delete(cfg.IsoAll, iso.IsoName)
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "ISOMount", Message: "ISO卸载成功"}, nil
}

// CreateBackup 创建备份（克隆硬盘）
func (d *VirtualBoxDriver) CreateBackup(vmUUID string, tips string) (*model.ZMessage, error) {
	name := fmt.Sprintf("%s-%s", vmUUID, time.Now().Format("20060102-150405"))
	if out, err := d.execVBox("clonevm", vmUUID, "--name", name, "--register"); err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: fmt.Sprintf("创建备份失败: %v (%s)", err, strings.TrimSpace(out))}, nil
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

// DeleteBackup 删除备份（删除克隆机）
func (d *VirtualBoxDriver) DeleteBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	if out, err := d.execVBox("unregistervm", backupName, "--delete"); err != nil {
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

// SetupUSB USB直通（VBoxManage usbfilter）
// 与 Python 版 VirtualBoxs.USBSetup 一致
func (d *VirtualBoxDriver) SetupUSB(vmUUID string, info *model.USBInfos, usbKey string, add bool) (*model.ZMessage, error) {
	var out string
	var err error
	if add {
		name := info.UsbHint
		if name == "" {
			name = usbKey
		}
		out, err = d.execVBox("usbfilter", "add", "0",
			"--target", vmUUID,
			"--name", name,
			"--vendorid", info.VidUUID,
			"--productid", info.PidUUID)
	} else {
		out, err = d.execVBox("usbfilter", "remove", "0", "--target", vmUUID)
	}
	if err != nil {
		return &model.ZMessage{Success: false, Action: "USBSetup",
			Message: fmt.Sprintf("USB操作失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		if cfg.UsbAll == nil {
			cfg.UsbAll = make(map[string]model.USBInfos)
		}
		if add {
			cfg.UsbAll[usbKey] = *info
		} else {
			delete(cfg.UsbAll, usbKey)
		}
	}
	d.mu.Unlock()

	text := "添加"
	if !add {
		text = "移除"
	}
	return &model.ZMessage{Success: true, Action: "USBSetup", Message: "USB设备" + text + "成功"}, nil
}

// SetPassword 通过 guestcontrol 修改虚拟机密码
func (d *VirtualBoxDriver) SetPassword(vmUUID string, password string) (*model.ZMessage, error) {
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

	out, err := d.execVBox("guestcontrol", vmUUID, "run",
		"--exe", "/bin/bash",
		"--username", user,
		"--password", oldPass,
		"--", "bash", "-c",
		fmt.Sprintf("echo '%s:%s' | chpasswd", user, password))
	if err != nil {
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
func (d *VirtualBoxDriver) Crontab() bool {
	d.mu.RLock()
	names := make([]string, 0, len(d.VmSaving))
	for name := range d.VmSaving {
		names = append(names, name)
	}
	d.mu.RUnlock()

	for _, name := range names {
		state := d.GetPower(name)
		d.mu.Lock()
		if cfg, ok := d.VmSaving[name]; ok {
			if s, ok2 := PowerStatusMap[state]; ok2 {
				cfg.VmFlag = s
			}
		}
		d.mu.Unlock()
	}
	return true
}
