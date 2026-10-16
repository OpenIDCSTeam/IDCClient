// Package driver 脚本参数转义与标识校验
//
// 多个驱动通过外部解释器完成操作（PowerShell -Command、sh -c、ssh 命令串）。
// 使用 exec.CommandContext 的参数数组只保证「参数边界」安全，
// 无法阻止解释器对字符串内部再做一次解析。
// 因此凡是拼入脚本文本的动态值都必须先转义，本文件集中提供该能力。
package driver

import (
	"fmt"
	"strings"
)

// psEscape 转义 PowerShell 单引号字面量内容。
//
// 单引号字符串内只有 ' 需要转义（双写为 ''）；
// 同时移除 CR/LF/NUL，避免脚本被拆分成多条语句。
func psEscape(v string) string {
	v = strings.ReplaceAll(v, "'", "''")
	v = strings.ReplaceAll(v, "\r", "")
	v = strings.ReplaceAll(v, "\n", "")
	v = strings.ReplaceAll(v, "\x00", "")
	return v
}

// shEscape 转义 POSIX shell 单引号字面量内容。
//
// 单引号字符串内无法直接表示单引号，需要先闭合再转义：
// ' -> '\''
func shEscape(v string) string {
	v = strings.ReplaceAll(v, "\r", "")
	v = strings.ReplaceAll(v, "\n", "")
	v = strings.ReplaceAll(v, "\x00", "")
	return strings.ReplaceAll(v, "'", `'\''`)
}

// safeIdent 校验标识可用于路径拼接与命令行参数。
//
// 虚拟机和设备的标识会参与宿主机目录拼接、脚本文本以及切片操作，
// 必须拒绝路径分隔符、上级目录、引号与空白。
func safeIdent(v string, maxLen int) error {
	if v == "" {
		return fmt.Errorf("标识不能为空")
	}
	if maxLen > 0 && len(v) > maxLen {
		return fmt.Errorf("标识长度超过上限 %d", maxLen)
	}
	if strings.TrimSpace(v) != v {
		return fmt.Errorf("标识首尾不能包含空白字符")
	}
	if strings.ContainsAny(v, `/\:*?"<>|'`+"`") {
		return fmt.Errorf("标识包含非法字符: %s", v)
	}
	if strings.Contains(v, "..") {
		return fmt.Errorf("标识不能包含上级目录引用")
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("标识包含控制字符")
		}
	}
	return nil
}

// sanitizeFileName 仅保留文件名部分并拒绝空结果，用于备份名等场景。
func sanitizeFileName(name string) (string, error) {
	base := strings.TrimSpace(name)
	if base == "" {
		return "", fmt.Errorf("文件名不能为空")
	}
	// 统一分隔符后再取最后一段，避免平台差异导致的绕过
	base = strings.ReplaceAll(base, "\\", "/")
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}
	if base == "" || base == "." || base == ".." {
		return "", fmt.Errorf("文件名无效")
	}
	if strings.ContainsAny(base, "\x00") {
		return "", fmt.Errorf("文件名包含非法字符")
	}
	return base, nil
}
