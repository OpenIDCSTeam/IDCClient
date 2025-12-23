package api

import (
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/models"
	"github.com/gin-gonic/gin"
)

// RegisterSystemAPI 注册系统管理API
func (r *RestManager) RegisterSystemAPI(router *gin.RouterGroup) {
	system := router.Group("/system")
	{
		// Token管理
		system.GET("/token/current", r.getCurrentToken)
		system.POST("/token/set", r.setToken)
		system.POST("/token/reset", r.resetToken)

		// 引擎类型
		system.GET("/engine", r.getEngineTypes)

		// 配置管理
		system.POST("/saving", r.saveSystem)
		system.POST("/save", r.saveSystem) // 别名
		system.POST("/loader", r.loadSystem)
		system.POST("/load", r.loadSystem) // 别名

		// 统计信息
		system.GET("/statis", r.getSystemStats)
		system.GET("/stats", r.getStats) // 别名

		// 日志和任务
		system.GET("/logger/detail", r.getLogs)
		system.GET("/tasker", r.getTasks)
	}
}

// getCurrentToken 获取当前Token - 与Python版本一致
func (r *RestManager) getCurrentToken(c *gin.Context) {
	token := r.hostManager.GetBearer()
	r.apiResponse(c, 200, "获取Token成功", gin.H{"token": token})
}

// setToken 设置Token - 与Python版本一致
func (r *RestManager) setToken(c *gin.Context) {
	var req struct {
		Token string `json:"token"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		r.apiError(c, 400, "Token不能为空")
		return
	}

	if req.Token == "" {
		r.apiError(c, 400, "Token不能为空")
		return
	}

	newToken := r.hostManager.SetPassword(req.Token)
	r.apiResponse(c, 200, "Token设置成功", gin.H{"token": newToken})
}

// resetToken 重置Token - 与Python版本一致
func (r *RestManager) resetToken(c *gin.Context) {
	newToken := r.hostManager.SetPassword("") // 空字符串会生成新Token
	r.apiResponse(c, 200, "Token重置成功", gin.H{"token": newToken})
}

// getEngineTypes 获取引擎类型 - 与Python版本一致
func (r *RestManager) getEngineTypes(c *gin.Context) {
	engineConfigs := r.hostManager.GetEngineConfigs()
	r.apiSuccess(c, engineConfigs)
}

// saveSystem 保存系统配置 - 与Python版本一致
func (r *RestManager) saveSystem(c *gin.Context) {
	err := r.hostManager.SaveAll()
	if err != nil {
		r.apiError(c, 500, "保存失败: "+err.Error())
		return
	}
	r.apiResponse(c, 200, "配置已保存", nil)
}

// loadSystem 加载系统配置 - 与Python版本一致
func (r *RestManager) loadSystem(c *gin.Context) {
	err := r.hostManager.LoadAll()
	if err != nil {
		r.apiError(c, 500, "加载失败: "+err.Error())
		return
	}
	r.apiResponse(c, 200, "配置已加载", nil)
}

// getSystemStats 获取系统统计信息 - 与Python版本一致
func (r *RestManager) getSystemStats(c *gin.Context) {
	stats := r.hostManager.GetSystemStats()
	r.apiSuccess(c, stats)
}

// getStats 获取统计信息（别名）- 与Python版本一致
func (r *RestManager) getStats(c *gin.Context) {
	hostCount := r.hostManager.GetHostCount()
	totalVMs := 0
	runningVMs := 0

	hosts := r.hostManager.GetAllHosts()
	for _, server := range hosts {
		vms := server.GetAllVMs()
		totalVMs += len(vms)
		// 这里应该根据实际状态统计运行中的虚拟机
		// 暂时假设一半是运行状态
		runningVMs += len(vms) / 2
	}

	stats := gin.H{
		"host_count":       hostCount,
		"vm_count":         totalVMs,
		"running_vm_count": runningVMs,
	}

	r.apiSuccess(c, stats)
}

// getLogs 获取日志 - 与Python版本一致
func (r *RestManager) getLogs(c *gin.Context) {
	hsName := r.getQueryParam(c, "hs_name", "")
	limit := r.getQueryInt(c, "limit", 100)

	var logs []*models.ZMessage
	var err error

	if hsName != "" {
		logs, err = r.hostManager.GetHostLogs(hsName, limit)
	} else {
		logs, err = r.hostManager.GetAllLogs(limit)
	}

	if err != nil {
		r.apiError(c, 500, "获取日志失败: "+err.Error())
		return
	}

	// 处理日志数据，与Python版本格式一致
	processedLogs := make([]gin.H, 0)
	for _, log := range logs {
		processedLog := gin.H{
			"id":         "",
			"actions":    log.Action,
			"message":    log.Message,
			"success":    log.Success,
			"results":    log.Results,
			"execute":    log.Execute,
			"level":      func() string { if log.Success { return "INFO" } else { return "ERROR" } }(),
			"timestamp":  log.Time,
			"host":       hsName,
			"created_at": log.Time,
		}
		processedLogs = append(processedLogs, processedLog)
	}

	r.apiResponse(c, 200, "获取日志成功", processedLogs)
}

// getTasks 获取任务 - 与Python版本一致
func (r *RestManager) getTasks(c *gin.Context) {
	hsName := r.getQueryParam(c, "hs_name", "")
	_ = r.getQueryInt(c, "limit", 100) // limit参数暂时未使用，但保留以备将来扩展

	if hsName == "" {
		r.apiError(c, 400, "主机名称不能为空")
		return
	}

	// 这里需要实现任务获取逻辑
	// TODO: 使用limit参数来限制返回的任务数量
	// 暂时返回空列表
	tasks := []interface{}{}
	r.apiResponse(c, 200, "获取任务成功", tasks)
}