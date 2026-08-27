// Package utils 通用工具函数 - Token生成
package utils

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// GenerateToken 生成256位随机Token
func GenerateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成Token失败: %w", err)
	}
	return hex.EncodeToString(b), nil
}
