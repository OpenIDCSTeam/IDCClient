// Package driver PortForward：基于 socat 的端口转发管理。
//
// 对应 Python 版 HostServer/OCInterfaceAPI/PortForward.py（socat 路径）：
//   - 通过 `nohup socat TCP-LISTEN:<wan>,reuseaddr,fork TCP:<container>:<lan> &`
//     建立转发进程
//   - 通过 `ps -ef | grep 'socat TCP\|socat UDP'` 解析现有转发与占用端口
//   - 通过 kill（必要时 kill -9）删除转发
//
// 说明：
//   - macvlan 直通方案（Python `add_port_forward_macvlan`）属于可选替代路径，
//     未在 PortsMap 主流程中使用，本实现未移植；
//   - 远程主机通过 SSH 执行，无需保持长连接（Go 每次调用独立建连，
//     与 Python 的「连接—执行—关闭」语义等价，且避免了连接泄漏）。
package driver

import (
	"fmt"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// PortForwardEntry 一条 socat 端口转发记录
type PortForwardEntry struct {
	WanPort  int    `json:"wan_port"`  // 外部端口
	LanAddr  string `json:"lan_addr"`  // 内部地址
	LanPort  int    `json:"lan_port"`  // 内部端口
	Protocol string `json:"protocol"`  // TCP / UDP
	Pid      int    `json:"pid"`       // socat 进程号
}

// portForward socat 端口转发管理器
type portForward struct {
	runner *hostCommandRunner
	config *model.HSConfig
}

// newPortForward 创建 socat 端口转发管理器
func newPortForward(cfg *model.HSConfig) *portForward {
	return &portForward{
		runner: newHostCommandRunner(cfg),
		config: cfg,
	}
}

var (
	reTCPListen = regexp.MustCompile(`TCP-LISTEN:(\d+)`)
	reUDPListen = regexp.MustCompile(`UDP-LISTEN:(\d+)`)
	reTCPTarget = regexp.MustCompile(`TCP:([0-9.]+):(\d+)`)
	reUDPTarget = regexp.MustCompile(`UDP:([0-9.]+):(\d+)`)
)

// listPorts 列出当前所有 socat 端口转发
func (p *portForward) listPorts() []PortForwardEntry {
	out, err := p.runner.runShell(
		`ps -ef | grep 'socat TCP\|socat UDP' | grep -v grep | grep -v watch`, 15*time.Second)
	if err != nil || strings.TrimSpace(out) == "" {
		return nil
	}
	return parseSocatPSOutput(out)
}

// parseSocatPSOutput 解析 `ps -ef` 中 socat 转发进程的输出。
//
// 行示例：
//
//	root 108104 1 0 15:10 ? 00:00:00 socat TCP-LISTEN:24203,reuseaddr,fork TCP:172.19.0.100:22
//
// 提取为纯函数以便单元测试（不依赖宿主机环境）。
func parseSocatPSOutput(out string) []PortForwardEntry {
	entries := make([]PortForwardEntry, 0, 4)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 8 {
			continue
		}

		// 定位独立的 socat 命令（排除路径中恰好含 socat 的情况）
		socatIdx := -1
		for i, part := range parts {
			if part == "socat" || strings.HasSuffix(part, "/socat") {
				socatIdx = i
				break
			}
		}
		if socatIdx == -1 || len(parts)-socatIdx < 3 {
			continue
		}

		listenPart := parts[socatIdx+1]  // TCP-LISTEN:24203,reuseaddr,fork
		forwardPart := parts[socatIdx+2] // TCP:172.19.0.100:22
		if !strings.Contains(listenPart, "LISTEN:") {
			continue
		}

		protocol := "TCP"
		wanPort := 0
		if m := reTCPListen.FindStringSubmatch(listenPart); m != nil {
			protocol = "TCP"
			wanPort, _ = strconv.Atoi(m[1])
		} else if m := reUDPListen.FindStringSubmatch(listenPart); m != nil {
			protocol = "UDP"
			wanPort, _ = strconv.Atoi(m[1])
		}
		if wanPort == 0 {
			continue
		}

		lanAddr := ""
		lanPort := 0
		if m := reTCPTarget.FindStringSubmatch(forwardPart); m != nil && protocol == "TCP" {
			lanAddr = m[1]
			lanPort, _ = strconv.Atoi(m[2])
		} else if m := reUDPTarget.FindStringSubmatch(forwardPart); m != nil && protocol == "UDP" {
			lanAddr = m[1]
			lanPort, _ = strconv.Atoi(m[2])
		}
		if lanAddr == "" || lanPort == 0 {
			continue
		}

		pid, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}

		entries = append(entries, PortForwardEntry{
			WanPort: wanPort, LanAddr: lanAddr, LanPort: lanPort,
			Protocol: protocol, Pid: pid,
		})
	}
	return entries
}

// hostPorts 返回已被 socat 占用的端口集合
func (p *portForward) hostPorts() map[int]bool {
	ports := make(map[int]bool)
	for _, e := range p.listPorts() {
		ports[e.WanPort] = true
	}
	return ports
}

// allocatePort 在配置的端口范围内随机分配一个未被 socat 占用的端口
func (p *portForward) allocatePort() int {
	start, end := p.config.PortsStart, p.config.PortsClose
	if start <= 0 || end <= start {
		start, end = 10000, 60000
	}
	wanPort := start + rand.Intn(end-start+1)
	existing := p.hostPorts()
	for i := 0; i < 100 && existing[wanPort]; i++ {
		wanPort = start + rand.Intn(end-start+1)
	}
	return wanPort
}

// addPortForward 添加 socat 转发并回查确认进程已启动
func (p *portForward) addPortForward(containerIP string, lanPort, wanPort int, protocol, vmName string) error {
	protocol = strings.ToUpper(protocol)
	if protocol != "TCP" && protocol != "UDP" {
		return fmt.Errorf("不支持的协议类型: %s", protocol)
	}

	cmd := fmt.Sprintf("nohup socat %s-LISTEN:%d,reuseaddr,fork %s:%s:%d > /dev/null 2>&1 &",
		protocol, wanPort, protocol, containerIP, lanPort)
	if _, err := p.runner.runShell(cmd, 15*time.Second); err != nil {
		return fmt.Errorf("启动socat失败: %w", err)
	}

	// 等待进程就绪后回查（与 Python 一致）
	time.Sleep(500 * time.Millisecond)
	for _, e := range p.listPorts() {
		if e.WanPort == wanPort && e.LanAddr == containerIP &&
			e.LanPort == lanPort && e.Protocol == protocol {
			return nil
		}
	}
	return fmt.Errorf("端口转发创建后未找到对应进程")
}

// removePortForward 删除指定端口（协议）的 socat 转发进程
func (p *portForward) removePortForward(wanPort int, protocol string) error {
	protocol = strings.ToUpper(protocol)

	pids := make([]int, 0, 2)
	for _, e := range p.listPorts() {
		if e.WanPort == wanPort && e.Protocol == protocol {
			pids = append(pids, e.Pid)
		}
	}
	if len(pids) == 0 {
		return fmt.Errorf("未找到端口 %d (%s) 的转发进程", wanPort, protocol)
	}

	var lastErr error
	for _, pid := range pids {
		if _, err := p.runner.runShell(fmt.Sprintf("kill %d", pid), 10*time.Second); err != nil {
			lastErr = fmt.Errorf("停止进程 %d 失败: %w", pid, err)
			continue
		}
		time.Sleep(500 * time.Millisecond)

		// 校验是否真正退出，未退出则强制终止
		out, _ := p.runner.runShell(fmt.Sprintf("ps -p %d -o pid=", pid), 10*time.Second)
		if strings.TrimSpace(out) == "" {
			continue
		}
		if _, err := p.runner.runShell(fmt.Sprintf("kill -9 %d", pid), 10*time.Second); err != nil {
			lastErr = fmt.Errorf("强制终止进程 %d 失败: %w", pid, err)
		}
	}
	return lastErr
}

// removeContainerForwards 删除指定容器地址的全部 socat 转发，返回删除数量
func (p *portForward) removeContainerForwards(containerIP string) int {
	removed := 0
	for _, e := range p.listPorts() {
		if e.LanAddr != containerIP {
			continue
		}
		if p.removePortForward(e.WanPort, e.Protocol) == nil {
			removed++
		}
	}
	return removed
}

// addPortForwardFirewall 通过 iptables 添加端口映射（LXD / SmolVM 使用）
func (p *portForward) addPortForwardFirewall(containerIP string, lanPort, wanPort int, vmName string) error {
	api := newIPTablesAPI(p.config)
	return api.addPortMapping(containerIP, lanPort, wanPort, vmName)
}

// removePortForwardFirewall 通过 iptables 删除端口映射
func (p *portForward) removePortForwardFirewall(containerIP string, lanPort, wanPort int) error {
	api := newIPTablesAPI(p.config)
	return api.removePortMapping(containerIP, lanPort, wanPort)
}

// hostPortsFirewall 返回 iptables 中已占用的端口集合
func (p *portForward) hostPortsFirewall() map[int]bool {
	api := newIPTablesAPI(p.config)
	return api.hostPorts()
}
