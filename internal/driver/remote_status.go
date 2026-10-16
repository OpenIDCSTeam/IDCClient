// Package driver 远程宿主机硬件状态采集（SSH）。
//
// 对应 Python 版 BasicServer.ssh_get_hw_status()：当本地型虚拟化平台
// （QEMU/KVM、LXD、Docker、SmolVM 等）的 server_addr 指向远程主机时，
// 必须通过 SSH 采集**对端**的 CPU/内存/磁盘状态。
//
// 此前的实现无条件调用 localHWStatus()，会把 IDCClient 所在机器的资源
// 误报为远程主机状态——前端看到的 CPU/内存/磁盘全部是错的。
//
// 同时实现了与 Python 一致的「连续失败熔断」：连续 3 次失败进入 300 秒冷却，
// 避免对端 sshd 因高频失败触发 MaxStartups / fail2ban 防护。
package driver

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

const (
	// 连续失败阈值与冷却时长（与 Python 一致）
	remoteStatusFailThreshold = 3
	remoteStatusCooldown      = 300 * time.Second
)

// remoteStatusState 单主机的 SSH 采集熔断状态
type remoteStatusState struct {
	mu            sync.Mutex
	failCount     int
	cooldownUntil time.Time
}

var (
	remoteStatusMu    sync.Mutex
	remoteStatusStateMap = map[string]*remoteStatusState{}
)

func remoteStatusStateFor(key string) *remoteStatusState {
	remoteStatusMu.Lock()
	defer remoteStatusMu.Unlock()
	st, ok := remoteStatusStateMap[key]
	if !ok {
		st = &remoteStatusState{}
		remoteStatusStateMap[key] = st
	}
	return st
}

// stripSSHScheme 去掉 server_addr 上的 "ssh://" 前缀（Python 亦做同样处理）
func stripSSHScheme(addr string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(addr), "ssh://"))
}

// isRemoteTarget 判断主机配置指向的是远程机器
func isRemoteTarget(cfg *model.HSConfig) bool {
	if cfg == nil {
		return false
	}
	return !isLocalHost(stripSSHScheme(cfg.ServerAddr))
}

// remoteHWStatus 通过 SSH 采集远程宿主机状态。
//
// 采集命令与 Python 版一致：
//   - nproc            → CPU 核心数
//   - /proc/stat ×2    → CPU 使用率（间隔 1 秒，兼容所有发行版）
//   - /proc/meminfo    → 内存
//   - /proc/cpuinfo    → CPU 型号
//   - df -k /          → 根分区
//
// 采集失败（含熔断冷却期）返回空状态，绝不用本机资源顶替。
func (b *BaseDriver) remoteHWStatus() *model.HWStatus {
	cfg := b.Config
	if cfg == nil {
		return emptyHWStatus()
	}

	addr := stripSSHScheme(cfg.ServerAddr)
	hostKey := addr + ":" + cfg.ServerName
	st := remoteStatusStateFor(hostKey)

	st.mu.Lock()
	if !st.cooldownUntil.IsZero() && time.Now().Before(st.cooldownUntil) {
		st.mu.Unlock()
		return emptyHWStatus()
	}
	st.mu.Unlock()

	sshCfg := sshConfigFromHS(cfg)
	sshCfg.Host = addr

	hw := &model.HWStatus{
		OnUpdate: time.Now().Unix(),
		ExtUsage: map[string][]int{},
		GpuUsage: map[string]float64{},
	}

	fail := func() *model.HWStatus {
		st.mu.Lock()
		st.failCount++
		if st.failCount >= remoteStatusFailThreshold {
			st.cooldownUntil = time.Now().Add(remoteStatusCooldown)
			st.failCount = 0
		}
		st.mu.Unlock()
		return emptyHWStatus()
	}

	// 连通性探测 + CPU 核心数
	res, err := RunSSHCommand(sshCfg, "nproc")
	if err != nil || res == nil || res.ExitCode != 0 {
		return fail()
	}
	// 连接成功：重置熔断状态
	st.mu.Lock()
	st.failCount = 0
	st.cooldownUntil = time.Time{}
	st.mu.Unlock()

	if n := strings.TrimSpace(res.Stdout); n != "" {
		fmt.Sscanf(n, "%d", &hw.CpuTotal)
	}

	// CPU 使用率：两次读取 /proc/stat 求差
	if cpu := sshCPUUsage(sshCfg); cpu >= 0 {
		hw.CpuUsage = cpu
	}

	// CPU 型号
	if res, err := RunSSHCommand(sshCfg, "grep -m1 'model name' /proc/cpuinfo"); err == nil && res != nil {
		if idx := strings.Index(res.Stdout, ":"); idx >= 0 {
			hw.CpuModel = strings.TrimSpace(res.Stdout[idx+1:])
		}
	}

	// 内存（/proc/meminfo，单位 KB）
	if res, err := RunSSHCommand(sshCfg, "grep -E '^(MemTotal|MemAvailable):' /proc/meminfo"); err == nil && res != nil {
		for _, line := range strings.Split(res.Stdout, "\n") {
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

	// 根分区
	if res, err := RunSSHCommand(sshCfg, "df -k /"); err == nil && res != nil {
		lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
		if len(lines) >= 2 {
			fields := strings.Fields(lines[len(lines)-1])
			if len(fields) >= 3 {
				var totalKB, usedKB int64
				fmt.Sscanf(fields[1], "%d", &totalKB)
				fmt.Sscanf(fields[2], "%d", &usedKB)
				hw.HddTotal = int(totalKB / 1024)
				hw.HddUsage = int(usedKB / 1024)
			}
		}
	}

	// 数据盘（Linux 挂载点）—— 与 Python 一致地列出非根分区
	if res, err := RunSSHCommand(sshCfg, "df -kP | awk 'NR>1 && $6!=\"/\" {print $6\" \"$2\" \"$3}'"); err == nil && res != nil {
		for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 3 {
				continue
			}
			var totalKB, usedKB int
			fmt.Sscanf(fields[1], "%d", &totalKB)
			fmt.Sscanf(fields[2], "%d", &usedKB)
			hw.ExtUsage[fields[0]] = []int{totalKB / 1024, usedKB / 1024}
		}
	}

	// 采集结果全 0 同样视为失败（避免把空数据写入历史）
	if hw.CpuTotal == 0 && hw.MemTotal == 0 {
		return fail()
	}

	return hw
}

// sshCPUUsage 通过两次 /proc/stat 采样计算 CPU 使用率（返回 -1 表示失败）
func sshCPUUsage(cfg SSHExecConfig) int {
	res1, err := RunSSHCommand(cfg, "head -1 /proc/stat")
	if err != nil || res1 == nil {
		return -1
	}
	time.Sleep(time.Second)
	res2, err := RunSSHCommand(cfg, "head -1 /proc/stat")
	if err != nil || res2 == nil {
		return -1
	}

	idle1, total1, ok1 := parseProcStat(res1.Stdout)
	idle2, total2, ok2 := parseProcStat(res2.Stdout)
	if !ok1 || !ok2 || total2 <= total1 {
		return -1
	}
	deltaTotal := total2 - total1
	deltaIdle := idle2 - idle1
	if deltaTotal <= 0 {
		return -1
	}
	usage := int((deltaTotal - deltaIdle) * 100 / deltaTotal)
	if usage < 0 {
		usage = 0
	}
	if usage > 100 {
		usage = 100
	}
	return usage
}

// parseProcStat 解析 /proc/stat 首行，返回 (idle+iowait, total)
func parseProcStat(line string) (int64, int64, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "cpu") {
		return 0, 0, false
	}
	fields := strings.Fields(line)
	if len(fields) < 8 {
		return 0, 0, false
	}
	var total, idle int64
	for i, f := range fields[1:] {
		var v int64
		if _, err := fmt.Sscanf(f, "%d", &v); err != nil {
			continue
		}
		total += v
		if i == 3 || i == 4 { // idle + iowait
			idle += v
		}
	}
	return idle, total, true
}

// statusForPlatform 按「本地 / 远程」选择采集方式。
//
// 本地型虚拟化平台（QEMU / LXD / Docker / SmolVM）在 server_addr 指向远程时
// 必须走 SSH，否则会把 IDCClient 本机资源误报为远程主机状态。
func (b *BaseDriver) statusForPlatform() (*model.HWStatus, error) {
	if isRemoteTarget(b.Config) {
		return b.remoteHWStatus(), nil
	}
	return localHWStatus(), nil
}
