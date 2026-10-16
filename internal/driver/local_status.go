// Package driver 本机硬件状态采集
//
// 对应 Python 版 BasicServer.local_get_hw_status()（psutil 实现）。
// 用于「驱动运行在 IDCClient 所在机器」的平台（Hyper-V / QEMU / VirtualBox /
// MEmu / VMware Workstation 等）。
//
// 注意：远程平台（Proxmox / ESXi / 青州云）必须使用各自 API 的实现，
// 否则会把 IDCClient 本机的资源误报为远程主机状态。
package driver

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/security"
)

// localStatusCacheTTL 本机状态缓存时间。
//
// 前端每 10 秒轮询一次主机状态，而本机采集需要执行系统命令
// （Windows 下 PowerShell 单次约 3 秒），因此缓存 30 秒以降低开销。
const localStatusCacheTTL = 30 * time.Second

var (
	localStatusMu     sync.Mutex
	localStatusCache  *model.HWStatus
	localStatusCached time.Time
)

// localHWStatus 采集本机硬件状态（带短缓存）
func localHWStatus() *model.HWStatus {
	localStatusMu.Lock()
	defer localStatusMu.Unlock()

	if localStatusCache != nil && time.Since(localStatusCached) < localStatusCacheTTL {
		snapshot := *localStatusCache
		return &snapshot
	}

	hw := &model.HWStatus{OnUpdate: time.Now().Unix()}
	switch runtime.GOOS {
	case "windows":
		fillLocalStatusWindows(hw)
	case "linux":
		fillLocalStatusLinux(hw)
	case "darwin":
		fillLocalStatusDarwin(hw)
	}
	if hw.ExtUsage == nil {
		hw.ExtUsage = map[string][]int{}
	}
	if hw.GpuUsage == nil {
		hw.GpuUsage = map[string]float64{}
	}

	localStatusCache = hw
	localStatusCached = time.Now()
	snapshot := *hw
	return &snapshot
}

// emptyHWStatus 返回仅带时间戳的空状态。
//
// 供「远程平台」采集失败时使用：远程平台的失败不能用本机资源顶上，
// 否则会把 IDCClient 所在机器的 CPU/内存显示为远程主机状态。
func emptyHWStatus() *model.HWStatus {
	return &model.HWStatus{
		OnUpdate: time.Now().Unix(),
		ExtUsage: map[string][]int{},
		GpuUsage: map[string]float64{},
	}
}

// fillLocalStatusLinux 通过 /proc 与 statfs 采集（无外部依赖）
func fillLocalStatusLinux(hw *model.HWStatus) {
	// CPU 使用率
	if data, err := os.ReadFile("/proc/stat"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "cpu ") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 8 {
				break
			}
			var total, idle int64
			for i, f := range fields[1:] {
				var v int64
				fmt.Sscanf(f, "%d", &v)
				total += v
				if i == 3 || i == 4 { // idle + iowait
					idle += v
				}
			}
			if total > 0 {
				hw.CpuUsage = int((total - idle) * 100 / total)
			}
			break
		}
	}

	// CPU 核数与型号
	if data, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		cores := 0
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "processor") {
				cores++
			}
			if hw.CpuModel == "" && strings.HasPrefix(line, "model name") {
				if idx := strings.Index(line, ":"); idx >= 0 {
					hw.CpuModel = strings.TrimSpace(line[idx+1:])
				}
			}
		}
		hw.CpuTotal = cores
	}

	// 内存
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			var kb int64
			fmt.Sscanf(fields[1], "%d", &kb)
			switch strings.TrimSuffix(fields[0], ":") {
			case "MemTotal":
				hw.MemTotal = int(kb / 1024)
			case "MemAvailable":
				if hw.MemTotal > 0 {
					hw.MemUsage = hw.MemTotal - int(kb/1024)
				}
			}
		}
	}

	// 根分区（使用 df，避免依赖平台相关的 syscall 结构体）
	fillDiskViaDF(hw)
}

// fillDiskViaDF 通过 `df -k /` 采集根分区用量（Linux / macOS 通用）
func fillDiskViaDF(hw *model.HWStatus) {
	out, _, err := security.SafeExecWithTimeout([]string{"df", "-k", "/"}, 5*time.Second)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 3 {
		return
	}
	var totalKB, usedKB int64
	fmt.Sscanf(fields[1], "%d", &totalKB)
	fmt.Sscanf(fields[2], "%d", &usedKB)
	hw.HddTotal = int(totalKB / 1024)
	hw.HddUsage = int(usedKB / 1024)
}

// fillLocalStatusWindows 通过 PowerShell + CIM 采集
func fillLocalStatusWindows(hw *model.HWStatus) {
	script := `$ErrorActionPreference='SilentlyContinue'; ` +
		`$os = Get-CimInstance Win32_OperatingSystem; ` +
		`$cpu = Get-CimInstance Win32_Processor | Select-Object -First 1; ` +
		`$disk = Get-CimInstance Win32_LogicalDisk -Filter "DeviceID='C:'"; ` +
		`[pscustomobject]@{ ` +
		`cpu_total=$cpu.NumberOfLogicalProcessors; ` +
		`cpu_model=$cpu.Name; ` +
		`cpu_usage=$cpu.LoadPercentage; ` +
		`mem_total=[math]::Round($os.TotalVisibleMemorySize/1024); ` +
		`mem_usage=[math]::Round(($os.TotalVisibleMemorySize-$os.FreePhysicalMemory)/1024); ` +
		`hdd_total=[math]::Round($disk.Size/1MB); ` +
		`hdd_usage=[math]::Round(($disk.Size-$disk.FreeSpace)/1MB) ` +
		`} | ConvertTo-Json -Compress`

	out, _, err := security.SafeExecWithTimeout(
		[]string{"powershell", "-NoProfile", "-NonInteractive", "-Command", script},
		25*time.Second)
	if err != nil {
		return
	}

	var parsed struct {
		CpuTotal int    `json:"cpu_total"`
		CpuModel string `json:"cpu_model"`
		CpuUsage int    `json:"cpu_usage"`
		MemTotal int    `json:"mem_total"`
		MemUsage int    `json:"mem_usage"`
		HddTotal int    `json:"hdd_total"`
		HddUsage int    `json:"hdd_usage"`
	}
	body := strings.TrimSpace(out)
	if body == "" {
		return
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return
	}
	hw.CpuTotal = parsed.CpuTotal
	hw.CpuModel = parsed.CpuModel
	hw.CpuUsage = parsed.CpuUsage
	hw.MemTotal = parsed.MemTotal
	hw.MemUsage = parsed.MemUsage
	hw.HddTotal = parsed.HddTotal
	hw.HddUsage = parsed.HddUsage
}

// fillLocalStatusDarwin 通过 sysctl 与 df 采集
func fillLocalStatusDarwin(hw *model.HWStatus) {
	if out, _, err := security.SafeExecWithTimeout([]string{"sysctl", "-n", "hw.ncpu"}, 5*time.Second); err == nil {
		fmt.Sscanf(strings.TrimSpace(out), "%d", &hw.CpuTotal)
	}
	if out, _, err := security.SafeExecWithTimeout([]string{"sysctl", "-n", "machdep.cpu.brand_string"}, 5*time.Second); err == nil {
		hw.CpuModel = strings.TrimSpace(out)
	}
	if out, _, err := security.SafeExecWithTimeout([]string{"sysctl", "-n", "hw.memsize"}, 5*time.Second); err == nil {
		var bytes int64
		fmt.Sscanf(strings.TrimSpace(out), "%d", &bytes)
		if bytes > 0 {
			hw.MemTotal = int(bytes / 1024 / 1024)
		}
	}
	fillDiskViaDF(hw)
}
