package api

import (
	"strconv"

	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/host"
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/models"
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/vm"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// RestManager REST API管理器
type RestManager struct {
	hostManager *host.HostManager
	vmManager   *vm.VMManager
	logger      *logrus.Logger
}

// NewRestManager 创建REST管理器
func NewRestManager(hostManager *host.HostManager, vmManager *vm.VMManager, logger *logrus.Logger) *RestManager {
	return &RestManager{
		hostManager: hostManager,
		vmManager:   vmManager,
		logger:      logger,
	}
}

// apiResponse 统一API响应格式
func (r *RestManager) apiResponse(c *gin.Context, code int, msg string, data interface{}) {
	c.JSON(code, models.NewAPIResponse(code, msg, data))
}

// apiSuccess 成功响应
func (r *RestManager) apiSuccess(c *gin.Context, data interface{}) {
	r.apiResponse(c, 200, "success", data)
}

// apiError 错误响应
func (r *RestManager) apiError(c *gin.Context, code int, msg string) {
	r.apiResponse(c, code, msg, nil)
}

// getQueryParam 获取查询参数
func (r *RestManager) getQueryParam(c *gin.Context, key, defaultValue string) string {
	value := c.Query(key)
	if value == "" {
		return defaultValue
	}
	return value
}

// getQueryInt 获取整数查询参数
func (r *RestManager) getQueryInt(c *gin.Context, key string, defaultValue int) int {
	value := c.Query(key)
	if value == "" {
		return defaultValue
	}
	if intValue, err := strconv.Atoi(value); err == nil {
		return intValue
	}
	return defaultValue
}

// getQueryBool 获取布尔查询参数
func (r *RestManager) getQueryBool(c *gin.Context, key string, defaultValue bool) bool {
	value := c.Query(key)
	if value == "" {
		return defaultValue
	}
	return value == "true" || value == "1"
}