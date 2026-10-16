// Package driver LXC/LXD 驱动生命周期实现（lxc 命令行）
// 与 Python 版 HostServer/LXContainer.py 的功能语义对齐。
package driver

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/security"
)

// execLXC 执行 lxc 命令
func (d *LXDDriver) execLXC(args ...string) (string, error) {
	full := append([]string{"lxc"}, args...)
	out, _, err := security.SafeExecWithTimeout(full, 10*time.Minute)
	return out, err
}

// lxdInstance lxc list JSON 条目
type lxdInstance struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	State  *struct {
		Status string `json:"status"`
	} `json:"state"`
}

// GetVMList 获取容器列表
func (d *LXDDriver) GetVMList() (map[string]*model.VMConfig, error) {
	result := d.GetVMSaving()

	out, err := d.execLXC("list", "--format", "json")
	if err != nil {
		return result, nil
	}

	var instances []lxdInstance
	if err := json.Unmarshal([]byte(out), &instances); err != nil {
		return result, nil
	}

	for _, inst := range instances {
		status := inst.Status
		if inst.State != nil && inst.State.Status != "" {
			status = inst.State.Status
		}
		var flag model.VMPowers
		switch strings.ToLower(status) {
		case "running":
			flag = model.VMPowerStarted
		case "stopped":
			flag = model.VMPowerStopped
		case "frozen":
			flag = model.VMPowerSuspend
		default:
			flag = model.VMPowerUnknown
		}

		if cfg, ok := result[inst.Name]; ok {
			cfg.VmFlag = flag
		} else {
			cfg := model.NewVMConfig()
			cfg.VmUUID = inst.Name
			cfg.OsName = inst.Name
			cfg.VmFlag = flag
			result[inst.Name] = cfg
		}
	}
	return result, nil
}

// ScanVMs 扫描并同步容器列表
func (d *LXDDriver) ScanVMs() (*model.ZMessage, error) {
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
func (d *LXDDriver) PowerControl(vmUUID string, action model.VMPowers) (*model.ZMessage, error) {
	var args []string
	switch action {
	case model.VMPowerSStart:
		args = []string{"start", vmUUID}
	case model.VMPowerSClose:
		args = []string{"stop", vmUUID}
	case model.VMPowerHClose:
		args = []string{"stop", vmUUID, "--force"}
	case model.VMPowerSReset:
		args = []string{"restart", vmUUID}
	case model.VMPowerHReset:
		args = []string{"restart", vmUUID, "--force"}
	case model.VMPowerAPause:
		args = []string{"pause", vmUUID}
	case model.VMPowerAWaked:
		args = []string{"start", vmUUID}
	default:
		return d.BaseDriver.PowerControl(vmUUID, action)
	}

	if out, err := d.execLXC(args...); err != nil {
		return &model.ZMessage{Success: false, Action: "VMPowers",
			Message: fmt.Sprintf("电源操作失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	if state, ok := PowerStatusMap[d.GetPower(vmUUID)]; ok {
		d.mu.Lock()
		if cfg, exists := d.VmSaving[vmUUID]; exists {
			cfg.VmFlag = state
		}
		d.mu.Unlock()
	}
	return &model.ZMessage{Success: true, Action: "VMPowers", Message: "电源操作已执行"}, nil
}

// CreateVM 创建容器
func (d *LXDDriver) CreateVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	image := cfg.OsName
	if image == "" {
		image = "images:alpine/3.19"
	}

	if out, err := d.execLXC("launch", image, cfg.VmUUID); err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: fmt.Sprintf("创建容器失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	if cfg.CpuNum > 0 {
		_, _ = d.execLXC("config", "set", cfg.VmUUID, "limits.cpu", fmt.Sprintf("%d", cfg.CpuNum))
	}
	if cfg.MemNum > 0 {
		_, _ = d.execLXC("config", "set", cfg.VmUUID, "limits.memory", fmt.Sprintf("%dMB", cfg.MemNum))
	}
	if d.Config.NetworkNat != "" {
		_, _ = d.execLXC("config", "device", "set", cfg.VmUUID, "eth0", "network", d.Config.NetworkNat)
	}

	cfg.VmFlag = model.VMPowerStarted
	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMCreate", Message: "容器创建成功"}, nil
}

// DeleteVM 删除容器
func (d *LXDDriver) DeleteVM(vmUUID string, removeBackup bool) (*model.ZMessage, error) {
	if out, err := d.execLXC("delete", vmUUID, "--force"); err != nil {
		return &model.ZMessage{Success: false, Action: "VMDelete",
			Message: fmt.Sprintf("删除失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	delete(d.VmSaving, vmUUID)
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDelete", Message: "容器删除成功"}, nil
}

// UpdateVM 更新容器资源限制
func (d *LXDDriver) UpdateVM(cfg *model.VMConfig, oldCfg *model.VMConfig) (*model.ZMessage, error) {
	if cfg.CpuNum != oldCfg.CpuNum && cfg.CpuNum > 0 {
		if out, err := d.execLXC("config", "set", cfg.VmUUID, "limits.cpu",
			fmt.Sprintf("%d", cfg.CpuNum)); err != nil {
			return &model.ZMessage{Success: false, Action: "VMUpdate",
				Message: fmt.Sprintf("修改CPU失败: %v (%s)", err, strings.TrimSpace(out))}, nil
		}
	}
	if cfg.MemNum != oldCfg.MemNum && cfg.MemNum > 0 {
		if out, err := d.execLXC("config", "set", cfg.VmUUID, "limits.memory",
			fmt.Sprintf("%dMB", cfg.MemNum)); err != nil {
			return &model.ZMessage{Success: false, Action: "VMUpdate",
				Message: fmt.Sprintf("修改内存失败: %v (%s)", err, strings.TrimSpace(out))}, nil
		}
	}

	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMUpdate", Message: "容器配置更新成功"}, nil
}

// ReinstallVM 重装容器
func (d *LXDDriver) ReinstallVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	_, _ = d.execLXC("delete", cfg.VmUUID, "--force")
	return d.CreateVM(cfg)
}

// MountHDD 挂载 / 卸载数据盘
func (d *LXDDriver) MountHDD(vmUUID string, hdd *model.SDConfig, mount bool) (*model.ZMessage, error) {
	devName := hdd.HddName
	if devName == "" {
		devName = "data"
	}

	if !mount {
		if out, err := d.execLXC("config", "device", "remove", vmUUID, devName); err != nil {
			return &model.ZMessage{Success: false, Action: "HDDMount",
				Message: fmt.Sprintf("卸载失败: %v (%s)", err, strings.TrimSpace(out))}, nil
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

	path := hdd.HddPath
	if path == "" {
		dir := d.Config.ExternPath
		if dir == "" {
			dir = d.Config.SystemPath
		}
		path = filepath.Join(dir, devName)
	}
	target := "/mnt/" + devName

	if out, err := d.execLXC("config", "device", "add", vmUUID, devName, "disk",
		"source="+path, "path="+target); err != nil {
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

// CreateBackup 创建备份（快照）
func (d *LXDDriver) CreateBackup(vmUUID string, tips string) (*model.ZMessage, error) {
	name := fmt.Sprintf("backup-%s", time.Now().Format("20060102-150405"))
	if out, err := d.execLXC("snapshot", vmUUID, name); err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: fmt.Sprintf("创建快照失败: %v (%s)", err, strings.TrimSpace(out))}, nil
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

// RestoreBackup 还原备份
func (d *LXDDriver) RestoreBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	if out, err := d.execLXC("restore", vmUUID, backupName); err != nil {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: fmt.Sprintf("还原失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}
	return &model.ZMessage{Success: true, Action: "Restores", Message: "备份还原成功"}, nil
}

// DeleteBackup 删除备份
func (d *LXDDriver) DeleteBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	if out, err := d.execLXC("delete", vmUUID+"/"+backupName); err != nil {
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

// SetPassword 设置容器密码（lxc exec chpasswd）
func (d *LXDDriver) SetPassword(vmUUID string, password string) (*model.ZMessage, error) {
	if d.GetPower(vmUUID) != "运行中" {
		return &model.ZMessage{Success: false, Action: "Password",
			Message: "容器未运行，无法设置密码"}, nil
	}

	user := "root"
	d.mu.RLock()
	if cfg, ok := d.VmSaving[vmUUID]; ok && cfg.OsUser != "" {
		user = cfg.OsUser
	}
	d.mu.RUnlock()

	out, err := runWithStdin(
		[]string{"lxc", "exec", vmUUID, "--", "chpasswd"},
		user+":"+password+"\n", 30*time.Second)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "Password",
			Message: fmt.Sprintf("设置密码失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		cfg.OsPass = password
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "Password", Message: "密码修改成功"}, nil
}

// Crontab 定时任务：同步容器状态
func (d *LXDDriver) Crontab() bool {
	vms, err := d.GetVMList()
	if err != nil {
		return false
	}
	d.mu.Lock()
	for name, cfg := range vms {
		if existing, ok := d.VmSaving[name]; ok {
			existing.VmFlag = cfg.VmFlag
		} else {
			d.VmSaving[name] = cfg
		}
	}
	d.mu.Unlock()
	return true
}
