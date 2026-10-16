// Package driver Docker/OCI 驱动生命周期实现（docker 命令行）
// 与 Python 版 HostServer/OCInterface.py 的功能语义对齐。
package driver

import (
	"fmt"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/security"
)

// execDocker 执行 docker 命令
func (d *DockerDriver) execDocker(args ...string) (string, error) {
	full := append([]string{"docker"}, args...)
	out, _, err := security.SafeExecWithTimeout(full, 5*time.Minute)
	return out, err
}

// ScanVMs 扫描并同步容器列表
func (d *DockerDriver) ScanVMs() (*model.ZMessage, error) {
	vms, err := d.GetVMList()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMDetect", Message: err.Error()}, nil
	}

	prefix := d.Config.FilterName
	d.mu.Lock()
	added := 0
	for id, cfg := range vms {
		if prefix != "" && !strings.HasPrefix(cfg.OsName, prefix) {
			continue
		}
		if _, exists := d.VmSaving[id]; !exists {
			d.VmSaving[id] = cfg
			added++
		}
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDetect",
		Message: fmt.Sprintf("扫描完成：共 %d 个，新增 %d 个", len(vms), added)}, nil
}

// PowerControl 电源控制
func (d *DockerDriver) PowerControl(vmUUID string, action model.VMPowers) (*model.ZMessage, error) {
	var args []string
	switch action {
	case model.VMPowerSStart:
		args = []string{"start", vmUUID}
	case model.VMPowerSClose:
		args = []string{"stop", vmUUID}
	case model.VMPowerHClose:
		args = []string{"kill", vmUUID}
	case model.VMPowerSReset, model.VMPowerHReset:
		args = []string{"restart", vmUUID}
	case model.VMPowerAPause:
		args = []string{"pause", vmUUID}
	case model.VMPowerAWaked:
		args = []string{"unpause", vmUUID}
	default:
		return d.BaseDriver.PowerControl(vmUUID, action)
	}

	if out, err := d.execDocker(args...); err != nil {
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
func (d *DockerDriver) CreateVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	image := cfg.OsName
	if image == "" {
		image = "alpine:latest"
	}
	cpu := cfg.CpuNum
	if cpu <= 0 {
		cpu = 1
	}
	mem := cfg.MemNum
	if mem <= 0 {
		mem = 512
	}

	args := []string{
		"create", "--name", cfg.VmUUID,
		"--cpus", fmt.Sprintf("%d", cpu),
		"--memory", fmt.Sprintf("%dm", mem),
	}
	if d.Config.NetworkNat != "" {
		args = append(args, "--network", d.Config.NetworkNat)
	} else {
		args = append(args, "--network", "bridge")
	}
	args = append(args, image)

	if out, err := d.execDocker(args...); err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: fmt.Sprintf("创建容器失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}
	if out, err := d.execDocker("start", cfg.VmUUID); err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: fmt.Sprintf("启动容器失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	cfg.VmFlag = model.VMPowerStarted
	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMCreate", Message: "容器创建成功"}, nil
}

// DeleteVM 删除容器
func (d *DockerDriver) DeleteVM(vmUUID string, removeBackup bool) (*model.ZMessage, error) {
	args := []string{"rm", "-f"}
	if removeBackup {
		args = append(args, "-v")
	}
	args = append(args, vmUUID)

	if out, err := d.execDocker(args...); err != nil {
		return &model.ZMessage{Success: false, Action: "VMDelete",
			Message: fmt.Sprintf("删除失败: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	d.mu.Lock()
	delete(d.VmSaving, vmUUID)
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDelete", Message: "容器删除成功"}, nil
}

// UpdateVM 更新容器资源限制
func (d *DockerDriver) UpdateVM(cfg *model.VMConfig, oldCfg *model.VMConfig) (*model.ZMessage, error) {
	var args []string
	if cfg.CpuNum != oldCfg.CpuNum && cfg.CpuNum > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%d", cfg.CpuNum))
	}
	if cfg.MemNum != oldCfg.MemNum && cfg.MemNum > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dm", cfg.MemNum))
	}
	if len(args) > 0 {
		full := append([]string{"update"}, args...)
		full = append(full, cfg.VmUUID)
		if out, err := d.execDocker(full...); err != nil {
			return &model.ZMessage{Success: false, Action: "VMUpdate",
				Message: fmt.Sprintf("更新失败: %v (%s)", err, strings.TrimSpace(out))}, nil
		}
	}

	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMUpdate", Message: "容器配置更新成功"}, nil
}

// ReinstallVM 重装容器（删除后按原配置重建）
func (d *DockerDriver) ReinstallVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	_, _ = d.execDocker("rm", "-f", cfg.VmUUID)
	return d.CreateVM(cfg)
}

// MountHDD 挂载 / 卸载数据卷
func (d *DockerDriver) MountHDD(vmUUID string, hdd *model.SDConfig, mount bool) (*model.ZMessage, error) {
	if hdd == nil || strings.TrimSpace(hdd.HddName) == "" {
		return &model.ZMessage{Success: false, Action: "HDDMount", Message: "磁盘信息不完整"}, nil
	}
	if _, err := d.GetVMDetail(vmUUID); err != nil {
		return &model.ZMessage{Success: false, Action: "HDDMount", Message: "容器不存在"}, nil
	}

	// Docker 数据卷需在创建时绑定，运行期无法持久追加；
	// 这里只登记配置，并明确告知需重建容器才生效（与 Python 版基类行为一致）。
	hdd.HddFlag = 0
	if mount {
		hdd.HddFlag = 1
	}
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		if cfg.HddAll == nil {
			cfg.HddAll = make(map[string]model.SDConfig)
		}
		cfg.HddAll[hdd.HddName] = *hdd
	}
	d.mu.Unlock()

	action := "挂载"
	if !mount {
		action = "卸载"
	}
	return &model.ZMessage{Success: true, Action: "HDDMount",
		Message: "容器数据盘" + action + "已记录（需重建容器生效）"}, nil
}

// CreateBackup 创建备份（提交镜像）
func (d *DockerDriver) CreateBackup(vmUUID string, tips string) (*model.ZMessage, error) {
	name := fmt.Sprintf("%s-backup-%s", vmUUID, time.Now().Format("20060102-150405"))
	if out, err := d.execDocker("commit", vmUUID, name); err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: fmt.Sprintf("提交镜像失败: %v (%s)", err, strings.TrimSpace(out))}, nil
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

// DeleteBackup 删除备份（删除镜像）
func (d *DockerDriver) DeleteBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	if out, err := d.execDocker("rmi", backupName); err != nil {
		return &model.ZMessage{Success: false, Action: "RMBackup",
			Message: fmt.Sprintf("删除镜像失败: %v (%s)", err, strings.TrimSpace(out))}, nil
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

// SetPassword 设置容器密码（docker exec chpasswd）
func (d *DockerDriver) SetPassword(vmUUID string, password string) (*model.ZMessage, error) {
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
		[]string{"docker", "exec", "-i", vmUUID, "chpasswd"},
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
func (d *DockerDriver) Crontab() bool {
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
