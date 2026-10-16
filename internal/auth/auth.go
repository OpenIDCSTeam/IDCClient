// Package auth 认证与权限系统
package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
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
	// SessionKeyPasswordTag 会话口令标记：口令变更后旧会话立即失效
	SessionKeyPasswordTag = "password_tag"
	// 临时凭据会话键：只允许访问指定主机的指定虚拟机
	SessionKeyTempLogin = "temp_login"
	SessionKeyTempHost  = "temp_hs_name"
	SessionKeyTempVM    = "temp_vm_uuid"
	// SessionKeyTokenLogin 系统 Token 登录标记。
	// Token 登录不存在对应用户口令，无法用口令标记校验会话有效性，
	// 必须单独识别，否则该会话会在下一次请求被判为失效。
	SessionKeyTokenLogin = "token_login"
)

// PasswordTag 计算口令哈希的短标记，用于判断会话是否仍然有效。
//
// 之所以不直接使用用户表的 updated_at：配额重算等后台任务会周期性
// 更新用户行，若以 updated_at 作为会话版本，会把所有在线用户强制登出。
// 口令哈希只在改密时变化，语义与「凭据是否仍然有效」一致。
func PasswordTag(passwordHash string) string {
	sum := sha256.Sum256([]byte(passwordHash))
	return hex.EncodeToString(sum[:16])
}

// UserContext 认证上下文中的用户信息
type UserContext struct {
	Username string
	IsAdmin  bool
	IsToken  bool // true=Bearer Token认证
	UserData map[string]interface{}
	// TempLogin 临时凭据登录：不对应数据库用户，
	// 权限范围被限制为 TempHsName 上的 TempVMUUID。
	TempLogin  bool
	TempHsName string
	TempVMUUID string
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
			// 系统 Token 登录的会话不对应真实用户口令，
			// 直接按管理员身份放行（与 Bearer 认证等价）。
			if tokenLogin, _ := session.Get(SessionKeyTokenLogin).(bool); tokenLogin {
				c.Set(ContextKeyUser, &UserContext{
					Username: "admin",
					IsAdmin:  true,
					IsToken:  true,
					UserData: map[string]interface{}{
						"username": "admin",
						"is_admin": true,
						"is_active": true,
					},
				})
				c.Next()
				return
			}

			// 临时凭据会话不对应数据库用户，直接按会话中记录的范围放行；
			// 具体可访问的资源由 HostVMAccessMiddleware 再次收紧。
			if tempLogin, _ := session.Get(SessionKeyTempLogin).(bool); tempLogin {
				hsName, _ := session.Get(SessionKeyTempHost).(string)
				vmUUID, _ := session.Get(SessionKeyTempVM).(string)
				if hsName != "" && vmUUID != "" {
					c.Set(ContextKeyUser, &UserContext{
						Username:   username,
						IsAdmin:    false,
						IsToken:    false,
						TempLogin:  true,
						TempHsName: hsName,
						TempVMUUID: vmUUID,
						UserData: map[string]interface{}{
							"username": username,
							"is_admin": false,
							"is_active": true,
							// 与 Python 版一致：受限身份的主机范围就是签发时绑定的主机，
							// 否则处理器内部的 CheckHostAccess 会因「用户不存在」而误拒。
							"assigned_hosts": []string{hsName},
							"temp_login":     true,
							"temp_hs_name":   hsName,
							"temp_vm_uuid":   vmUUID,
						},
					})
					c.Next()
					return
				}
			}

			user, err := loadUser(username)
			if err == nil && user != nil {
				// 账号被禁用后必须立即失效：仅在登录时校验会让已建立的
				// 会话继续持有权限，禁用操作无法真正生效。
				if !model.SafeGetBool(user, "is_active") {
					utils.Fail(c, 401, "账号已被禁用")
					c.Abort()
					return
				}
				// 口令变更后旧会话失效，避免改密无法撤销既有登录
				savedTag, _ := session.Get(SessionKeyPasswordTag).(string)
				if savedTag == "" || savedTag != PasswordTag(model.SafeGetString(user, "password")) {
					utils.Fail(c, 401, "登录状态已失效，请重新登录")
					c.Abort()
					return
				}
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

// RecoveryMiddleware panic恢复中间件。
//
// 记录完整堆栈并返回真实 HTTP 500：
// 原实现只返回 HTTP 200 + 内部错误文本，既不写日志也不便于监控发现，
// 同时把内部细节暴露给调用方。
func RecoveryMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				stack := debug.Stack()
				log.Printf("[panic] %s %s 发生未捕获异常: %v\n%s",
					c.Request.Method, c.Request.URL.Path, err, stack)
				c.AbortWithStatusJSON(http.StatusInternalServerError, utils.Response{
					Code: 500,
					Msg:  "服务器内部错误",
				})
			}
		}()
		c.Next()
	}
}
