package api

import (
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/models"
	"github.com/gin-gonic/gin"
)

// RegisterHostAPI 注册主机管理API
func (r *RestManager) RegisterHostAPI(router *gin.RouterGroup) {
	hosts := router.Group("/hosts")
	{
		hosts.GET("/", r.getHosts)
		hosts.GET("/detail", r.getHostDetails)
		hosts.GET("/names", r.getHostNames)
		hosts.GET("/:hs_name", r.getHost)
		hosts.POST("/", r.createHost)
		hosts.PUT("/:hs_name", r.updateHost)
		hosts.DELETE("/:hs_name", r.deleteHost)
		
		// 电源管理
		hosts.POST("/:hs_name/power", r.powerHost)
		
		// 状态管理
		hosts.GET("/:hs_name/status", r.getHostStatus)
		hosts.GET("/:hs_name/status/list", r.getHostStatusList)
	}
}

// getHosts 获取所有主机
func (r *RestManager) getHosts(c *gin.Context) {
	hosts := r.hostManager.GetAllHosts()
	result := make([]gin.H, 0)
	
	for hsName, server := range hosts {
		config := server.GetConfig()
		vms := server.GetAllVMs()
		
		hostInfo := gin.H{
			"name":     hsName,
			"type":     config.ServerType,
			"addr":     config.ServerAddr,
			"vm_count": len(vms),
			"status":   "running",
		}
		result = append(result, hostInfo)
	}
	
	r.apiSuccess(c, result)
}

// getHostDetails 获取主机详细信息
func (r *RestManager) getHostDetails(c *gin.Context) {
	hsName := r.getQueryParam(c, "hs_name", "")
	if hsName == "" {
		r.apiError(c, 400, "主机名称不能为空")
		return
	}
	
	r.getHost(c)
}

// getHostNames 获取主机名称列表
func (r *RestManager) getHostNames(c *gin.Context) {
	names := r.hostManager.GetHostNames()
	r.apiSuccess(c, names)
}

// getHost 获取单个主机
func (r *RestManager) getHost(c *gin.Context) {
	hsName := c.Param("hs_name")
	if hsName == "" {
		hsName = r.getQueryParam(c, "hs_name", "")
		if hsName == "" {
			r.apiError(c, 400, "主机名称不能为空")
			return
		}
	}
	
	server := r.hostManager.GetHost(hsName)
	if server == nil {
		r.apiError(c, 404, "主机不存在")
		return
	}
	
	config := server.GetConfig()
	vms := server.GetAllVMs()
	vmList := make([]string, 0)
	
	for vmUUID := range vms {
		vmList = append(vmList, vmUUID)
	}
	
	hostInfo := gin.H{
		"name":        hsName,
		"type":        config.ServerType,
		"addr":        config.ServerAddr,
		"vm_count":    len(vms),
		"config":      config,
		"vm_list":     vmList,
		"last_updated": 0,
	}
	
	r.apiSuccess(c, hostInfo)
}

// createHost 创建主机
func (r *RestManager) createHost(c *gin.Context) {
	var req struct {
		HSName   string        `json:"hs_name" binding:"required"`
		HSType   string        `json:"hs_type" binding:"required"`
		HSConfig models.HSConfig `json:"hs_config" binding:"required"`
	}
	
	if err := c.ShouldBindJSON(&req); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	result := r.hostManager.AddHost(req.HSName, req.HSType, &req.HSConfig)
	if result.Success {
		r.apiSuccess(c, result.Results)
	} else {
		r.apiError(c, 500, result.Message)
	}
}

// updateHost 更新主机
func (r *RestManager) updateHost(c *gin.Context) {
	hsName := c.Param("hs_name")
	if hsName == "" {
		r.apiError(c, 400, "主机名称不能为空")
		return
	}
	
	var config models.HSConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	result := r.hostManager.UpdateHost(hsName, &config)
	if result.Success {
		r.apiSuccess(c, result.Results)
	} else {
		r.apiError(c, 500, result.Message)
	}
}

// deleteHost 删除主机
func (r *RestManager) deleteHost(c *gin.Context) {
	hsName := c.Param("hs_name")
	if hsName == "" {
		r.apiError(c, 400, "主机名称不能为空")
		return
	}
	
	if r.hostManager.DeleteHost(hsName) {
		r.apiSuccess(c, nil)
	} else {
		r.apiError(c, 500, "删除失败")
	}
}

// powerHost 主机电源控制
func (r *RestManager) powerHost(c *gin.Context) {
	hsName := c.Param("hs_name")
	if hsName == "" {
		r.apiError(c, 400, "主机名称不能为空")
		return
	}
	
	enable := r.getQueryBool(c, "enable", true)
	result := r.hostManager.PowerHost(hsName, enable)
	
	if result.Success {
		r.apiSuccess(c, result.Results)
	} else {
		r.apiError(c, 500, result.Message)
	}
}

// getHostStatus 获取主机状态
func (r *RestManager) getHostStatus(c *gin.Context) {
	hsName := c.Param("hs_name")
	if hsName == "" {
		r.apiError(c, 400, "主机名称不能为空")
		return
	}
	
	server := r.hostManager.GetHost(hsName)
	if server == nil {
		r.apiError(c, 404, "主机不存在")
		return
	}
	
	status := server.GetHostStatus()
	if status == nil {
		r.apiError(c, 404, "无状态数据")
		return
	}
	
	r.apiSuccess(c, status)
}

// getHostStatusList 获取主机状态列表
func (r *RestManager) getHostStatusList(c *gin.Context) {
	hsName := c.Param("hs_name")
	if hsName == "" {
		r.apiError(c, 400, "主机名称不能为空")
		return
	}
	
	server := r.hostManager.GetHost(hsName)
	if server == nil {
		r.apiError(c, 404, "主机不存在")
		return
	}
	
	statuses := server.GetAllHostStatus()
	r.apiSuccess(c, statuses)
}