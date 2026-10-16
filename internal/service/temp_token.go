// Package service 临时凭据管理服务
package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// tempTokenData 临时凭据数据
type tempTokenData struct {
	HsName      string    // 主机名
	VmUUID      string    // 虚拟机UUID
	VirtualUser string    // 虚拟用户名
	Expire      time.Time // 过期时间
}

// TempTokenStore 临时凭据存储（线程安全）
type TempTokenStore struct {
	mu     sync.RWMutex
	tokens map[string]tempTokenData // token -> data
}

// NewTempTokenStore 创建临时凭据存储
func NewTempTokenStore() *TempTokenStore {
	store := &TempTokenStore{
		tokens: make(map[string]tempTokenData),
	}
	// 启动后台清理协程
	go store.cleanupLoop()
	return store
}

// TempTokenTTL 临时凭据有效期
const TempTokenTTL = 5 * time.Minute

// Generate 生成临时凭据（5分钟有效期）
func (s *TempTokenStore) Generate(hsName, vmUUID, virtualUser string) string {
	token, _ := s.GenerateWithExpire(hsName, vmUUID, virtualUser)
	return token
}

// GenerateWithExpire 生成临时凭据并返回过期时间。
// 外部插件（FSPlugins 等）需要 expire 字段用于自行判断凭据是否仍然有效，
// 因此把过期时间显式返回给调用方，避免各处理器各自硬编码 300 秒。
func (s *TempTokenStore) GenerateWithExpire(hsName, vmUUID, virtualUser string) (string, time.Time) {
	token := s.generateToken(hsName, vmUUID, time.Now().Unix())
	expire := time.Now().Add(TempTokenTTL)

	s.mu.Lock()
	s.tokens[token] = tempTokenData{
		HsName:      hsName,
		VmUUID:      vmUUID,
		VirtualUser: virtualUser,
		Expire:      expire,
	}
	s.mu.Unlock()

	return token, expire
}

// Consume 消费临时凭据（一次性，消费后删除）
func (s *TempTokenStore) Consume(token string) (hsName, vmUUID, virtualUser string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	data, exists := s.tokens[token]
	if !exists {
		return "", "", "", false
	}
	
	// 检查是否过期
	if time.Now().After(data.Expire) {
		delete(s.tokens, token)
		return "", "", "", false
	}
	
	// 消费后立即删除（一次性使用）
	delete(s.tokens, token)
	
	return data.HsName, data.VmUUID, data.VirtualUser, true
}

// PurgeExpired 清理过期凭据
func (s *TempTokenStore) PurgeExpired() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	now := time.Now()
	count := 0
	for token, data := range s.tokens {
		if now.After(data.Expire) {
			delete(s.tokens, token)
			count++
		}
	}
	return count
}

// cleanupLoop 定期清理过期凭据（每分钟一次）
func (s *TempTokenStore) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	
	for range ticker.C {
		s.PurgeExpired()
	}
}

// generateToken 生成临时令牌（SHA256）
func (s *TempTokenStore) generateToken(hsName, vmUUID string, timestamp int64) string {
	raw := fmt.Sprintf("%s:%s:%d:%d", hsName, vmUUID, timestamp, time.Now().UnixNano())
	hash := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(hash[:])
}
