// Package utils 提供通用工具函数
package utils

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// Response 统一响应结构
type Response struct {
	Code int         `json:"code"`
	Msg  string      `json:"msg"`
	Data interface{} `json:"data,omitempty"`
}

// Success 返回成功响应
func Success(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, Response{
		Code: 200,
		Msg:  "成功",
		Data: data,
	})
}

// SuccessMsg 返回带消息的成功响应
func SuccessMsg(c *gin.Context, msg string, data interface{}) {
	c.JSON(http.StatusOK, Response{
		Code: 200,
		Msg:  msg,
		Data: data,
	})
}

// Fail 返回失败响应
func Fail(c *gin.Context, code int, msg string) {
	c.JSON(http.StatusOK, Response{
		Code: code,
		Msg:  msg,
	})
}

// BadRequest 400 参数错误
func BadRequest(c *gin.Context, msg string) {
	Fail(c, 400, msg)
}

// Unauthorized 401 未授权
func Unauthorized(c *gin.Context) {
	Fail(c, 401, "未授权，请先登录")
}

// Forbidden 403 权限不足
func Forbidden(c *gin.Context) {
	Fail(c, 403, "权限不足")
}

// NotFound 404 资源不存在
func NotFound(c *gin.Context, msg string) {
	Fail(c, 404, msg)
}

// TooManyRequests 429 限流
func TooManyRequests(c *gin.Context, msg string) {
	Fail(c, 429, msg)
}

// InternalError 500 服务器错误
func InternalError(c *gin.Context, msg string) {
	Fail(c, 500, msg)
}
