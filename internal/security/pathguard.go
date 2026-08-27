// Package security 路径安全模块
// 防止路径穿越攻击
package security

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SafeJoin 安全拼接路径，确保结果在基础目录内
func SafeJoin(basePath string, paths ...string) (string, error) {
	// 清理基础路径
	basePath, err := filepath.Abs(filepath.Clean(basePath))
	if err != nil {
		return "", fmt.Errorf("解析基础路径失败: %w", err)
	}

	// 拼接并清理
	result := filepath.Join(append([]string{basePath}, paths...)...)
	result = filepath.Clean(result)

	// 确保结果在基础目录内
	if !strings.HasPrefix(result, basePath+string(os.PathSeparator)) && result != basePath {
		return "", fmt.Errorf("路径穿越攻击: %s 超出了 %s", result, basePath)
	}

	return result, nil
}

// ValidatePath 验证给定路径是否在允许的根目录内
func ValidatePath(targetPath, rootPath string) error {
	absTarget, err := filepath.Abs(filepath.Clean(targetPath))
	if err != nil {
		return fmt.Errorf("解析目标路径失败: %w", err)
	}

	absRoot, err := filepath.Abs(filepath.Clean(rootPath))
	if err != nil {
		return fmt.Errorf("解析根路径失败: %w", err)
	}

	if !strings.HasPrefix(absTarget, absRoot+string(os.PathSeparator)) && absTarget != absRoot {
		return fmt.Errorf("路径 %s 不在允许的目录 %s 内", absTarget, absRoot)
	}

	return nil
}

// SanitizeFileName 清理文件名中的危险字符
func SanitizeFileName(name string) string {
	// 移除路径分隔符和空字节
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "\x00", "")
	// 移除开头的点（防止隐藏文件）
	name = strings.TrimLeft(name, ".")
	if name == "" {
		name = "unnamed"
	}
	return name
}