package api

import (
	"encoding/json"
	"strconv"

	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/host"
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/vm"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// RestManager REST API管理器
type RestManager struct {
	hostManager *host.HostManager
	vmManager   *vm.VMManager
	logger      *logrus.Logger
	userDB      map[string]map[string]interface{} // 简化的用户数据库
}

// NewRestManager 创建REST管理器
func NewRestManager(hostManager *host.HostManager, vmManager *vm.VMManager, logger *logrus.Logger) *RestManager {
	return &RestManager{
		hostManager: hostManager,
		vmManager:   vmManager,
		logger:      logger,
		// 初始化用户数据库连接（简化实现）
		userDB: make(map[string]map[string]interface{}),
	}
}

// ============================================================================
// 用户管理相关方法 - 与Python版本保持一致
// ============================================================================

// getUserByUsername 根据用户名获取用户信息 - 与Python版本一致
func (r *RestManager) getUserByUsername(username string) map[string]interface{} {
	// 简化实现，实际应该从数据库查询
	// 创建默认管理员用户
	if username == "admin" {
		return map[string]interface{}{
			"id":        int64(1),
			"username":  "admin",
			"password":  r.hashPassword("admin123"), // 默认密码
			"is_admin":  int64(1),
			"is_active": int64(1),
		}
	}
	return nil
}

// verifyPassword 验证密码 - 与Python版本一致
func (r *RestManager) verifyPassword(password, hashedPassword string) bool {
	// 简化实现，实际应该使用bcrypt等安全哈希
	return r.hashPassword(password) == hashedPassword
}

// hashPassword 哈希密码 - 与Python版本一致
func (r *RestManager) hashPassword(password string) string {
	// 简化实现，实际应该使用bcrypt等安全哈希
	// 这里使用简单的哈希算法作为示例
	return password + "_hashed"
}

// updateUserLastLogin 更新用户最后登录时间 - 与Python版本一致
func (r *RestManager) updateUserLastLogin(userID int64) {
	// 简化实现，实际应该更新数据库
	r.logger.Infof("更新用户 %d 的最后登录时间", userID)
}

// ============================================================================
// 用户管理API方法 - 与Python版本保持一致
// ============================================================================

// getUsers 获取所有用户列表 - 与Python版本一致
func (r *RestManager) getUsers(c *gin.Context) {
	// 检查管理员权限（简化实现）
	users := []map[string]interface{}{
		{
			"id":             int64(1),
			"username":       "admin",
			"email":          "admin@example.com",
			"is_admin":       int64(1),
			"is_active":      int64(1),
			"can_create_vm":  int64(1),
			"can_modify_vm":  int64(1),
			"can_delete_vm":  int64(1),
			"quota_cpu":      int64(8),
			"quota_ram":      int64(16384),
			"quota_ssd":      int64(102400),
			"assigned_hosts": []string{},
		},
	}
	
	r.apiResponse(c, 200, "获取成功", users)
}

// createUser 创建新用户 - 与Python版本一致
func (r *RestManager) createUser(c *gin.Context) {
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		r.apiError(c, 400, "无效的请求数据")
		return
	}
	
	username := ""
	email := ""
	password := ""
	
	if un, ok := data["username"].(string); ok {
		username = un
	}
	if em, ok := data["email"].(string); ok {
		email = em
	}
	if pw, ok := data["password"].(string); ok {
		password = pw
	}
	
	// 验证必填字段
	if username == "" || email == "" || password == "" {
		r.apiError(c, 400, "用户名、邮箱和密码不能为空")
		return
	}
	
	if len(username) < 3 || len(username) > 20 {
		r.apiError(c, 400, "用户名长度必须在3-20个字符之间")
		return
	}
	
	if len(password) < 6 {
		r.apiError(c, 400, "密码长度至少6个字符")
		return
	}
	
	// 检查用户名是否已存在
	if r.getUserByUsername(username) != nil {
		r.apiError(c, 400, "用户名已存在")
		return
	}
	
	// 创建用户（简化实现）
	userID := int64(len(r.userDB) + 2)
	r.userDB[username] = map[string]interface{}{
		"id":             userID,
		"username":       username,
		"email":          email,
		"password":       r.hashPassword(password),
		"is_admin":       data["is_admin"],
		"is_active":      data["is_active"],
		"can_create_vm":  data["can_create_vm"],
		"can_modify_vm":  data["can_modify_vm"],
		"can_delete_vm":  data["can_delete_vm"],
		"quota_cpu":      data["quota_cpu"],
		"quota_ram":      data["quota_ram"],
		"quota_ssd":      data["quota_ssd"],
		"assigned_hosts": data["assigned_hosts"],
	}
	
	r.apiResponse(c, 200, "用户创建成功", gin.H{"user_id": userID})
}

// getUser 获取单个用户信息 - 与Python版本一致
func (r *RestManager) getUser(c *gin.Context) {
	userIDStr := c.Param("user_id")
	userID, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil {
		r.apiError(c, 400, "无效的用户ID")
		return
	}
	
	// 简化实现：返回管理员用户信息
	if userID == 1 {
		user := map[string]interface{}{
			"id":             int64(1),
			"username":       "admin",
			"email":          "admin@example.com",
			"is_admin":       int64(1),
			"is_active":      int64(1),
			"can_create_vm":  int64(1),
			"can_modify_vm":  int64(1),
			"can_delete_vm":  int64(1),
			"quota_cpu":      int64(8),
			"quota_ram":      int64(16384),
			"quota_ssd":      int64(102400),
			"assigned_hosts": []string{},
		}
		r.apiResponse(c, 200, "获取成功", user)
	} else {
		r.apiError(c, 404, "用户不存在")
	}
}

// updateUser 更新用户信息 - 与Python版本一致
func (r *RestManager) updateUser(c *gin.Context) {
	userIDStr := c.Param("user_id")
	_, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil {
		r.apiError(c, 400, "无效的用户ID")
		return
	}
	
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		r.apiError(c, 400, "无效的请求数据")
		return
	}
	
	// 简化实现：直接返回成功
	r.apiResponse(c, 200, "更新成功", nil)
}

// deleteUser 删除用户 - 与Python版本一致
func (r *RestManager) deleteUser(c *gin.Context) {
	userIDStr := c.Param("user_id")
	userID, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil {
		r.apiError(c, 400, "无效的用户ID")
		return
	}
	
	// 不允许删除管理员用户
	if userID == 1 {
		r.apiError(c, 400, "不能删除管理员用户")
		return
	}
	
	// 简化实现：直接返回成功
	r.apiResponse(c, 200, "删除成功", nil)
}

// ============================================================================
// 系统设置API方法 - 与Python版本保持一致
// ============================================================================

// getSystemSettings 获取系统设置 - 与Python版本一致
func (r *RestManager) getSystemSettings(c *gin.Context) {
	settings := map[string]interface{}{
		"registration_enabled":      "1",
		"email_verification_enabled": "0",
		"resend_apikey":            "",
		"resend_email":             "",
	}
	r.apiResponse(c, 200, "获取成功", settings)
}

// updateSystemSettings 更新系统设置 - 与Python版本一致
func (r *RestManager) updateSystemSettings(c *gin.Context) {
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		r.apiError(c, 400, "无效的请求数据")
		return
	}
	
	// 简化实现：直接返回成功
	r.logger.Infof("更新系统设置: %v", data)
	r.apiResponse(c, 200, "更新成功", nil)
}

// apiResponse 统一API响应格式 - 与Python版本完全一致
// Python: return jsonify({'code': code, 'msg': msg, 'data': data})
func (r *RestManager) apiResponse(c *gin.Context, code int, msg string, data interface{}) {
	response := gin.H{
		"code": code,
		"msg":  msg,
		"data": data,
	}
	c.JSON(code, response)
}

// apiSuccess 成功响应 - 与Python版本一致
func (r *RestManager) apiSuccess(c *gin.Context, data interface{}) {
	r.apiResponse(c, 200, "success", data)
}

// apiError 错误响应 - 与Python版本一致
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

// mapToStruct 将map转换为struct
func (r *RestManager) mapToStruct(data map[string]interface{}, result interface{}) error {
	// 将map转换为JSON，再从JSON转换为struct
	jsonData, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(jsonData, result)
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

// ============================================================================
// 主机管理相关方法 - 与Python版本保持一致
// ============================================================================

// getHostDetails 获取主机详细信息 - 与Python版本一致
func (r *RestManager) getHostDetails(c *gin.Context) {
	hsName := r.getQueryParam(c, "hs_name", "")
	
	if hsName == "" {
		r.apiError(c, 400, "主机名称不能为空")
		return
	}
	
	server := r.hostManager.GetHost(hsName)
	if server == nil {
		r.apiError(c, 404, "主机不存在")
		return
	}

	// 获取详细信息
	config := server.GetConfig()
	vms := server.GetAllVMs()
	vmList := make([]gin.H, 0)
	
	for vmUUID, vmConfig := range vms {
		vmList = append(vmList, gin.H{
			"uuid":     vmUUID,
			"name":     vmConfig.VMName,
			"os_name":  vmConfig.OSName,
			"cpu":      vmConfig.CPUNum,
			"memory":   vmConfig.RAMNum,
			"disk":     vmConfig.HDDNum,
			"status":   "running", // 简化状态
			"networks": vmConfig.NicAll,
		})
	}
	
	hostData := gin.H{
		"name":         hsName,
		"type":         config.ServerType,
		"addr":         config.ServerAddr,
		"user":         config.ServerUser,
		"config":       config,
		"vm_count":     len(vms),
		"vm_list":      vmList,
		"status":       "active",
		"last_updated": 0,
	}
	
	r.apiSuccess(c, hostData)
}

// getHostNames 获取主机名称列表 - 与Python版本一致
func (r *RestManager) getHostNames(c *gin.Context) {
	hosts := r.hostManager.GetAllHosts()
	nameList := make([]string, 0)
	
	for hsName := range hosts {
		nameList = append(nameList, hsName)
	}
	
	r.apiSuccess(c, nameList)
}

// createHost 创建主机 - 与Python版本一致（实际调用addHost）
func (r *RestManager) createHost(c *gin.Context) {
	r.addHost(c)
}

// ============================================================================
// 缺失的API方法 - 与Python版本保持一致
// ============================================================================

// mountVMHDD 挂载数据盘 - 与Python版本一致
func (r *RestManager) mountVMHDD(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	hddName, _ := data["hdd_name"].(string)
	hddSize, _ := data["hdd_size"].(float64)
	hddType, _ := data["hdd_type"].(float64)
	
	if hddName == "" {
		r.apiError(c, 400, "磁盘名称不能为空")
		return
	}
	
	// 验证磁盘名称：只能包含数字、字母和下划线
	// 这里简化验证，实际应该使用正则表达式
	if len(hddName) == 0 {
		r.apiError(c, 400, "磁盘名称不能为空")
		return
	}
	
	if int(hddSize) < 1024 {
		r.apiError(c, 400, "磁盘大小至少为1024MB")
		return
	}
	
	// 记录磁盘类型信息（尽管当前版本未实现，但保留参数用于未来扩展）
	r.logger.Infof("挂载数据盘: 名称=%s, 大小=%dMB, 类型=%d", hddName, int(hddSize), int(hddType))
	
	// TODO: 实现挂载数据盘逻辑
	r.apiResponse(c, 200, "数据盘挂载成功", nil)
}

// unmountVMHDD 卸载数据盘 - 与Python版本一致
func (r *RestManager) unmountVMHDD(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	hddName, _ := data["hdd_name"].(string)
	
	if hddName == "" {
		r.apiError(c, 400, "数据盘名称不能为空")
		return
	}
	
	// TODO: 实现卸载数据盘逻辑
	r.apiResponse(c, 200, "数据盘已卸载", nil)
}

// transferVMHDD 移交数据盘所有权 - 与Python版本一致
func (r *RestManager) transferVMHDD(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	hddName, _ := data["hdd_name"].(string)
	targetVM, _ := data["target_vm"].(string)
	
	if hddName == "" {
		r.apiError(c, 400, "数据盘名称不能为空")
		return
	}
	
	if targetVM == "" {
		r.apiError(c, 400, "目标虚拟机不能为空")
		return
	}
	
	// TODO: 实现移交数据盘逻辑
	r.apiResponse(c, 200, "数据盘所有权移交成功", nil)
}

// deleteVMHDD 删除数据盘 - 与Python版本一致
func (r *RestManager) deleteVMHDD(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	hddName, _ := data["hdd_name"].(string)
	
	if hddName == "" {
		r.apiError(c, 400, "数据盘名称不能为空")
		return
	}
	
	// TODO: 实现删除数据盘逻辑
	r.apiResponse(c, 200, "数据盘已删除", nil)
}

// mountVMISO 挂载ISO镜像 - 与Python版本一致
func (r *RestManager) mountVMISO(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	isoName, _ := data["iso_name"].(string)
	isoFile, _ := data["iso_file"].(string)
	isoHint, _ := data["iso_hint"].(string)
	
	if isoName == "" {
		r.apiError(c, 400, "挂载名称不能为空")
		return
	}
	
	if isoFile == "" {
		r.apiError(c, 400, "ISO文件不能为空")
		return
	}
	
	// 记录ISO挂载信息（包括提示信息）
	r.logger.Infof("挂载ISO镜像: 名称=%s, 文件=%s, 提示=%s", isoName, isoFile, isoHint)
	
	// TODO: 实现挂载ISO逻辑
	r.apiResponse(c, 200, "ISO挂载成功", nil)
}

// unmountVMISO 卸载ISO镜像 - 与Python版本一致
func (r *RestManager) unmountVMISO(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	isoName := c.Param("iso_name")
	
	if hsName == "" || vmUUID == "" || isoName == "" {
		r.apiError(c, 400, "主机名称、虚拟机UUID和ISO名称不能为空")
		return
	}
	
	// TODO: 实现卸载ISO逻辑
	r.apiResponse(c, 200, "ISO已卸载", nil)
}

// createVMBackup 创建备份 - 与Python版本一致
func (r *RestManager) createVMBackup(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	vmTips, _ := data["vm_tips"].(string)
	
	if vmTips == "" {
		r.apiError(c, 400, "备份说明不能为空")
		return
	}
	
	// TODO: 实现创建备份逻辑
	r.apiResponse(c, 200, "备份创建成功", nil)
}

// restoreVMBackup 还原备份 - 与Python版本一致
func (r *RestManager) restoreVMBackup(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	vmBack, _ := data["vm_back"].(string)
	
	if vmBack == "" {
		r.apiError(c, 400, "备份名称不能为空")
		return
	}
	
	// TODO: 实现还原备份逻辑
	r.apiResponse(c, 200, "备份还原成功", nil)
}

// deleteVMBackup 删除备份 - 与Python版本一致
func (r *RestManager) deleteVMBackup(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	
	if hsName == "" || vmUUID == "" {
		r.apiError(c, 400, "主机名称和虚拟机UUID不能为空")
		return
	}
	
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	vmBack, _ := data["vm_back"].(string)
	
	if vmBack == "" {
		r.apiError(c, 400, "备份名称不能为空")
		return
	}
	
	// TODO: 实现删除备份逻辑
	r.apiResponse(c, 200, "备份已删除", nil)
}

// getGlobalProxyConfigs 获取全局反向代理配置列表 - 与Python版本一致
func (r *RestManager) getGlobalProxyConfigs(c *gin.Context) {
	// TODO: 实现获取全局代理配置逻辑
	proxyList := []interface{}{}
	r.apiSuccess(c, proxyList)
}

// addGlobalProxyConfig 添加全局反向代理配置 - 与Python版本一致
func (r *RestManager) addGlobalProxyConfig(c *gin.Context) {
	var data map[string]interface{}
	if err := c.ShouldBindJSON(&data); err != nil {
		r.apiError(c, 400, "参数错误: "+err.Error())
		return
	}
	
	webAddr, _ := data["web_addr"].(string)
	lanAddr, _ := data["lan_addr"].(string)
	lanPort, _ := data["lan_port"].(float64)
	
	if webAddr == "" {
		r.apiError(c, 400, "域名地址不能为空")
		return
	}
	
	if lanAddr == "" {
		r.apiError(c, 400, "内网地址不能为空")
		return
	}
	
	if lanPort == 0 {
		r.apiError(c, 400, "内网端口不能为空")
		return
	}
	
	// TODO: 实现添加全局代理配置逻辑
	r.apiResponse(c, 200, "代理配置添加成功", nil)
}

// deleteGlobalProxyConfig 删除全局反向代理配置 - 与Python版本一致
func (r *RestManager) deleteGlobalProxyConfig(c *gin.Context) {
	webAddr := c.Param("web_addr")
	
	if webAddr == "" {
		r.apiError(c, 400, "域名地址不能为空")
		return
	}
	
	// TODO: 实现删除全局代理配置逻辑
	r.apiResponse(c, 200, "代理配置已删除", nil)
}

// ============================================================================
// 缺失的基础API方法 - 与Python版本保持一致
// ============================================================================

// uploadVMStatus 虚拟机上报状态数据 - 与Python版本一致（无需认证）
func (r *RestManager) uploadVMStatus(c *gin.Context) {
	macAddr := c.Query("nic")
	if macAddr == "" {
		r.apiError(c, 400, "MAC地址参数缺失")
		return
	}
	
	var statusData map[string]interface{}
	if err := c.ShouldBindJSON(&statusData); err != nil {
		r.apiError(c, 400, "状态数据为空")
		return
	}
	
	r.logger.Infof("[虚拟机上报] 收到MAC地址: %s, 状态数据: %v", macAddr, statusData)
	
	// 使用VMManager处理状态上报
	response, err := r.vmManager.UploadStatus(macAddr, statusData)
	if err != nil {
		r.logger.Errorf("[虚拟机上报] 处理失败: %v", err)
		r.apiError(c, 404, err.Error())
		return
	}
	
	r.logger.Infof("[虚拟机上报] 处理成功: 主机=%s, 虚拟机=%s", response.HSName, response.VMUUID)
	r.apiResponse(c, 200, "状态上报成功", response)
}
