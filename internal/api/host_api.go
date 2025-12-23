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

// getHosts 获取所有主机 - 与Python版本路由名一致
func (r *RestManager) getHosts(c *gin.Context) {
	hosts := r.hostManager.GetAllHosts()
	result := make(map[string]gin.H)
	
	for hsName, server := range hosts {
		config := server.GetConfig()
		vms := server.GetAllVMs()
		
		hostInfo := gin.H{
			"name":     hsName,
			"type":     config.ServerType,
			"addr":     config.ServerAddr,
			"vm_count": len(vms),
			"status":   "active",
			"config":   config,
		}
		result[hsName] = hostInfo
	}
	
	r.apiSuccess(c, result)
}

// addHost 添加主机 - 与Python版本路由名一致
func (r *RestManager) addHost(c *gin.Context) {
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	hsName, nameOk := data["name"].(string)
	hsType, typeOk := data["type"].(string)
	
	// 如果name和type字段不存在，尝试使用hs_name和hs_type
	if !nameOk || hsName == "" {
		hsName, nameOk = data["hs_name"].(string)
	}
	if !typeOk || hsType == "" {
		hsType, typeOk = data["hs_type"].(string)
	}
	
	// 添加调试日志
	r.logger.Debugf("[addHost] 接收到的data: %+v", data)
	r.logger.Debugf("[addHost] hsName: '%s', hsType: '%s' (nameOk: %t, typeOk: %t)", hsName, hsType, nameOk, typeOk)
	
	if hsName == "" || hsType == "" {
		r.logger.Debugf("[addHost] 参数验证失败 - hsName为空: %t, hsType为空: %t", hsName == "", hsType == "")
		r.apiError(c, 400, "主机名称和类型不能为空")
		return
	}

	// 构建配置
	configData, _ := data["config"].(map[string]interface{})
	if configData == nil {
		configData = make(map[string]interface{})
	}
	configData["server_type"] = hsType

	// 转换为HSConfig结构
	var hsConfig models.HSConfig
	if err := r.mapToStruct(configData, &hsConfig); err != nil {
		r.apiError(c, 400, "配置转换错误: "+err.Error())
		return
	}
	
	// 设置server_name，确保保存数据能正常工作，与Python版本保持一致
	hsConfig.ServerName = hsName

	result := r.hostManager.AddHost(hsName, hsType, &hsConfig)
	if result.Success {
		// Python版本会调用all_save()
		if err := r.hostManager.SaveAll(); err != nil {
			r.logger.Error("保存数据失败: " + err.Error())
		}
		r.apiResponse(c, 200, result.Message, nil)
	} else {
		r.apiError(c, 400, result.Message)
	}
}

// getHost 获取单个主机 - 与Python版本路由名一致
func (r *RestManager) getHost(c *gin.Context) {
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

	// 检查是否需要详细信息（通过查询参数控制）
	includeStatus := c.Query("status") == "true"

	config := server.GetConfig()
	vms := server.GetAllVMs()
	vmList := make([]string, 0)
	
	for vmUUID := range vms {
		vmList = append(vmList, vmUUID)
	}
	
	// 构建基础响应数据（快速获取）
	hostData := gin.H{
		"name":         hsName,
		"type":         config.ServerType,
		"addr":         config.ServerAddr,
		"config":       config,
		"vm_count":     len(vms),
		"vm_list":      vmList,
		"last_updated": 0, // 可以根据需要添加时间戳
	}

	// 只有明确要求时才获取状态信息（避免每次调用都执行耗时的系统检查）
	if includeStatus {
		hostData["status"] = gin.H{}
		hostData["status_source"] = "fresh"
		// 这里可以添加实际的状态获取逻辑
	} else {
		hostData["status"] = nil
		hostData["status_note"] = "Use ?status=true to get detailed host status"
	}
	
	r.apiSuccess(c, hostData)
}

// updateHost 更新主机
func (r *RestManager) updateHost(c *gin.Context) {
	hsName := c.Param("hs_name")
	if hsName == "" {
		r.apiError(c, 400, "主机名称不能为空")
		return
	}
	
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	// 添加调试日志
	r.logger.Infof("更新主机 %s，接收到的数据: %+v", hsName, data)
	
	// 支持两种数据格式：
	// 1. {"config": {...}} - Python标准格式
	// 2. {...} - 直接配置数据格式
	var configData map[string]interface{}
	if config, exists := data["config"]; exists {
		// 格式1：{"config": {...}}
		if configMap, ok := config.(map[string]interface{}); ok {
			configData = configMap
			r.logger.Infof("使用格式1（嵌套config），配置数据: %+v", configData)
		} else {
			r.apiError(c, 400, "配置格式错误：config必须是对象")
			return
		}
	} else {
		// 格式2：直接配置数据
		configData = data
		r.logger.Infof("使用格式2（直接配置），配置数据: %+v", configData)
	}
	
	if configData == nil || len(configData) == 0 {
		r.logger.Error("配置数据为空")
		r.apiError(c, 400, "配置不能为空")
		return
	}

	// 转换为HSConfig结构
	var hsConfig models.HSConfig
	if err := r.mapToStruct(configData, &hsConfig); err != nil {
		r.apiError(c, 400, "配置转换错误: "+err.Error())
		return
	}

	result := r.hostManager.UpdateHost(hsName, &hsConfig)
	if result.Success {
		// 成功后保存所有数据，与Python版本保持一致
		if err := r.hostManager.SaveAll(); err != nil {
			r.logger.Error("保存数据失败: " + err.Error())
			r.apiError(c, 500, "主机更新成功，但保存数据失败: "+err.Error())
			return
		}
		r.apiResponse(c, 200, result.Message, nil)
	} else {
		r.apiError(c, 400, result.Message)
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
		// 删除成功后保存所有数据，与Python版本保持一致
		if err := r.hostManager.SaveAll(); err != nil {
			r.logger.Error("保存数据失败: " + err.Error())
		}
		r.apiResponse(c, 200, "主机已删除", nil)
	} else {
		r.apiError(c, 404, "主机不存在")
	}
}

// powerHost 主机电源控制
func (r *RestManager) powerHost(c *gin.Context) {
	hsName := c.Param("hs_name")
	if hsName == "" {
		r.apiError(c, 400, "主机名称不能为空")
		return
	}
	
	// 从JSON body获取enable参数，与Python版本保持一致
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		// 如果没有JSON body，使用默认值true
		data = make(map[string]interface{})
	}
	
	enable, ok := data["enable"].(bool)
	if !ok {
		// 如果没有提供enable参数或类型不正确，使用默认值true
		enable = true
	}
	
	result := r.hostManager.PowerHost(hsName, enable)
	
	if result.Success {
		// 操作成功后保存所有数据，与Python版本保持一致
		if err := r.hostManager.SaveAll(); err != nil {
			r.logger.Error("保存数据失败: " + err.Error())
		}
		r.apiResponse(c, 200, result.Message, nil)
	} else {
		r.apiError(c, 400, result.Message)
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

// 主机电源控制（启用/禁用）
func (r *RestManager) hostPower(c *gin.Context) {
	hsName := c.Param("hs_name")
	
	// 检查主机是否存在
	host := r.hostManager.GetHost(hsName)
	if host == nil {
		r.apiError(c, 404, "主机不存在")
		return
	}
	
	// 解析请求数据
	var requestData struct {
		Enable bool `json:"enable"`
	}
	
	if err := c.ShouldBindJSON(&requestData); err != nil {
		// 如果没有JSON数据或解析失败，默认启用
		requestData.Enable = true
	}
	
	r.logger.Debugf("[hostPower] 控制主机 %s 电源，enable: %t", hsName, requestData.Enable)
	
	// 调用主机电源控制
	result := r.hostManager.PowerHost(hsName, requestData.Enable)
	
	if result.Success {
		// 保存数据（PowerHost方法内部已经调用了SaveData）
		if err := r.hostManager.SaveAll(); err != nil {
			r.logger.Error("保存数据失败: " + err.Error())
		}
		r.apiSuccess(c, nil)
	} else {
		r.apiError(c, 400, result.Message)
	}
}