// Package security 凭据加密模块
// 使用 AES-256-GCM 加密存储敏感凭据
package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CredentialCipher 凭据加密器
type CredentialCipher struct {
	key []byte
}

// 密钥文件路径（相对于数据目录）
const keyFileName = ".credkey"

// NewCredentialCipher 创建凭据加密器
// 如果密钥文件不存在则自动生成并持久化
func NewCredentialCipher(dataDir string) (*CredentialCipher, error) {
	keyPath := filepath.Join(dataDir, keyFileName)

	key, err := os.ReadFile(keyPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("读取密钥文件失败: %w", err)
		}
		// 生成新密钥
		key = make([]byte, 32) // AES-256
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, fmt.Errorf("生成密钥失败: %w", err)
		}
		// 持久化存储（权限 0600）
		if err := os.WriteFile(keyPath, key, 0600); err != nil {
			return nil, fmt.Errorf("保存密钥文件失败: %w", err)
		}
	}

	if len(key) != 32 {
		return nil, fmt.Errorf("密钥长度不正确: 期望32字节，实际%d字节", len(key))
	}

	return &CredentialCipher{key: key}, nil
}

// Encrypt 使用 AES-256-GCM 加密明文
// 返回 base64 编码的密文（包含 nonce）
func (c *CredentialCipher) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}

	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", fmt.Errorf("创建AES cipher失败: %w", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("创建GCM失败: %w", err)
	}

	nonce := make([]byte, aesGCM.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("生成nonce失败: %w", err)
	}

	// nonce + ciphertext
	ciphertext := aesGCM.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt 使用 AES-256-GCM 解密密文
func (c *CredentialCipher) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}

	ciphertext, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// 可能是未加密的旧数据，直接返回
		return encoded, nil
	}

	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", fmt.Errorf("创建AES cipher失败: %w", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("创建GCM失败: %w", err)
	}

	nonceSize := aesGCM.NonceSize()
	if len(ciphertext) < nonceSize {
		// 密文太短，可能是未加密的旧数据
		return encoded, nil
	}

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := aesGCM.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// 解密失败，可能是未加密的旧数据
		return encoded, nil
	}

	return string(plaintext), nil
}