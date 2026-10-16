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
// LXC/LXD 驱动（通过 lxc 命令行）
// ============================================================

// 说明：vSphere/ESXi 引擎统一由 esxi_driver.go + esxi_ext.go 的 ESXiDriver 实现
// （基于 govmomi SDK，覆盖创建/删除/电源/快照/直通/截图等完整能力）。
// 历史上此处还有一个基于 vSphere REST 的 VSphereDriver 精简版，因从未被
// createDriver 注册（属死代码），且与 ESXiDriver 构成「同一引擎两套实现」的
// 隐患，已删除以免误用。

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

// GetStatus 获取 LXD 主机状态。
//
// 与 Python 版 LXContainer.HSStatus 一致：优先解析 `lxc info` 的
// CPU 核数与内存总量；原实现读取输出后直接丢弃，状态恒为空。
func (d *LXDDriver) GetStatus() (*model.HWStatus, error) {
	// 远程 LXD：必须通过 SSH 采集对端宿主机状态（与 Python 版一致），
	// 否则会把 IDCClient 本机资源误报为该主机状态。
	if isRemoteTarget(d.Config) {
		return d.remoteHWStatus(), nil
	}
	hw := &model.HWStatus{OnUpdate: time.Now().Unix()}
	out, _, err := security.SafeExecWithTimeout([]string{"lxc", "info"}, 15*time.Second)
	if err != nil {
		return localHWStatus(), nil
	}

	var inCPU, inMem bool
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "cpu_total") || strings.Contains(trimmed, "cpu_total"):
			inCPU = true
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(trimmed, "cpu_total:")), "%d", &hw.CpuTotal)
		case strings.HasPrefix(trimmed, "memory_total"):
			inMem = true
			var memBytes int64
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(trimmed, "memory_total:")), "%d", &memBytes)
			if memBytes > 0 {
				hw.MemTotal = int(memBytes / 1024 / 1024)
			}
		}
	}
	_ = inCPU
	_ = inMem
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

// GetStatus 获取 Docker 主机状态。
//
// 与 Python 版 OCInterface.HSStatus 一致：读取 docker info 的
// NCPU / MemTotal / Containers 等真实字段。
// 原实现把「运行中容器数」当成 CPU 核数写入 CpuTotal，属于字段错位。
func (d *DockerDriver) GetStatus() (*model.HWStatus, error) {
	// 远程 Docker：通过 SSH 采集对端宿主机状态（与 Python 版一致），
	// 否则会把 IDCClient 本机资源误报为该主机状态。
	if isRemoteTarget(d.Config) {
		return d.remoteHWStatus(), nil
	}
	hw := &model.HWStatus{OnUpdate: time.Now().Unix()}
	out, _, err := security.SafeExecWithTimeout([]string{
		"docker", "info", "--format",
		"{{.NCPU}}|{{.MemTotal}}|{{.Containers}}|{{.ContainersRunning}}|{{.OperatingSystem}}",
	}, 10*time.Second)
	if err != nil {
		// 本地 docker CLI 不可用时回退本机采集
		return localHWStatus(), nil
	}

	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) >= 2 {
		fmt.Sscanf(strings.TrimSpace(parts[0]), "%d", &hw.CpuTotal)
		var memBytes int64
		fmt.Sscanf(strings.TrimSpace(parts[1]), "%d", &memBytes)
		if memBytes > 0 {
			hw.MemTotal = int(memBytes / 1024 / 1024)
		}
	}
	if len(parts) >= 5 {
		hw.CpuModel = strings.TrimSpace(parts[4])
	}
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

// GetStatus 获取 QEMU/KVM 主机状态。
//
// 与 Python 版 QEMUService.HSStatus 一致：server_addr 为本地时用本机采集，
// 指向远程主机时通过 SSH 采集对端状态。
// 原实现直接走基类的本机采集，远程 QEMU 主机会把 IDCClient 自身资源当作其状态。
func (d *QEMUDriver) GetStatus() (*model.HWStatus, error) {
	return d.BaseDriver.statusForPlatform()
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

// GetScreenshot 截取虚拟机屏幕。
//
// 与 Python 版 QEMUService.VMScreen 一致：virsh screenshot 输出 PPM，
// 需转换后再返回。原实现写 --file /dev/stdout 会得到二进制 PPM 与命令输出混杂的
// 无效字符串，前端根本无法显示。
func (d *QEMUDriver) GetScreenshot(vmUUID string) (string, error) {
	if d.GetPower(vmUUID) != "运行中" {
		return "", fmt.Errorf("虚拟机未运行，无法截图")
	}
	return d.captureQEMUScreenshot(vmUUID)
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

// 注：MEmu 的 GetPower 由 memu_lifecycle.go 实现（memuc isvmrunning），
// 与 Python 版 MemuAndroid.GetPower 一致，此处不再重复定义。

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