// Package driver 各平台驱动实现
// 所有驱动使用 security.SafeExec 或 security.SafeShellExec 进行命令执行
package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/security"
)

// ============================================================
// VMware vSphere/ESXi 驱动（通过 HTTP API）
// ============================================================

// VSphereDriver VMware vSphere/ESXi 驱动
type VSphereDriver struct{ *BaseDriver }

func NewVSphereDriver(cfg *model.HSConfig) *VSphereDriver {
	return &VSphereDriver{BaseDriver: NewBaseDriver(cfg)}
}

func (d *VSphereDriver) GetPower(vmUUID string) string {
	// 通过 vSphere REST API 获取 VM 电源状态
	url := fmt.Sprintf("https://%s/rest/vcenter/vm/%s/power",
		d.Config.ServerAddr, vmUUID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return "未知"
	}
	req.SetBasicAuth(d.Config.ServerUser, d.Config.ServerPass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "未知"
	}
	defer resp.Body.Close()
	var result struct {
		Value struct {
			State string `json:"state"`
		} `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "未知"
	}
	switch result.Value.State {
	case "POWERED_ON":
		return "运行中"
	case "POWERED_OFF":
		return "已关机"
	case "SUSPENDED":
		return "已暂停"
	}
	return "未知"
}

func (d *VSphereDriver) GetStatus() (*model.HWStatus, error) {
	hw := &model.HWStatus{OnUpdate: time.Now().Unix()}
	url := fmt.Sprintf("https://%s/rest/vcenter/host", d.Config.ServerAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.SetBasicAuth(d.Config.ServerUser, d.Config.ServerPass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return hw, nil
	}
	defer resp.Body.Close()
	var result struct {
		Value []struct {
			PowerState string `json:"power_state"`
			Name       string `json:"name"`
		} `json:"value"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return hw, nil
}

// ============================================================
// LXC/LXD 驱动（通过 lxc 命令行）
// ============================================================

// LXDDriver LXC/LXD 驱动
type LXDDriver struct{ *BaseDriver }

func NewLXDDriver(cfg *model.HSConfig) *LXDDriver {
	return &LXDDriver{BaseDriver: NewBaseDriver(cfg)}
}

func (d *LXDDriver) GetPower(vmUUID string) string {
	out, _, err := security.SafeExecWithTimeout(
		[]string{"lxc", "info", vmUUID}, 10*time.Second)
	if err != nil {
		return "未知"
	}
	lower := strings.ToLower(out)
	if strings.Contains(lower, "status: running") {
		return "运行中"
	}
	if strings.Contains(lower, "status: stopped") {
		return "已关机"
	}
	if strings.Contains(lower, "status: frozen") {
		return "已暂停"
	}
	return "未知"
}

func (d *LXDDriver) GetStatus() (*model.HWStatus, error) {
	hw := &model.HWStatus{OnUpdate: time.Now().Unix()}
	out, _, err := security.SafeExecWithTimeout(
		[]string{"lxc", "storage", "info", "default"}, 10*time.Second)
	if err != nil {
		return hw, nil
	}
	_ = out
	return hw, nil
}

// ============================================================
// Docker/OCI 驱动（通过 docker 命令行）
// ============================================================

// DockerDriver Docker/OCI 驱动
type DockerDriver struct{ *BaseDriver }

func NewDockerDriver(cfg *model.HSConfig) *DockerDriver {
	return &DockerDriver{BaseDriver: NewBaseDriver(cfg)}
}

func (d *DockerDriver) GetPower(vmUUID string) string {
	out, _, err := security.SafeExecWithTimeout(
		[]string{"docker", "inspect", "--format={{.State.Status}}", vmUUID},
		10*time.Second)
	if err != nil {
		return "未知"
	}
	switch strings.TrimSpace(out) {
	case "running":
		return "运行中"
	case "exited", "stopped":
		return "已关机"
	case "paused":
		return "已暂停"
	}
	return "未知"
}

func (d *DockerDriver) GetStatus() (*model.HWStatus, error) {
	hw := &model.HWStatus{OnUpdate: time.Now().Unix()}
	out, _, err := security.SafeExecWithTimeout(
		[]string{"docker", "info", "--format={{.ContainersRunning}}"}, 10*time.Second)
	if err != nil {
		return hw, nil
	}
	fmt.Sscanf(strings.TrimSpace(out), "%d", &hw.CpuTotal)
	return hw, nil
}

func (d *DockerDriver) GetVMList() (map[string]*model.VMConfig, error) {
	out, _, err := security.SafeExecWithTimeout(
		[]string{"docker", "ps", "-a", "--format={{.ID}}\t{{.Names}}\t{{.Status}}"},
		10*time.Second)
	if err != nil {
		return d.BaseDriver.GetVMList()
	}
	vms := d.GetVMSaving()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		id, name, status := parts[0], parts[1], parts[2]
		if cfg, ok := vms[id]; ok {
			if strings.HasPrefix(status, "Up") {
				cfg.VmFlag = model.VMPowerStarted
			} else if strings.HasPrefix(status, "Exited") {
				cfg.VmFlag = model.VMPowerStopped
			}
		} else {
			cfg := model.NewVMConfig()
			cfg.VmUUID = id
			cfg.OsName = name
			if strings.HasPrefix(status, "Up") {
				cfg.VmFlag = model.VMPowerStarted
			}
			vms[id] = cfg
		}
	}
	return vms, nil
}

// ============================================================
// Hyper-V 驱动（通过 PowerShell 命令行，仅 Windows）
// ============================================================

// HyperVDriver Hyper-V 驱动
type HyperVDriver struct{ *BaseDriver }

func NewHyperVDriver(cfg *model.HSConfig) *HyperVDriver {
	return &HyperVDriver{BaseDriver: NewBaseDriver(cfg)}
}

func (d *HyperVDriver) GetPower(vmUUID string) string {
	out, _, err := security.SafeExecWithTimeout(
		[]string{"powershell", "-Command",
			fmt.Sprintf("(Get-VM -Id '%s').State", vmUUID)},
		15*time.Second)
	if err != nil {
		return "未知"
	}
	switch strings.TrimSpace(out) {
	case "Running":
		return "运行中"
	case "Off":
		return "已关机"
	case "Paused":
		return "已暂停"
	}
	return "未知"
}

func (d *HyperVDriver) GetVMList() (map[string]*model.VMConfig, error) {
	out, _, err := security.SafeExecWithTimeout(
		[]string{"powershell", "-Command",
			"Get-VM | Select-Object Id,Name,State | ConvertTo-Json -Compress"},
		15*time.Second)
	if err != nil {
		return d.BaseDriver.GetVMList()
	}
	var vms []struct {
		Id    string `json:"Id"`
		Name  string `json:"Name"`
		State string `json:"State"`
	}
	if err := json.Unmarshal([]byte(out), &vms); err != nil {
		return d.BaseDriver.GetVMList()
	}
	result := d.GetVMSaving()
	for _, v := range vms {
		if cfg, ok := result[v.Id]; ok {
			switch v.State {
			case "Running":
				cfg.VmFlag = model.VMPowerStarted
			case "Off":
				cfg.VmFlag = model.VMPowerStopped
			}
		} else {
			cfg := model.NewVMConfig()
			cfg.VmUUID = v.Id
			cfg.OsName = v.Name
			switch v.State {
			case "Running":
				cfg.VmFlag = model.VMPowerStarted
			case "Off":
				cfg.VmFlag = model.VMPowerStopped
			}
			result[v.Id] = cfg
		}
	}
	return result, nil
}

// ============================================================
// QEMU/KVM 驱动（通过 virsh 命令行）
// ============================================================

// QEMUDriver QEMU/KVM 驱动
type QEMUDriver struct{ *BaseDriver }

func NewQEMUDriver(cfg *model.HSConfig) *QEMUDriver {
	return &QEMUDriver{BaseDriver: NewBaseDriver(cfg)}
}

func (d *QEMUDriver) GetPower(vmUUID string) string {
	out, _, err := security.SafeExecWithTimeout(
		[]string{"virsh", "domstate", vmUUID}, 10*time.Second)
	if err != nil {
		return "未知"
	}
	switch strings.TrimSpace(out) {
	case "running":
		return "运行中"
	case "shut off":
		return "已关机"
	case "paused":
		return "已暂停"
	}
	return "未知"
}

func (d *QEMUDriver) PowerControl(vmUUID string, action model.VMPowers) (*model.ZMessage, error) {
	var cmd []string
	switch action {
	case model.VMPowerSStart:
		cmd = []string{"virsh", "start", vmUUID}
	case model.VMPowerSClose:
		cmd = []string{"virsh", "shutdown", vmUUID}
	case model.VMPowerHClose:
		cmd = []string{"virsh", "destroy", vmUUID}
	case model.VMPowerSReset:
		cmd = []string{"virsh", "reboot", vmUUID}
	case model.VMPowerAPause:
		cmd = []string{"virsh", "suspend", vmUUID}
	case model.VMPowerAWaked:
		cmd = []string{"virsh", "resume", vmUUID}
	default:
		return d.BaseDriver.PowerControl(vmUUID, action)
	}
	_, _, err := security.SafeExecWithTimeout(cmd, 30*time.Second)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMPowers", Message: err.Error()}, nil
	}
	return d.BaseDriver.PowerControl(vmUUID, action)
}

func (d *QEMUDriver) GetScreenshot(vmUUID string) (string, error) {
	out, _, err := security.SafeExecWithTimeout(
		[]string{"virsh", "screenshot", vmUUID, "--file", "/dev/stdout"},
		10*time.Second)
	if err != nil {
		return "", err
	}
	return out, nil
}

// ============================================================
// VMware Workstation 驱动（通过 REST API）
// ============================================================

// WorkstationDriver VMware Workstation 驱动
type WorkstationDriver struct{ *BaseDriver }

func NewWorkstationDriver(cfg *model.HSConfig) *WorkstationDriver {
	return &WorkstationDriver{BaseDriver: NewBaseDriver(cfg)}
}

func (d *WorkstationDriver) GetPower(vmUUID string) string {
	url := fmt.Sprintf("http://%s/api/vms/%s/power",
		d.Config.ServerAddr, vmUUID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.SetBasicAuth(d.Config.ServerUser, d.Config.ServerPass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "未知"
	}
	defer resp.Body.Close()
	var result struct {
		PowerState string `json:"power_state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "未知"
	}
	switch result.PowerState {
	case "poweredOn":
		return "运行中"
	case "poweredOff":
		return "已关机"
	case "suspended":
		return "已暂停"
	}
	return "未知"
}

// ============================================================
// VirtualBox 驱动（通过 VBoxManage 命令行）
// ============================================================

// VirtualBoxDriver VirtualBox 驱动
type VirtualBoxDriver struct{ *BaseDriver }

func NewVirtualBoxDriver(cfg *model.HSConfig) *VirtualBoxDriver {
	return &VirtualBoxDriver{BaseDriver: NewBaseDriver(cfg)}
}

func (d *VirtualBoxDriver) GetPower(vmUUID string) string {
	out, _, err := security.SafeExecWithTimeout(
		[]string{"VBoxManage", "showvminfo", vmUUID, "--machinereadable"},
		10*time.Second)
	if err != nil {
		return "未知"
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "VMState=") {
			state := strings.Trim(strings.TrimPrefix(line, "VMState="), "\"")
			switch state {
			case "running":
				return "运行中"
			case "poweroff":
				return "已关机"
			case "paused":
				return "已暂停"
			case "saved":
				return "已暂停"
			}
		}
	}
	return "未知"
}

func (d *VirtualBoxDriver) GetVMList() (map[string]*model.VMConfig, error) {
	out, _, err := security.SafeExecWithTimeout(
		[]string{"VBoxManage", "list", "vms"}, 10*time.Second)
	if err != nil {
		return d.BaseDriver.GetVMList()
	}
	result := d.GetVMSaving()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		// 格式: "vm_name" {vm_uuid}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) < 2 {
			continue
		}
		name := strings.Trim(parts[0], "\"")
		uuid := strings.Trim(parts[1], "{}")
		if cfg, ok := result[uuid]; ok {
			cfg.OsName = name
		} else {
			cfg := model.NewVMConfig()
			cfg.VmUUID = uuid
			cfg.OsName = name
			result[uuid] = cfg
		}
	}
	// 批量获取状态
	for uuid := range result {
		power := d.GetPower(uuid)
		if power == "运行中" {
			result[uuid].VmFlag = model.VMPowerStarted
		}
	}
	return result, nil
}

// ============================================================
// MEmu Android 驱动（通过 memuc 命令行）
// ============================================================

// MemuDriver MEmu Android 驱动
type MemuDriver struct{ *BaseDriver }

func NewMemuDriver(cfg *model.HSConfig) *MemuDriver {
	return &MemuDriver{BaseDriver: NewBaseDriver(cfg)}
}

func (d *MemuDriver) GetPower(vmUUID string) string {
	out, _, err := security.SafeExecWithTimeout(
		[]string{"memuc", "listvms"}, 10*time.Second)
	if err != nil {
		return "未知"
	}
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == vmUUID {
			switch fields[2] {
			case "运行中":
				return "运行中"
			case "已关闭":
				return "已关机"
			}
		}
	}
	return "未知"
}

// ============================================================
// 青州云 驱动（通过 HTTP API）
// ============================================================

// QingzhouDriver 青州云 驱动
type QingzhouDriver struct{ *BaseDriver }

func NewQingzhouDriver(cfg *model.HSConfig) *QingzhouDriver {
	return &QingzhouDriver{BaseDriver: NewBaseDriver(cfg)}
}

func (d *QingzhouDriver) GetPower(vmUUID string) string {
	url := fmt.Sprintf("http://%s/api/vm/%s/status",
		d.Config.ServerAddr, vmUUID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+d.Config.ServerPass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "未知"
	}
	defer resp.Body.Close()
	var result struct {
		Code int `json:"code"`
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "未知"
	}
	switch result.Data.Status {
	case "running", "active":
		return "运行中"
	case "stopped", "inactive":
		return "已关机"
	case "paused":
		return "已暂停"
	}
	return "未知"
}

// GetStatus 获取青州云主机状态
func (d *QingzhouDriver) GetStatus() (*model.HWStatus, error) {
	hw := &model.HWStatus{OnUpdate: time.Now().Unix()}
	url := fmt.Sprintf("http://%s/api/host/status", d.Config.ServerAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+d.Config.ServerPass)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return hw, nil
	}
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(hw)
	hw.OnUpdate = time.Now().Unix()
	return hw, nil
}