// Package driver 宿主机命令执行（本地 / SSH 双通道）。
//
// 对应 Python 版 PortForward.execute_command / IPTablesAPI.execute_iptables_command：
// 本地主机直接执行（带安全检查），远程主机通过 SSH 执行。
//
// 与 Python 一致的远程判定：
//   - server_addr 为 "" / localhost / 127.0.0.1 → 本地
//   - server_addr 以 "ssh://" 开头 → 视为 SSH 转发模式，按本地处理
//   - 其它 → 远程，走 SSH
package driver

import (
	"fmt"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/security"
)

// hostCommandRunner 在宿主机上执行命令
type hostCommandRunner struct {
	config *model.HSConfig
}

// newHostCommandRunner 创建命令执行器
func newHostCommandRunner(cfg *model.HSConfig) *hostCommandRunner {
	return &hostCommandRunner{config: cfg}
}

// isRemote 判断目标是否为远程主机（口径与 Python 一致）
func (r *hostCommandRunner) isRemote() bool {
	if r.config == nil {
		return false
	}
	addr := strings.TrimSpace(r.config.ServerAddr)
	if strings.HasPrefix(addr, "ssh://") {
		// SSH 转发模式：命令由本机的 ssh 转发通道处理，仍按本地执行
		return false
	}
	return !isLocalHost(addr)
}

// runShell 执行 shell 命令字符串。
//
// 本地：走 security.SafeShellExec（白名单 + 注入检查 + 允许管道）。
// 远程：通过 SSH 执行（Python 侧同样不做白名单校验）。
func (r *hostCommandRunner) runShell(cmd string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if r.isRemote() {
		res, err := RunSSHCommand(sshConfigFromHS(r.config), cmd)
		if err != nil {
			return "", err
		}
		if res == nil {
			return "", fmt.Errorf("远程命令无返回: %s", cmd)
		}
		if res.ExitCode != 0 {
			msg := strings.TrimSpace(res.Stderr)
			if msg == "" {
				msg = strings.TrimSpace(res.Stdout)
			}
			return res.Stdout, fmt.Errorf("远程命令执行失败（exit=%d）: %s", res.ExitCode, msg)
		}
		return res.Stdout, nil
	}
	out, _, err := security.SafeShellExec(cmd, timeout, true)
	if err != nil {
		return out, err
	}
	return out, nil
}

// runArgs 以参数数组形式执行命令（不经 shell，无注入风险）。
//
// 对应 Python 侧 IPTablesAPI 的本地实现（subprocess.run(list)）。
// 远程时按 Python 的做法逐参数 shell 转义后拼接执行。
func (r *hostCommandRunner) runArgs(args ...string) error {
	if len(args) == 0 {
		return fmt.Errorf("命令参数为空")
	}
	if r.isRemote() {
		parts := make([]string, 0, len(args))
		for _, a := range args {
			parts = append(parts, shellQuote(a))
		}
		_, err := r.runShell(strings.Join(parts, " "), 30*time.Second)
		return err
	}
	if _, _, err := security.SafeExecWithTimeout(args, 30*time.Second); err != nil {
		return err
	}
	return nil
}
