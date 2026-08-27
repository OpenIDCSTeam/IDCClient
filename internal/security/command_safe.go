// Package security 命令执行安全模块
// 提供统一的命令执行入口，防止命令注入
package security

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// 命令注入检测的危险模式
var dangerousPatterns = []*regexp.Regexp{
	regexp.MustCompile(`[;&|` + "`" + `$]`),
	regexp.MustCompile(`\$\(`),
	regexp.MustCompile(`>\s*/`),
	regexp.MustCompile(`\.\./\.\.`),
}

// 允许的安全命令前缀白名单
var safeCmdPrefixes = []string{
	"docker ", "firecracker ", "cloud-hypervisor ", "qemu-",
	"socat ", "nohup socat ", "kill ", "ps ", "test ", "echo ",
	"mkdir ", "rm ", "cp ", "mv ", "cat ", "ls ", "chmod ", "chown ",
	"ip ", "iptables ", "nft ", "mount ", "umount ", "sync",
	"tar ", "truncate ", "mkfs.", "sed ", "head ", "nproc",
	"qm ", "pvesh ", "virsh ", "VBoxManage", "vmrest",
	"openssl ", "ssh-keygen", "gunzip ", "gzip ",
	"command -v ", "which ", "dpkg ", "apt ", "yum ",
	"systemctl ", "service ",
	"df ", "free ", "top ", "uptime", "uname",
	"lsblk", "blkid", "fdisk", "parted",
	"brctl ", "bridge ", "tc ",
	"curl ", "wget ",
}

// IsSafeCmd 检查命令字符串是否在白名单中
func IsSafeCmd(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	for _, prefix := range safeCmdPrefixes {
		if strings.HasPrefix(cmd, prefix) {
			return true
		}
	}
	return false
}

// ValidateCmd 验证 shell 命令是否安全
func ValidateCmd(cmd string, allowPipe bool) error {
	if strings.TrimSpace(cmd) == "" {
		return fmt.Errorf("命令为空")
	}

	// 白名单命令放行
	if IsSafeCmd(cmd) {
		// 仍检查反引号命令替换
		if strings.Contains(cmd, "`") {
			return fmt.Errorf("命令包含反引号命令替换: %s", truncate(cmd, 80))
		}
		if strings.Contains(cmd, "$(") {
			return fmt.Errorf("命令包含$()命令替换: %s", truncate(cmd, 80))
		}
		return nil
	}

	// 非白名单命令严格检查
	for _, pattern := range dangerousPatterns {
		if pattern.MatchString(cmd) {
			return fmt.Errorf("命令包含危险字符: %s", truncate(cmd, 80))
		}
	}

	if !allowPipe && strings.Contains(cmd, "|") {
		return fmt.Errorf("命令包含管道符（未启用allowPipe）: %s", truncate(cmd, 80))
	}

	return nil
}

// SafeExec 安全执行本地命令（列表形式，无 shell 注入风险）
func SafeExec(ctx context.Context, args []string) (string, string, error) {
	if len(args) == 0 {
		return "", "", fmt.Errorf("命令参数为空")
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	output, err := cmd.CombinedOutput()
	stdout := string(output)

	if err != nil {
		return stdout, "", fmt.Errorf("命令执行失败 [%s]: %w", strings.Join(args, " "), err)
	}
	return stdout, "", nil
}

// SafeExecWithTimeout 带超时的安全命令执行
func SafeExecWithTimeout(args []string, timeout time.Duration) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return SafeExec(ctx, args)
}

// SafeShellExec 安全执行 shell 命令（带注入检查，仅限白名单命令）
func SafeShellExec(cmd string, timeout time.Duration, allowPipe bool) (string, string, error) {
	if err := ValidateCmd(cmd, allowPipe); err != nil {
		return "", "", fmt.Errorf("安全检查未通过: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	output, err := c.CombinedOutput()
	stdout := string(output)

	if err != nil {
		return stdout, "", fmt.Errorf("命令执行失败: %w", err)
	}
	return stdout, "", nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}