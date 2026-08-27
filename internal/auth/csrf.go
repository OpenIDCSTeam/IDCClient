// Package auth CSRF 防护中间件
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// CSRFTokenStore CSRF Token 存储
type CSRFTokenStore struct {
	tokens map[string]time.Time // token -> 过期时间
	mu     sync.RWMutex
}

// NewCSRFTokenStore 创建 CSRF Token 存储
func NewCSRFTokenStore() *CSRFTokenStore {
	store := &CSRFTokenStore{
		tokens: make(map[string]time.Time),
	}
	// 启动清理协程
	go store.cleanupLoop()
	return store
}

// Generate 生成新的 CSRF Token
func (s *CSRFTokenStore) Generate() string {
	token := generateRandomToken(32)
	s.mu.Lock()
	s.tokens[token] = time.Now().Add(24 * time.Hour) // 24小时有效期
	s.mu.Unlock()
	return token
}

// Validate 验证 CSRF Token
func (s *CSRFTokenStore) Validate(token string) bool {
	s.mu.RLock()
	expiry, exists := s.tokens[token]
	s.mu.RUnlock()

	if !exists {
		return false
	}

	// 检查是否过期
	if time.Now().After(expiry) {
		s.mu.Lock()
		delete(s.tokens, token)
		s.mu.Unlock()
		return false
	}

	return true
}

// Delete 删除 CSRF Token
func (s *CSRFTokenStore) Delete(token string) {
	s.mu.Lock()
	delete(s.tokens, token)
	s.mu.Unlock()
}

// cleanupLoop 定期清理过期的 Token
func (s *CSRFTokenStore) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Hour)
	defer ticker.Stop()

	for range ticker.C {
		s.cleanup()
	}
}

// cleanup 清理过期的 Token
func (s *CSRFTokenStore) cleanup() {
	now := time.Now()
	s.mu.Lock()
	for token, expiry := range s.tokens {
		if now.After(expiry) {
			delete(s.tokens, token)
		}
	}
	s.mu.Unlock()
}

// 全局 CSRF Token 存储
var csrfStore = NewCSRFTokenStore()

// CSRFMiddleware CSRF 防护中间件
func CSRFMiddleware(exemptPaths ...string) gin.HandlerFunc {
	// 豁免路径集合
	exemptSet := make(map[string]bool)
	for _, path := range exemptPaths {
		exemptSet[path] = true
	}

	return func(c *gin.Context) {
		// OPTIONS 请求不检查 CSRF
		if c.Request.Method == "OPTIONS" {
			c.Next()
			return
		}

		// GET、HEAD、TRACE 请求不检查 CSRF（安全方法）
		if c.Request.Method == "GET" || c.Request.Method == "HEAD" || c.Request.Method == "TRACE" {
			c.Next()
			return
		}

		// 检查是否在豁免路径中
		if exemptSet[c.Request.URL.Path] {
			c.Next()
			return
		}

		// 从请求头或表单中获取 CSRF Token
		token := c.GetHeader("X-CSRF-Token")
		if token == "" {
			token = c.PostForm("csrf_token")
		}

		// 验证 Token
		if token == "" || !csrfStore.Validate(token) {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "CSRF token 无效或已过期",
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// CSRFTokenHandler 生成 CSRF Token 的处理器
func CSRFTokenHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := csrfStore.Generate()
		c.JSON(http.StatusOK, gin.H{
			"csrf_token": token,
		})
	}
}

// generateRandomToken 生成随机 Token
func generateRandomToken(length int) string {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		panic("生成随机 Token 失败: " + err.Error())
	}
	return base64.URLEncoding.EncodeToString(bytes)
}

// GetCSRFToken 从上下文中获取或生成 CSRF Token
func GetCSRFToken(c *gin.Context) string {
	// 尝试从 cookie 中获取已有的 token
	if token, err := c.Cookie("csrf_token"); err == nil && token != "" {
		if csrfStore.Validate(token) {
			return token
		}
	}

	// 生成新的 token
	token := csrfStore.Generate()
	
	// 设置到 cookie
	c.SetCookie(
		"csrf_token",
		token,
		86400,           // 24小时
		"/",
		"",
		false,           // 允许 HTTP（开发环境）
		true,            // HttpOnly
	)

	return token
}
