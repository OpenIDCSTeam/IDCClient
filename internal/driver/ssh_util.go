package driver

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// SSHExecConfig SSH 执行参数
type SSHExecConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Timeout  time.Duration
}

// sshExecResult SSH 命令执行结果
type sshExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// RunSSHCommand 通过 SSH 执行一条命令（阻塞至执行结束）
//
// 用于需要在 PVE 节点上执行 qm 等命令行工具的场景：
// Proxmox 的 REST API 无法执行任意 shell 命令，远程模式下必须走 SSH。
func RunSSHCommand(cfg SSHExecConfig, cmd string) (*sshExecResult, error) {
	if strings.TrimSpace(cfg.Host) == "" {
		return nil, fmt.Errorf("SSH 主机地址为空")
	}
	if cfg.Port <= 0 {
		cfg.Port = 22
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if strings.TrimSpace(cfg.User) == "" {
		cfg.User = "root"
	}

	clientCfg := &ssh.ClientConfig{
		User: cfg.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(cfg.Password),
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = cfg.Password
				}
				return answers, nil
			}),
		},
		// PVE 节点普遍使用自签名证书与未知 host key，
		// 与项目整体「默认不校验下游平台」的策略保持一致。
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         cfg.Timeout,
	}

	addr := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	conn, err := ssh.Dial("tcp", addr, clientCfg)
	if err != nil {
		return nil, fmt.Errorf("SSH 连接 %s 失败: %w", addr, err)
	}
	defer conn.Close()

	session, err := conn.NewSession()
	if err != nil {
		return nil, fmt.Errorf("SSH 创建会话失败: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	done := make(chan error, 1)
	go func() {
		done <- session.Run(cmd)
	}()

	select {
	case err := <-done:
		res := &sshExecResult{
			Stdout: strings.TrimSpace(stdout.String()),
			Stderr: strings.TrimSpace(stderr.String()),
		}
		if err != nil {
			if exitErr, ok := err.(*ssh.ExitError); ok {
				res.ExitCode = exitErr.ExitStatus()
				return res, nil // 命令已执行，退出码非 0 由调用方判断
			}
			return res, err
		}
		return res, nil

	case <-time.After(cfg.Timeout):
		_ = session.Signal(ssh.SIGKILL)
		_ = session.Close()
		return nil, fmt.Errorf("SSH 命令执行超时（%s）: %s", cfg.Timeout, cmd)
	}
}

// sshConfigFromHS 从主机配置构造 SSH 参数
func sshConfigFromHS(cfg *model.HSConfig) SSHExecConfig {
	if cfg == nil {
		return SSHExecConfig{}
	}
	port := cfg.ServerPort
	if port <= 0 {
		port = 22
	}
	return SSHExecConfig{
		Host:     cfg.ServerAddr,
		Port:     port,
		User:     cfg.ServerUser,
		Password: cfg.ServerPass,
		Timeout:  120 * time.Second,
	}
}

// isLocalHost 判断主机地址是否为本机（本机无需 SSH，直接本地执行）
func isLocalHost(addr string) bool {
	switch strings.TrimSpace(strings.ToLower(addr)) {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// shellQuote 对 shell 参数做单引号转义，避免路径中的特殊字符被解释
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
