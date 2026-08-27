// Package middleware CSRF保护中间件
package middleware

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

const (
	csrfTokenKey    = "csrf_token"
	csrfHeaderName  = "X-CSRF-Token"
	csrfFormName    = "csrf_token"
	csrfTokenLength = 32
)

// CSRFProtection CSRF保护中间件
func CSRFProtection() gin.HandlerFunc {
	return func(c *gin.Context) {
		session := sessions.Default(c)

		// GET/HEAD/OPTIONS请求：生成或验证token存在
		if c.Request.Method == "GET" || c.Request.Method == "HEAD" || c.Request.Method == "OPTIONS" {
			token := session.Get(csrfTokenKey)
			if token == nil {
				// 生成新token
				newToken, err := generateCSRFToken()
				if err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "生成CSRF Token失败"})
					c.Abort()
					return
				}
				session.Set(csrfTokenKey, newToken)
				_ = session.Save()
			}
			c.Next()
			return
		}

		// POST/PUT/DELETE/PATCH请求：验证token
		sessionToken := session.Get(csrfTokenKey)
		if sessionToken == nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "CSRF token缺失，请刷新页面"})
			c.Abort()
			return
		}

		// 从请求头或表单获取token
		requestToken := c.GetHeader(csrfHeaderName)
		if requestToken == "" {
			requestToken = c.PostForm(csrfFormName)
		}

		if requestToken == "" {
			c.JSON(http.StatusForbidden, gin.H{"error": "CSRF token未提供"})
			c.Abort()
			return
		}

		// 验证token
		if requestToken != sessionToken {
			c.JSON(http.StatusForbidden, gin.H{"error": "CSRF token无效"})
			c.Abort()
			return
		}

		c.Next()
	}
}

// generateCSRFToken 生成CSRF Token
func generateCSRFToken() (string, error) {
	b := make([]byte, csrfTokenLength)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

// GetCSRFToken 获取当前CSRF Token（用于前端获取）
func GetCSRFToken(c *gin.Context) string {
	session := sessions.Default(c)
	token := session.Get(csrfTokenKey)
	if token == nil {
		return ""
	}
	if str, ok := token.(string); ok {
		return str
	}
	return ""
}
