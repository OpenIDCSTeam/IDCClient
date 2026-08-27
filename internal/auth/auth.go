// Package auth 认证与权限系统
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
)

// ============================================================
// IP登录限流
// ============================================================

type loginAttempt struct {
	count     int
	lockedAt  time.Time
}

var (
	loginAttempts = make(map[string]*loginAttempt)
	loginMu       sync.Mutex
)

const (
	maxLoginAttempts = 5
	lockDuration     = 300 * time.Second
)

// CheckLoginLimit 检查IP是否被限流，返回true表示被锁定
func CheckLoginLimit(ip string) bool {
	loginMu.Lock()
	defer loginMu.Unlock()

	attempt, ok := loginAttempts[ip]
	if !ok {
		return false
	}
	if attempt.count >= maxLoginAttempts {
		if time.Since(attempt.lockedAt) < lockDuration {
			return true
		}
		// 锁定已过期，重置
		delete(loginAttempts, ip)
	}
	return false
}

// RecordLoginFailure 记录登录失败
func RecordLoginFailure(ip string) {
	loginMu.Lock()
	defer loginMu.Unlock()

	attempt, ok := loginAttempts[ip]
	if !ok {
		attempt = &loginAttempt{}
		loginAttempts[ip] = attempt
	}
	attempt.count++
	if attempt.count >= maxLoginAttempts {
		attempt.lockedAt = time.Now()
	}
}

// ResetLoginAttempts 重置登录失败计数（登录成功后调用）
func ResetLoginAttempts(ip string) {
	loginMu.Lock()
	defer loginMu.Unlock()
	delete(loginAttempts, ip)
}

// ============================================================
// 密码哈希
// ============================================================

// HashPassword 使用bcrypt哈希密码
func HashPassword(password string) (string, error) {
	bytes, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("密码哈希失败: %w", err)
	}
	return string(bytes), nil
}

// CheckPassword 验证密码（支持bcrypt和旧版SHA256）
func CheckPassword(password, hash string) bool {
	// 尝试bcrypt验证
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)); err == nil {
		return true
	}
	// 兼容旧版SHA256哈希
	sha := sha256.Sum256([]byte(password))
	shaHex := hex.EncodeToString(sha[:])
	return shaHex == hash
}

// ============================================================
// Token管理
// ============================================================

// GenerateToken 生成256位随机Token（委托给utils包）
func GenerateToken() (string, error) {
	return utils.GenerateToken()
}

// ============================================================
// 认证上下文键
// ============================================================

const (
	ContextKeyUser    = "current_user"
	ContextKeyIsAdmin = "is_admin"
	ContextKeyIsToken = "is_token_auth"
	SessionKeyUser    = "username"
)

// UserContext 认证上下文中的用户信息
type UserContext struct {
	Username string
	IsAdmin  bool
	IsToken  bool // true=Bearer Token认证
	UserData map[string]interface{}
}

// ============================================================
// 认证中间件
// ============================================================

// UserLoader 用于从数据库加载用户的函数类型
type UserLoader func(username string) (map[string]interface{}, error)

// TokenGetter 用于获取系统Token的函数类型
type TokenGetter func() string

// AuthMiddleware 认证中间件（Bearer Token + Session双模式）
// 注入用户上下文，未认证时返回401
func AuthMiddleware(loadUser UserLoader, getToken TokenGetter) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. 尝试Bearer Token认证
		authHeader := c.GetHeader("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			sysToken := getToken()
			if token == sysToken && sysToken != "" {
				// Token认证成功，注入管理员上下文
				c.Set(ContextKeyUser, &UserContext{
					Username: "admin",
					IsAdmin:  true,
					IsToken:  true,
					UserData: map[string]interface{}{
						"username": "admin",
						"is_admin": true,
					},
				})
				c.Next()
				return
			}
		}

		// 2. 尝试Session认证
		session := sessions.Default(c)
		username, ok := session.Get(SessionKeyUser).(string)
		if ok && username != "" {
			user, err := loadUser(username)
			if err == nil && user != nil {
				isAdmin := model.SafeGetBool(user, "is_admin")
				c.Set(ContextKeyUser, &UserContext{
					Username: username,
					IsAdmin:  isAdmin,
					IsToken:  false,
					UserData: user,
				})
				c.Next()
				return
			}
		}

		// 3. 未认证
		utils.Unauthorized(c)
		c.Abort()
	}
}

// OptionalAuthMiddleware 可选认证中间件（不强制要求登录）
func OptionalAuthMiddleware(loadUser UserLoader, getToken TokenGetter) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token := strings.TrimPrefix(authHeader, "Bearer ")
			sysToken := getToken()
			if token == sysToken && sysToken != "" {
				c.Set(ContextKeyUser, &UserContext{
					Username: "admin",
					IsAdmin:  true,
					IsToken:  true,
					UserData: map[string]interface{}{"username": "admin", "is_admin": true},
				})
			}
		} else {
			session := sessions.Default(c)
			username, ok := session.Get(SessionKeyUser).(string)
			if ok && username != "" {
				if user, err := loadUser(username); err == nil && user != nil {
					isAdmin := model.SafeGetBool(user, "is_admin")
					c.Set(ContextKeyUser, &UserContext{
						Username: username,
						IsAdmin:  isAdmin,
						IsToken:  false,
						UserData: user,
					})
				}
			}
		}
		c.Next()
	}
}

// RequireAdmin 管理员权限中间件
func RequireAdmin() gin.HandlerFunc {
	return func(c *gin.Context) {
		uc := GetUserContext(c)
		if uc == nil || !uc.IsAdmin {
			utils.Forbidden(c)
			c.Abort()
			return
		}
		c.Next()
	}
}

// GetUserContext 从gin上下文获取用户信息
func GetUserContext(c *gin.Context) *UserContext {
	val, exists := c.Get(ContextKeyUser)
	if !exists {
		return nil
	}
	uc, _ := val.(*UserContext)
	return uc
}

// GetClientIP 获取客户端真实IP
func GetClientIP(c *gin.Context) string {
	// 优先从X-Forwarded-For获取
	if ip := c.GetHeader("X-Forwarded-For"); ip != "" {
		parts := strings.Split(ip, ",")
		return strings.TrimSpace(parts[0])
	}
	if ip := c.GetHeader("X-Real-IP"); ip != "" {
		return ip
	}
	return c.ClientIP()
}

// ============================================================
// Panic恢复中间件
// ============================================================

// RecoveryMiddleware panic恢复中间件
func RecoveryMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				c.JSON(http.StatusOK, utils.Response{
					Code: 500,
					Msg:  fmt.Sprintf("服务器内部错误: %v", err),
				})
				c.Abort()
			}
		}()
		c.Next()
	}
}
