package api

import (
	"fmt"
	"strconv"
	
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/models"
	"github.com/gin-gonic/gin"
)

// RegisterVMAPI 注册虚拟机管理API
func (r *RestManager) RegisterVMAPI(router *gin.RouterGroup) {
	vms := router.Group("/vms")
	{
		// 虚拟机列表
		vms.GET("/", r.getVMs)
		vms.GET("/:hs_name", r.getVMsByHost)
		vms.GET("/:hs_name/:vm_uuid", r.getVM)
		
		// 虚拟机管理
		vms.POST("/:hs_name", r.createVM)
		vms.PUT("/:hs_name/:vm_uuid", r.updateVM)
		vms.DELETE("/:hs_name/:vm_uuid", r.deleteVM)
		
		// 电源控制
		vms.POST("/:hs_name/:vm_uuid/power", r.vmPower)
		
		// 密码管理
		vms.POST("/:hs_name/:vm_uuid/password", r.vmPassword)
		
		// 状态和监控
		vms.GET("/:hs_name/:vm_uuid/status", r.getVMStatus)
		vms.GET("/:hs_name/:vm_uuid/status/list", r.getVMStatusList)
		
		// VNC控制台
		vms.GET("/:hs_name/:vm_uuid/console", r.getVMConsole)
		
		// 扫描
		vms.POST("/:hs_name/scan", r.scanVMs)
		
		// 状态上报
		vms.POST("/upload/status", r.uploadVMStatus)
	}
}

// getVMs 获取所有虚拟机
func (r *RestManager) getVMs(c *gin.Context) {
	hsName := r.getQueryParam(c, "hs_name", "")
	if hsName == "" {
		r.apiError(c, 400, "主机名称不能为空")
		return
	}
	
	r.getVMsByHost(c)
}

// getVMsByHost 获取指定主机的虚拟机
func (r *RestManager) getVMsByHost(c *gin.Context) {
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

	// 从数据库重新加载数据
	err := server.LoadData()
	if err != nil {
		r.logger.Errorf("加载虚拟机数据失败: %v", err)
	}

	vmConfigs := server.GetAllVMs()
	vmsData := make(map[string]interface{})

	for vmUUID, vmConfig := range vmConfigs {
		// 从 DataManage 获取状态（直接从数据库读取）
		vmStatus := server.VMStatus(vmUUID)
		var status []interface{}
		
		// 只取最新的一条状态，与Python版本保持一致
		if len(vmStatus) > 0 {
			latestStatus := vmStatus[len(vmStatus)-1]
			if latestStatus != nil {
				status = []interface{}{latestStatus}
			}
		}

		vmsData[vmUUID] = map[string]interface{}{
			"uuid":   vmUUID,
			"config": vmConfig,
			"status": status,
		}
	}

	r.apiSuccess(c, vmsData)
}

// getVM 获取单个虚拟机
func (r *RestManager) getVM(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	server := r.hostManager.GetHost(hsName)
	if server == nil {
		r.apiError(c, 404, "主机不存在")
		return
	}

	// 获取虚拟机配置
	vmConfigs := server.GetAllVMs()
	vmConfig, exists := vmConfigs[vmUUID]
	if !exists {
		r.apiError(c, 404, "虚拟机不存在")
		return
	}

	// 获取虚拟机状态
	vmStatus := server.VMStatus(vmUUID)
	var statusResult []interface{}
	
	// 将HWStatus对象转换为字典，与Python版本保持一致
	if len(vmStatus) > 0 {
		for _, hwStatus := range vmStatus {
			if hwStatus != nil {
				statusResult = append(statusResult, hwStatus)
			} else {
				statusResult = append(statusResult, nil)
			}
		}
	}

	vmData := gin.H{
		"uuid":   vmUUID,
		"config": vmConfig,
		"status": statusResult,
	}
	
	r.apiSuccess(c, vmData)
}

// createVM 创建虚拟机
func (r *RestManager) createVM(c *gin.Context) {
	hsName := c.Param("hs_name")
	if hsName == "" {
		r.apiError(c, 400, "主机名称不能为空")
		return
	}
	
	var vmConfig models.VMConfig
	if err := c.ShouldBindJSON(&vmConfig); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	err := r.vmManager.CreateVM(hsName, &vmConfig)
	if err != nil {
		r.apiError(c, 500, "创建虚拟机失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, gin.H{"vm_uuid": vmConfig.VMUUID})
}

// updateVM 更新虚拟机
func (r *RestManager) updateVM(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	var vmConfig models.VMConfig
	if err := c.ShouldBindJSON(&vmConfig); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	err := r.vmManager.UpdateVM(hsName, vmUUID, &vmConfig)
	if err != nil {
		r.apiError(c, 500, "更新虚拟机失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, nil)
}

// deleteVM 删除虚拟机
func (r *RestManager) deleteVM(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	err := r.vmManager.DeleteVM(hsName, vmUUID)
	if err != nil {
		r.apiError(c, 500, "删除虚拟机失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, nil)
}

// vmPower 虚拟机电源控制
func (r *RestManager) vmPower(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	server := r.hostManager.GetHost(hsName)
	if server == nil {
		r.apiError(c, 404, "主机不存在")
		return
	}

	// 从JSON body获取action参数，与Python版本保持一致
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		// 如果没有JSON body，使用默认值
		data = make(map[string]interface{})
	}
	
	actionStr, ok := data["action"].(string)
	if !ok || actionStr == "" {
		actionStr = "start" // 默认启动
	}

	// 映射操作到VMPowers枚举，与Python版本保持一致
	var powerAction models.VMPowers
	switch actionStr {
	case "start":
		powerAction = models.VMPowerStart
	case "stop":
		powerAction = models.VMPowerStop
	case "hard_stop":
		powerAction = models.VMPowerHardStop
	case "reset":
		powerAction = models.VMPowerReset
	case "hard_reset":
		powerAction = models.VMPowerHardReset
	case "pause":
		powerAction = models.VMPowerPause
	case "resume":
		powerAction = models.VMPowerResume
	default:
		r.apiError(c, 400, fmt.Sprintf("不支持的操作: %s", actionStr))
		return
	}

	// 执行电源操作
	result := server.VMPower(vmUUID, powerAction)
	
	if result != nil && result.Success {
		message := result.Message
		if message == "" {
			message = fmt.Sprintf("电源操作 %s 成功", actionStr)
		}
		r.apiResponse(c, 200, message, nil)
	} else {
		message := "操作失败"
		if result != nil && result.Message != "" {
			message = result.Message
		}
		r.apiError(c, 400, message)
	}
}
func (r *RestManager) vmPassword(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	var req struct {
		Password string `json:"password" binding:"required"`
	}
	
	if err := c.ShouldBindJSON(&req); err != nil {
		r.apiError(c, 400, "密码不能为空")
		return
	}
	
	err := r.vmManager.VMPassword(hsName, vmUUID, req.Password)
	if err != nil {
		r.apiError(c, 500, "修改密码失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, nil)
}

// getVMStatus 获取虚拟机状态
func (r *RestManager) getVMStatus(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	statuses, err := r.vmManager.VMStatus(hsName, vmUUID)
	if err != nil {
		r.apiError(c, 500, "获取状态失败: "+err.Error())
		return
	}
	
	if len(statuses) == 0 {
		r.apiError(c, 404, "无状态数据")
		return
	}
	
	r.apiSuccess(c, statuses[len(statuses)-1]) // 返回最新状态
}

// getVMStatusList 获取虚拟机状态列表
func (r *RestManager) getVMStatusList(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	statuses, err := r.vmManager.VMStatus(hsName, vmUUID)
	if err != nil {
		r.apiError(c, 500, "获取状态失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, statuses)
}

// getVMConsole 获取虚拟机VNC控制台
func (r *RestManager) getVMConsole(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	consoleURL, err := r.vmManager.VMConsole(hsName, vmUUID)
	if err != nil {
		r.apiError(c, 500, "获取控制台失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, gin.H{"vnc_url": consoleURL})
}

// scanVMs 扫描虚拟机
func (r *RestManager) scanVMs(c *gin.Context) {
	hsName := c.Param("hs_name")
	if hsName == "" {
		r.apiError(c, 400, "主机名称不能为空")
		return
	}
	
	prefix := r.getQueryParam(c, "prefix", "")
	
	result, err := r.vmManager.ScanVMs(hsName, prefix)
	if err != nil {
		r.apiError(c, 500, "扫描失败: "+err.Error())
		return
	}
	
	if result.Success {
		r.apiSuccess(c, result.Results)
	} else {
		r.apiError(c, 500, result.Message)
	}
}

// ============================================================================
// NAT端口转发管理
// ============================================================================

// getVMNATRules 获取虚拟机NAT端口转发规则
func (r *RestManager) getVMNATRules(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	rules, err := r.vmManager.GetNATRules(hsName, vmUUID)
	if err != nil {
		r.apiError(c, 500, "获取NAT规则失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, rules)
}

// addVMNATRule 添加虚拟机NAT端口转发规则
func (r *RestManager) addVMNATRule(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	var rule models.PortData
	if err := c.ShouldBindJSON(&rule); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	err := r.vmManager.AddNATRule(hsName, vmUUID, &rule)
	if err != nil {
		r.apiError(c, 500, "添加NAT规则失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, nil)
}

// deleteVMNATRule 删除虚拟机NAT端口转发规则
func (r *RestManager) deleteVMNATRule(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	ruleIndex := c.Param("rule_index")
	
	if hsName == "" || vmUUID == "" || ruleIndex == "" {
		r.apiError(c, 400, "主机名称、虚拟机UUID和规则索引不能为空")
		return
	}
	
	index, err := strconv.Atoi(ruleIndex)
	if err != nil {
		r.apiError(c, 400, "规则索引必须是数字")
		return
	}
	
	err = r.vmManager.DeleteNATRule(hsName, vmUUID, index)
	if err != nil {
		r.apiError(c, 500, "删除NAT规则失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, nil)
}

// ============================================================================
// IP地址管理
// ============================================================================

// getVMIPAddresses 获取虚拟机IP地址列表
func (r *RestManager) getVMIPAddresses(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	ipAddresses, err := r.vmManager.GetIPAddresses(hsName, vmUUID)
	if err != nil {
		r.apiError(c, 500, "获取IP地址失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, ipAddresses)
}

// addVMIPAddress 添加虚拟机IP地址
func (r *RestManager) addVMIPAddress(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	var req struct {
		NicName  string `json:"nic_name" binding:"required"`
		IPConfig models.NCConfig `json:"ip_config" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	err := r.vmManager.AddIPAddress(hsName, vmUUID, req.NicName, &req.IPConfig)
	if err != nil {
		r.apiError(c, 500, "添加IP地址失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, nil)
}

// updateVMIPAddress 修改虚拟机IP地址配置
func (r *RestManager) updateVMIPAddress(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	nicName := c.Param("nic_name")
	
	if hsName == "" || vmUUID == "" || nicName == "" {
		r.apiError(c, 400, "主机名称、虚拟机UUID和网卡名称不能为空")
		return
	}
	
	var ipConfig models.NCConfig
	if err := c.ShouldBindJSON(&ipConfig); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	err := r.vmManager.UpdateIPAddress(hsName, vmUUID, nicName, &ipConfig)
	if err != nil {
		r.apiError(c, 500, "修改IP地址失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, nil)
}

// deleteVMIPAddress 删除虚拟机IP地址（按索引）
func (r *RestManager) deleteVMIPAddress(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	ipIndex := c.Param("ip_index")
	
	if hsName == "" || vmUUID == "" || ipIndex == "" {
		r.apiError(c, 400, "主机名称、虚拟机UUID和IP索引不能为空")
		return
	}
	
	index, err := strconv.Atoi(ipIndex)
	if err != nil {
		r.apiError(c, 400, "IP索引必须是数字")
		return
	}
	
	err = r.vmManager.DeleteIPAddress(hsName, vmUUID, index)
	if err != nil {
		r.apiError(c, 500, "删除IP地址失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, nil)
}

// deleteVMIPAddressByName 删除虚拟机IP地址（按网卡名称）
func (r *RestManager) deleteVMIPAddressByName(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	nicName := c.Param("nic_name")
	
	if hsName == "" || vmUUID == "" || nicName == "" {
		r.apiError(c, 400, "主机名称、虚拟机UUID和网卡名称不能为空")
		return
	}
	
	err := r.vmManager.DeleteIPAddressByName(hsName, vmUUID, nicName)
	if err != nil {
		r.apiError(c, 500, "删除IP地址失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, nil)
}

// ============================================================================
// 反向代理管理
// ============================================================================

// getVMProxyConfigs 获取虚拟机反向代理配置列表
func (r *RestManager) getVMProxyConfigs(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	proxyConfigs, err := r.vmManager.GetProxyConfigs(hsName, vmUUID)
	if err != nil {
		r.apiError(c, 500, "获取反向代理配置失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, proxyConfigs)
}

// addVMProxyConfig 添加虚拟机反向代理配置
func (r *RestManager) addVMProxyConfig(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	var proxyConfig models.WebProxy
	if err := c.ShouldBindJSON(&proxyConfig); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	err := r.vmManager.AddProxyConfig(hsName, vmUUID, &proxyConfig)
	if err != nil {
		r.apiError(c, 500, "添加反向代理配置失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, nil)
}

// deleteVMProxyConfig 删除虚拟机反向代理配置
func (r *RestManager) deleteVMProxyConfig(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	proxyIndex := c.Param("proxy_index")
	
	if hsName == "" || vmUUID == "" || proxyIndex == "" {
		r.apiError(c, 400, "主机名称、虚拟机UUID和代理索引不能为空")
		return
	}
	
	index, err := strconv.Atoi(proxyIndex)
	if err != nil {
		r.apiError(c, 400, "代理索引必须是数字")
		return
	}
	
	err = r.vmManager.DeleteProxyConfig(hsName, vmUUID, index)
	if err != nil {
		r.apiError(c, 500, "删除反向代理配置失败: "+err.Error())
		return
	}
	
	r.apiSuccess(c, nil)
}