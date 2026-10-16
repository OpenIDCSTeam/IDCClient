// Package utils 提供通用工具函数
package utils

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Response 统一响应结构。
//
// 与 Python 版 RestManager.api_response 保持一致：
// {code, msg, data, timestamp}，HTTP 状态码恒为 200，业务语义由 code 表达。
// 前端（React）基于 Python 版协议开发，因此这里必须保持同样的契约，
// 否则 axios 会因非 2xx 直接进入异常分支而拿不到 code。
type Response struct {
	Code      int         `json:"code"`
	Msg       string      `json:"msg"`
	Data      interface{} `json:"data,omitempty"`
	Timestamp int64       `json:"timestamp"`
}

// now 返回秒级时间戳，与 Python 版 int(time.time()) 对齐
func now() int64 {
	return time.Now().Unix()
}

// Success 返回成功响应
func Success(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, Response{
		Code:      200,
		Msg:       "成功",
		Data:      data,
		Timestamp: now(),
	})
}

// SuccessMsg 返回带消息的成功响应
func SuccessMsg(c *gin.Context, msg string, data interface{}) {
	c.JSON(http.StatusOK, Response{
		Code:      200,
		Msg:       msg,
		Data:      data,
		Timestamp: now(),
	})
}

// Fail 返回失败响应。
//
// 保持 HTTP 200 + 业务码的形态以兼容既有前端；
// 失败可观测性通过日志与监控指标补充，而不是改变响应协议。
func Fail(c *gin.Context, code int, msg string) {
	c.JSON(http.StatusOK, Response{
		Code:      code,
		Msg:       msg,
		Timestamp: now(),
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
