package middleware

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
)

// AuthMiddleware 认证中间件
type AuthMiddleware struct {
	hostManager interface {
		ValidateToken(token string) bool
		GetBearer() string
	}
	logger *logrus.Logger
}

// NewAuthMiddleware 创建认证中间件
func NewAuthMiddleware(hostManager interface{}, logger *logrus.Logger) *AuthMiddleware {
	return &AuthMiddleware{
		hostManager: hostManager,
		logger:      logger,
	}
}

// RequireAuth 需要认证的中间件
func (a *AuthMiddleware) RequireAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 检查是否需要跳过认证的路径
		if a.shouldSkipAuth(c.Request.URL.Path) {
			c.Next()
			return
		}

		// 获取Token
		token := a.extractToken(c)
		if token == "" {
			a.unauthorizedResponse(c, "缺少认证Token")
			return
		}

		// 验证Token
		if !a.hostManager.ValidateToken(token) {
			a.unauthorizedResponse(c, "无效的Token")
			return
		}

		// Token有效，继续处理
		c.Set("token", token)
		c.Next()
	}
}

// OptionalAuth 可选认证的中间件
func (a *AuthMiddleware) OptionalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := a.extractToken(c)
		if token != "" && a.hostManager.ValidateToken(token) {
			c.Set("token", token)
			c.Set("authenticated", true)
		} else {
			c.Set("authenticated", false)
		}
		c.Next()
	}
}

// extractToken 从请求中提取Token
func (a *AuthMiddleware) extractToken(c *gin.Context) string {
	// 从Authorization头获取
	authHeader := c.GetHeader("Authorization")
	if authHeader != "" {
		parts := strings.SplitN(authHeader, " ", 2)
		if len(parts) == 2 && strings.ToLower(parts[0]) == "bearer" {
			return parts[1]
		}
	}

	// 从查询参数获取
	if token := c.Query("token"); token != "" {
		return token
	}

	// 从Cookie获取
	if cookie, err := c.Cookie("token"); err == nil && cookie != "" {
		return cookie
	}

	return ""
}

// shouldSkipAuth 检查是否应该跳过认证
func (a *AuthMiddleware) shouldSkipAuth(path string) bool {
	// 健康检查
	if path == "/health" || path == "/" {
		return true
	}

	// 登录相关
	if strings.HasPrefix(path, "/api/v1/login") {
		return true
	}

	// 静态文件
	if strings.HasPrefix(path, "/static/") {
		return true
	}

	// 系统信息（可选）
	if path == "/api/v1/system/info" {
		return true
	}

	return false
}

// unauthorizedResponse 返回未授权响应
func (a *AuthMiddleware) unauthorizedResponse(c *gin.Context, message string) {
	c.JSON(http.StatusUnauthorized, gin.H{
		"code":    401,
		"message": message,
		"data":    nil,
	})
	c.Abort()
}

// LoggingMiddleware 日志中间件
func LoggingMiddleware(logger *logrus.Logger) gin.HandlerFunc {
	return gin.LoggerWithFormatter(func(param gin.LogFormatterParams) string {
		logger.WithFields(logrus.Fields{
			"status_code": param.StatusCode,
			"latency":     param.Latency,
			"client_ip":   param.ClientIP,
			"method":      param.Method,
			"path":        param.Path,
			"error":       param.ErrorMessage,
		}).Info("HTTP Request")
		return ""
	})
}

// RecoveryMiddleware 恢复中间件
func RecoveryMiddleware(logger *logrus.Logger) gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		logger.WithFields(logrus.Fields{
			"error":      recovered,
			"stack":      gin.Stack(),
			"client_ip":  c.ClientIP(),
			"method":     c.Request.Method,
			"path":       c.Request.URL.Path,
			"user_agent": c.Request.UserAgent(),
		}).Error("Panic recovered")

		c.JSON(http.StatusInternalServerError, gin.H{
			"code":    500,
			"message": "服务器内部错误",
			"data":    nil,
		})
	})
}

// CORSMiddleware CORS中间件
func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Authorization, X-Requested-With")
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Max-Age", "86400")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// RateLimitMiddleware 限流中间件
type RateLimitMiddleware struct {
	visitors map[string]*visitor
	mu       sync.RWMutex
	rate     rate.Limit
	burst    int
}

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func NewRateLimitMiddleware(r int, b int) *RateLimitMiddleware {
	return &RateLimitMiddleware{
		visitors: make(map[string]*visitor),
		rate:     rate.Limit(r),
		burst:    b,
	}
}

func (r *RateLimitMiddleware) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		r.mu.Lock()
		
		v, exists := r.visitors[ip]
		if !exists {
			limiter := rate.NewLimiter(r.rate, r.burst)
			r.visitors[ip] = &visitor{limiter, time.Now()}
			v = r.visitors[ip]
		}
		
		v.lastSeen = time.Now()
		r.mu.Unlock()
		
		if !v.limiter.Allow() {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"code":    429,
				"message": "请求过于频繁，请稍后再试",
				"data":    nil,
			})
			c.Abort()
			return
		}
		
		c.Next()
	}
}

// CleanupVisitors 清理过期的访客记录
func (r *RateLimitMiddleware) CleanupVisitors() {
	r.mu.Lock()
	defer r.mu.Unlock()
	
	for ip, v := range r.visitors {
		if time.Since(v.lastSeen) > 3*time.Minute {
			delete(r.visitors, ip)
		}
	}
}

// SecurityHeadersMiddleware 安全头中间件
func SecurityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Content-Security-Policy", "default-src 'self'")
		
		c.Next()
	}
}

// RequestIDMiddleware 请求ID中间件
func RequestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			requestID = generateRequestID()
		}
		
		c.Set("request_id", requestID)
		c.Header("X-Request-ID", requestID)
		c.Next()
	}
}

func generateRequestID() string {
	return uuid.New().String()
}