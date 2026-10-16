// Package driver SmolVM (Firecracker microVM) 驱动扩展实现。
//
// 补齐 Python 版 HostServer/SmolVM.py 中存在、而 Go 版此前缺失的能力：
//   - 虚拟机列表 / 详情（从 vm.json 恢复）
//   - 配置更新（UpdateVM）与重启
//   - 控制台连接信息（Firecracker 为串口/TTY，非 VNC）
//   - 数据盘挂载（额外 drive）
//
// 说明：Firecracker 不支持 ISO 光驱、PCI/USB 直通与磁盘路径之外的设备，
// 这些能力统一返回明确失败，不做伪实现。
package driver

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// smolvmRestart 重启 microVM（kill → 重新拉起 firecracker → FC API 装配）
func (d *SmolVMDriver) smolvmRestart(cfg *model.VMConfig) error {
	vmUUID := cfg.VmUUID
	d.killPid(d.readPid(vmUUID), true)
	time.Sleep(500 * time.Millisecond)

	vmDir := d.vmDir(vmUUID)
	rootfs := filepath.ToSlash(filepath.Join(vmDir, "rootfs.ext4"))
	sock := d.vmSock(vmUUID)
	pidFile := d.vmPidFile(vmUUID)

	d.bindTap(cfg, true)
	d.execHost(fmt.Sprintf("rm -f \"%s\"", sock), 5*time.Second)

	hv := d.hypervisorBin()
	startCmd := fmt.Sprintf("setsid %s --api-sock \"%s\" >>\"%s/fc.log\" 2>&1 & echo $! > \"%s\"",
		hv, sock, vmDir, pidFile)
	if ok, _, err := d.execHost(startCmd, 10*time.Second); !ok {
		return fmt.Errorf("重新启动 firecracker 失败: %v", err)
	}
	d.execHost(fmt.Sprintf("sleep 0.3; chmod 0600 \"%s\" 2>/dev/null || true", sock), 3*time.Second)

	if !d.waitSockReady(sock, 10*time.Second) {
		d.killPid(d.readPid(vmUUID), true)
		return fmt.Errorf("firecracker socket 未就绪")
	}
	if err := d.assembleVM(cfg, sock, rootfs); err != nil {
		d.killPid(d.readPid(vmUUID), true)
		return fmt.Errorf("FC API 装配失败: %w", err)
	}
	return nil
}

// GetVMList 扫描 <ExternPath>/*/vm.json 重建虚拟机列表
func (d *SmolVMDriver) GetVMList() (map[string]*model.VMConfig, error) {
	result := d.GetVMSaving()

	base := strings.TrimSpace(d.Config.ExternPath)
	if base == "" {
		base = "/var/lib/smolvm"
	}
	ok, out, _ := d.execHost(fmt.Sprintf("ls -1 \"%s\" 2>/dev/null || true", base), 10*time.Second)
	if !ok {
		return result, nil
	}

	for _, name := range splitLines(out) {
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		cfg := d.loadVMJSON(name)
		if cfg == nil {
			continue
		}
		// 电源状态以 pid 存活为准
		if d.pidAlive(d.readPid(name)) {
			cfg.VmFlag = model.VMPowerStarted
		} else {
			cfg.VmFlag = model.VMPowerStopped
		}
		if existing, exists := result[name]; exists {
			existing.VmFlag = cfg.VmFlag
		} else {
			result[name] = cfg
		}
	}
	return result, nil
}

// GetVMDetail 获取虚拟机详情（内存优先，缺失时从 vm.json 恢复）
func (d *SmolVMDriver) GetVMDetail(vmUUID string) (*model.VMConfig, error) {
	if cfg, err := d.BaseDriver.GetVMDetail(vmUUID); err == nil {
		return cfg, nil
	}
	cfg := d.loadVMJSON(vmUUID)
	if cfg == nil {
		return nil, fmt.Errorf("虚拟机 %s 不存在", vmUUID)
	}
	if d.pidAlive(d.readPid(vmUUID)) {
		cfg.VmFlag = model.VMPowerStarted
	} else {
		cfg.VmFlag = model.VMPowerStopped
	}
	return cfg, nil
}

// UpdateVM 更新 microVM 配置（CPU / 内存）。
//
// Firecracker 的 CPU/内存只能在启动时确定，因此更新后必须重启才生效。
func (d *SmolVMDriver) UpdateVM(cfg *model.VMConfig, oldCfg *model.VMConfig) (*model.ZMessage, error) {
	if cfg == nil {
		return &model.ZMessage{Success: false, Action: "VMUpdate", Message: "配置为空"}, nil
	}

	changed := cfg.CpuNum != oldCfg.CpuNum || cfg.MemNum != oldCfg.MemNum ||
		cfg.OsName != oldCfg.OsName

	// 保留旧的业务字段（所有者、端口、备份等），仅更新资源配置
	merged := cfg
	merged.Backups = oldCfg.Backups
	if merged.OwnAll == nil {
		merged.OwnAll = oldCfg.OwnAll
	}

	d.saveVMJSON(merged)
	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = merged
	d.mu.Unlock()

	if !changed {
		return &model.ZMessage{Success: true, Action: "VMUpdate", Message: "配置已保存"}, nil
	}

	// 运行中则需要重启使 CPU/内存生效
	if d.pidAlive(d.readPid(cfg.VmUUID)) {
		if err := d.smolvmRestart(merged); err != nil {
			return &model.ZMessage{Success: false, Action: "VMUpdate",
				Message: fmt.Sprintf("配置已保存，但重启失败: %v", err)}, nil
		}
		d.mu.Lock()
		if c, ok := d.VmSaving[cfg.VmUUID]; ok {
			c.VmFlag = model.VMPowerStarted
		}
		d.mu.Unlock()
		return &model.ZMessage{Success: true, Action: "VMUpdate",
			Message: "配置更新成功并已重启生效"}, nil
	}

	return &model.ZMessage{Success: true, Action: "VMUpdate",
		Message: "配置更新成功（下次启动生效）"}, nil
}

// GetVNCInfo 获取控制台连接信息。
//
// Firecracker 只提供串口控制台，没有 VNC；这里返回与 TTY 访问方式一致的信息，
// 避免像以前那样走基类直接返回「不支持」而让前端无法进入控制台。
func (d *SmolVMDriver) GetVNCInfo(vmUUID string, ipAddr string) (*model.ZMessage, error) {
	if !d.pidAlive(d.readPid(vmUUID)) {
		return &model.ZMessage{Success: false, Action: "VCRemote",
			Message: "microVM 未运行，无法连接控制台"}, nil
	}
	sock := d.vmSock(vmUUID)
	return &model.ZMessage{Success: true, Action: "VCRemote",
		Message: "串口控制台信息获取成功",
		Results: map[string]interface{}{
			"type":      "tty",
			"vm_uuid":   vmUUID,
			"socket":    sock,
			"connect":   fmt.Sprintf("socat -,raw,echo=0 UNIX-CONNECT:%s", sock),
			"password":  "",
		}}, nil
}

// MountHDD 挂载 / 卸载数据盘（Firecracker 额外 drive，需重启生效）
func (d *SmolVMDriver) MountHDD(vmUUID string, hdd *model.SDConfig, mount bool) (*model.ZMessage, error) {
	if hdd == nil || strings.TrimSpace(hdd.HddName) == "" {
		return &model.ZMessage{Success: false, Action: "HDDMount", Message: "磁盘信息不完整"}, nil
	}
	d.mu.RLock()
	cfg, ok := d.VmSaving[vmUUID]
	d.mu.RUnlock()
	if !ok {
		return &model.ZMessage{Success: false, Action: "HDDMount", Message: "虚拟机不存在"}, nil
	}

	if !mount {
		d.mu.Lock()
		if h, exists := cfg.HddAll[hdd.HddName]; exists {
			h.HddFlag = 0
			cfg.HddAll[hdd.HddName] = h
		}
		d.mu.Unlock()
		d.saveVMJSON(cfg)
		if d.pidAlive(d.readPid(vmUUID)) {
			if err := d.smolvmRestart(cfg); err != nil {
				return &model.ZMessage{Success: false, Action: "HDDMount",
					Message: fmt.Sprintf("卸载后重启失败: %v", err)}, nil
			}
		}
		return &model.ZMessage{Success: true, Action: "HDDMount", Message: "磁盘卸载成功"}, nil
	}

	// 创建磁盘镜像文件（ext4）
	diskPath := hdd.HddPath
	if diskPath == "" {
		diskPath = filepath.ToSlash(filepath.Join(d.vmDir(vmUUID), hdd.HddName+".ext4"))
	}
	sizeMB := hdd.HddSize
	if sizeMB <= 0 {
		sizeMB = 10240
	}
	createCmd := fmt.Sprintf(
		"test -f \"%s\" || (dd if=/dev/zero of=\"%s\" bs=1M count=%d status=none && mkfs.ext4 -F \"%s\")",
		diskPath, diskPath, sizeMB, diskPath)
	if ok, _, err := d.execHost(createCmd, 10*time.Minute); !ok {
		return &model.ZMessage{Success: false, Action: "HDDMount",
			Message: fmt.Sprintf("创建数据盘失败: %v", err)}, nil
	}

	d.mu.Lock()
	if cfg.HddAll == nil {
		cfg.HddAll = make(map[string]model.SDConfig)
	}
	mounted := *hdd
	mounted.HddFlag = 1
	mounted.HddPath = diskPath
	cfg.HddAll[hdd.HddName] = mounted
	d.mu.Unlock()
	d.saveVMJSON(cfg)

	if d.pidAlive(d.readPid(vmUUID)) {
		if err := d.smolvmRestart(cfg); err != nil {
			return &model.ZMessage{Success: false, Action: "HDDMount",
				Message: fmt.Sprintf("挂载后重启失败: %v", err)}, nil
		}
	}

	return &model.ZMessage{Success: true, Action: "HDDMount", Message: "磁盘挂载成功"}, nil
}

// MountISO Firecracker 无光驱设备，明确拒绝
func (d *SmolVMDriver) MountISO(vmUUID string, iso *model.IMConfig, mount bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "ISOMount",
		Message: "SmolVM (Firecracker) 不支持 ISO 光驱"}, nil
}

// MountUSB Firecracker 无 USB 设备，明确拒绝
func (d *SmolVMDriver) MountUSB(vmUUID string, usbKey string, mount bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "USBMount",
		Message: "SmolVM (Firecracker) 不支持 USB 存储"}, nil
}

// GetPCIList Firecracker 不支持 PCI 直通
func (d *SmolVMDriver) GetPCIList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// GetGPUList Firecracker 不支持 GPU 直通
func (d *SmolVMDriver) GetGPUList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// GetUSBList Firecracker 不支持 USB 直通
func (d *SmolVMDriver) GetUSBList() (map[string]*model.USBInfos, error) {
	return make(map[string]*model.USBInfos), nil
}

// SetupPCI Firecracker 不支持 PCI 直通，明确拒绝
func (d *SmolVMDriver) SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "PCISetup",
		Message: "SmolVM (Firecracker) 不支持 PCI 设备直通"}, nil
}

// SetupUSB Firecracker 不支持 USB 直通，明确拒绝
func (d *SmolVMDriver) SetupUSB(vmUUID string, info *model.USBInfos, usbKey string, add bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "USBSetup",
		Message: "SmolVM (Firecracker) 不支持 USB 设备直通"}, nil
}

// GetStatus 主机状态。
//
// 与 Python 版 SmolVM.HSStatus 一致：server_addr 指向远程（或带 ssh:// 前缀）时
// 通过 SSH 采集对端宿主机状态，本地时使用本机采集。
func (d *SmolVMDriver) GetStatus() (*model.HWStatus, error) {
	return d.BaseDriver.statusForPlatform()
}
