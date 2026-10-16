// Package api 系统管理和主机管理处理器
package api

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/auth"
	datastore "github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/driver"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/logger"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/service"
	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
)

// splitCSV 解析逗号分隔的查询参数（空值返回 nil，表示不过滤）。
func splitCSV(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// sortedKeys 返回 map 的键名（排序后），用于日志中展示本次提交的字段集合。
func sortedKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ============================================================
// 系统管理 API
// ============================================================

// handleSystemEngine GET /api/system/engine
// 返回结构与 Python 版 get_engine_types 一致：
//
//	{ current_platform, current_arch, engine_types: { 类型: { name, description, enabled, ... } } }
//
// 前端以 Object.entries(engine_types).map(([type, cfg]) => cfg.enabled ? <Option>{cfg.description}</Option> : null)
// 渲染「服务器类型」下拉框，因此每个值必须是对象而非字符串。
func (app *App) handleSystemEngine(c *gin.Context) {
	utils.Success(c, map[string]interface{}{
		"current_platform": driver.CurrentPlatform(),
		"current_arch":     driver.CurrentCPUArch(),
		"engine_types":     driver.EngineTypes(),
	})
}

// handleSystemSave POST /api/system/saving
func (app *App) handleSystemSave(c *gin.Context) {
	// 触发所有已启用主机的驱动保存配置
	savedCount := 0
	hosts, err := app.DB.GetAllHosts()
	if err != nil {
		utils.InternalError(c, "获取主机列表失败")
		return
	}

	for _, host := range hosts {
		if host.EnableHost == 0 {
			continue
		}
		d := app.HostSvc.GetDriver(host.HsName)
		if d != nil {
			// 驱动保存方法（如果驱动支持）
			if saver, ok := d.(interface{ Save() error }); ok {
				if err := saver.Save(); err != nil {
					app.Logger.Log("system", "WARNING", "系统保存", fmt.Sprintf("保存主机配置失败 %s: %v", host.HsName, err), false)
					continue
				}
			}
			savedCount++
		}
	}

	app.Logger.Log("system", "INFO", "系统保存", fmt.Sprintf("系统配置保存完成，已保存 %d 个主机", savedCount), true)
	utils.SuccessMsg(c, "配置已保存", map[string]interface{}{
		"saved_count": savedCount,
	})
}

// handleSystemLoad POST /api/system/loader
func (app *App) handleSystemLoad(c *gin.Context) {
	if err := app.HostSvc.LoadAllDrivers(); err != nil {
		utils.InternalError(c, "加载配置失败: "+err.Error())
		return
	}
	utils.SuccessMsg(c, "配置已加载", nil)
}

// handleSystemStatis GET /api/system/statis
func (app *App) handleSystemStatis(c *gin.Context) {
	hosts, err := app.DB.GetAllHosts()
	if err != nil {
		utils.InternalError(c, "获取统计信息失败")
		return
	}

	result := map[string]interface{}{
		"host_count": len(hosts),
		"vm_count":   0,
		"hosts":      []interface{}{},
	}

	vmCount := 0
	hostStats := []map[string]interface{}{}
	for _, h := range hosts {
		vms, _ := app.DB.GetVMSaving(h.HsName)
		vmCount += len(vms)
		hostStats = append(hostStats, map[string]interface{}{
			"hs_name":     h.HsName,
			"server_type": h.ServerType,
			"vm_count":    len(vms),
			"enable_host": h.EnableHost == 1,
		})
	}
	result["vm_count"] = vmCount
	result["hosts"] = hostStats
	utils.Success(c, result)
}

// handleSystemStats GET /api/system/stats
func (app *App) handleSystemStats(c *gin.Context) {
	stats, err := app.SystemSvc.GetSystemStats(app.DB)
	if err != nil {
		utils.InternalError(c, "获取统计信息失败")
		return
	}
	utils.Success(c, stats)
}

// handleTokenCurrent GET /api/token/current
func (app *App) handleTokenCurrent(c *gin.Context) {
	utils.Success(c, gin.H{"token": app.SystemSvc.GetToken()})
}

// handleTokenSet POST /api/token/set
func (app *App) handleTokenSet(c *gin.Context) {
	var req struct {
		Token string `json:"token"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Token == "" {
		utils.BadRequest(c, "Token不能为空")
		return
	}
	if err := app.SystemSvc.SetToken(req.Token); err != nil {
		utils.InternalError(c, "设置Token失败")
		return
	}
	utils.SuccessMsg(c, "Token已设置", nil)
}

// handleTokenReset POST /api/token/reset
func (app *App) handleTokenReset(c *gin.Context) {
	token, err := app.SystemSvc.ResetToken()
	if err != nil {
		utils.InternalError(c, "重置Token失败")
		return
	}
	utils.Success(c, gin.H{"token": token})
}

// handleLoggerDetail GET /api/system/logger/detail
//
// 支持过滤：
//
//	hs_name  主机名
//	level    级别（支持逗号分隔多级，如 "WARNING,ERROR"）
//	module   模块（支持逗号分隔，如 "vm,task"）
//	operator 操作人
//	success  结果（true/false）
//	keyword  关键字（匹配操作/描述/详情/操作人/虚拟机）
//	days     只查最近 N 天
//	limit/offset 分页
func (app *App) handleLoggerDetail(c *gin.Context) {
	query := datastore.LogQuery{
		HsName:   strings.TrimSpace(c.Query("hs_name")),
		Operator: strings.TrimSpace(c.Query("operator")),
		Keyword:  strings.TrimSpace(c.Query("keyword")),
		Modules:  splitCSV(c.Query("module")),
		Levels:   splitCSV(c.Query("level")),
		Limit:    100,
	}
	if v := strings.TrimSpace(c.Query("success")); v != "" {
		b := v == "1" || strings.EqualFold(v, "true")
		query.Success = &b
	}
	if n, err := strconv.Atoi(c.Query("days")); err == nil && n > 0 {
		query.SinceDays = n
	}
	if n, err := strconv.Atoi(c.Query("limit")); err == nil && n > 0 {
		query.Limit = n
	}
	if n, err := strconv.Atoi(c.Query("offset")); err == nil && n > 0 {
		query.Offset = n
	}

	logs, err := app.Logger.QueryLogs(query)
	if err != nil {
		utils.InternalError(c, "获取日志失败")
		return
	}
	if logs == nil {
		logs = []map[string]interface{}{}
	}
	utils.Success(c, logs)
}

// handleLoggerStats GET /api/system/logger/stats
// 返回级别与模块分布，用于展示「哪些日志重要、哪些只是噪声」。
func (app *App) handleLoggerStats(c *gin.Context) {
	days := 7
	if n, err := strconv.Atoi(c.Query("days")); err == nil && n > 0 {
		days = n
	}
	stats, err := app.Logger.Stats(days)
	if err != nil {
		utils.InternalError(c, "统计日志失败")
		return
	}
	utils.Success(c, stats)
}

// handleLoggerClear POST /api/system/logger/clear
func (app *App) handleLoggerClear(c *gin.Context) {
	var req struct {
		HsName string `json:"hs_name"`
	}
	_ = c.ShouldBindJSON(&req)
	if err := app.Logger.ClearLogs(req.HsName); err != nil {
		utils.InternalError(c, "清空日志失败")
		return
	}
	utils.SuccessMsg(c, "日志已清空", nil)
}

// handleTasker GET /api/system/tasker
func (app *App) handleTasker(c *gin.Context) {
	hsName := c.Query("hs_name")
	tasks, err := app.DB.GetTasks(hsName, 50)
	if err != nil {
		utils.InternalError(c, "获取任务失败")
		return
	}
	if tasks == nil {
		tasks = []map[string]interface{}{}
	}
	utils.Success(c, tasks)
}

// handleGetSettings GET /api/system/settings
func (app *App) handleGetSettings(c *gin.Context) {
	settings, err := app.SystemSvc.GetSettings()
	if err != nil {
		utils.InternalError(c, "获取设置失败")
		return
	}
	utils.Success(c, settings)
}

// handleUpdateSettings POST /api/system/settings
func (app *App) handleUpdateSettings(c *gin.Context) {
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	// 合并到现有设置
	existing, _ := app.SystemSvc.GetSettings()
	for k, v := range req {
		existing[k] = v
	}
	if err := app.SystemSvc.UpdateSettings(existing); err != nil {
		utils.InternalError(c, "更新设置失败")
		return
	}
	utils.SuccessMsg(c, "设置已更新", nil)
}

// handleSystemIPv4 GET /api/system/ipv4
func (app *App) handleSystemIPv4(c *gin.Context) {
	ips, err := service.GetSystemIPv4()
	if err != nil {
		utils.InternalError(c, "获取IP地址失败")
		return
	}
	utils.Success(c, ips)
}

// handleTestEmail POST /api/system/test-email
func (app *App) handleTestEmail(c *gin.Context) {
	var req struct {
		TestEmail    string `json:"test_email"`
		Subject      string `json:"subject"`
		Body         string `json:"body"`
		ResendEmail  string `json:"resend_email"`
		ResendAPIKey string `json:"resend_apikey"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}

	emailSvc := &auth.EmailService{
		APIKey:    req.ResendAPIKey,
		FromEmail: req.ResendEmail,
	}
	if err := emailSvc.SendTestEmail(req.TestEmail, req.Subject, req.Body); err != nil {
		utils.Fail(c, 500, "发送测试邮件失败: "+err.Error())
		return
	}
	utils.SuccessMsg(c, "测试邮件已发送", nil)
}

// handleRecalculateQuotas POST /api/system/recalculate-quotas
//
// 复用 QuotaService.RecalculateAll，而不是在此处另写一份扫描逻辑：
// 原实现为每个用户重复读取全部主机与虚拟机（复杂度 O(用户数 × 虚拟机数)），
// 且用 map 首项决定归属，与调度器的重算结果可能互相覆盖。
func (app *App) handleRecalculateQuotas(c *gin.Context) {
	if app.QuotaSvc == nil {
		utils.InternalError(c, "配额服务未初始化")
		return
	}
	if err := app.QuotaSvc.RecalculateAll(); err != nil {
		app.Logger.Log("system", "ERROR", "配额重算", fmt.Sprintf("重算失败: %v", err), false)
		utils.InternalError(c, "配额重新计算失败")
		return
	}
	utils.SuccessMsg(c, "配额重新计算完成", nil)
}

// ============================================================
// 主机管理 API
// ============================================================

// handleHostList GET /api/server/detail
// 返回结构与 Python get_hosts 一致：
// 每个主机返回 { name, type, addr, server_*, server_area, status, config, vm_count, enable_host }。
// 前端以 data[hs].status === "active" 过滤启用主机，因此 status 字段必不可少。
func (app *App) handleHostList(c *gin.Context) {
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}

	hosts, err := app.HostSvc.GetAllHosts()
	if err != nil {
		utils.InternalError(c, "获取主机列表失败")
		return
	}

	result := make(map[string]interface{}, len(hosts))
	for hsName, raw := range hosts {
		cfg, ok := raw.(*model.HSConfig)
		if !ok || cfg == nil {
			continue
		}
		// 普通用户只能看到被分配的主机
		if !uc.IsAdmin && !service.CheckHostAccess(hsName, uc.UserData) {
			continue
		}

		status := "inactive"
		if cfg.EnableHost {
			status = "active"
		}
		vmCount := app.hostVMCount(hsName)

		hostConfig := service.HSConfigToAPI(cfg)
		// 「可使用该主机资源」不等于「可读取宿主 SSH / 路由器口令」
		if !uc.IsAdmin {
			hostConfig = sanitizeHostConfig(hostConfig)
		}

		result[hsName] = map[string]interface{}{
			"name": hsName,
			"type": cfg.ServerType,
			"addr": cfg.ServerAddr,
			// 财务系统插件兼容字段
			"server_name": hsName,
			"server_type": cfg.ServerType,
			"server_addr": cfg.ServerAddr,
			"server_area": cfg.ServerArea,
			"status":      status,
			"config":      hostConfig,
			"vm_count":    vmCount,
			"vms_count":   vmCount,
			"enable_host": cfg.EnableHost,
		}
	}

	utils.Success(c, result)
}

// hostVMUUIDs 返回主机下的虚拟机 UUID 列表（优先内存态，回退数据库）
func (app *App) hostVMUUIDs(hsName string) []string {
	uuids := []string{}
	if d := app.HostSvc.GetDriver(hsName); d != nil {
		for uuid := range d.GetVMSaving() {
			uuids = append(uuids, uuid)
		}
	}
	if len(uuids) == 0 {
		if vms, err := app.DB.GetVMSaving(hsName); err == nil {
			for uuid := range vms {
				uuids = append(uuids, uuid)
			}
		}
	}
	sort.Strings(uuids)
	return uuids
}

// hostVMCount 统计主机下的虚拟机数量
func (app *App) hostVMCount(hsName string) int {
	return len(app.hostVMUUIDs(hsName))
}

// handleHostDetail GET /api/server/detail/:hs_name
// 返回结构与 Python get_host 一致：
//
//	{ name, type, addr, config, vm_count, vm_list, last_updated, status }
//
// 前端编辑主机时读取 data.type 与 data.config 回填表单，缺一不可；
// 这里额外附带引擎元信息（ban_init/ban_edit/messages/tab_lock）作为冗余保护。
func (app *App) handleHostDetail(c *gin.Context) {
	hsName := c.Param("hs_name")
	cfg, err := app.HostSvc.GetHost(hsName)
	if err != nil || cfg == nil {
		utils.NotFound(c, "主机不存在")
		return
	}

	vmList := app.hostVMUUIDs(hsName)
	meta, _ := driver.EngineMetaOf(cfg.ServerType)

	utils.Success(c, map[string]interface{}{
		"name":         hsName,
		"type":         cfg.ServerType,
		"addr":         cfg.ServerAddr,
		"config":       service.HSConfigToAPI(cfg),
		"vm_count":     len(vmList),
		"vm_list":      vmList,
		"last_updated": 0,
		"status":       nil,
		"status_note":  "Use ?status=true to get detailed host status",
		"server_type":  cfg.ServerType,
		"ban_init":     meta.BanInit,
		"ban_edit":     meta.BanEdit,
		"messages":     meta.Messages,
		"tab_lock":     meta.TabLock,
	})
}

// handleHostCreate POST /api/server/create
// 请求体与 Python add_host 一致（前端实际发送的格式）：
//
//	{ "name": "x99pve", "type": "PromoxSetup", "config": { ...HSConfig 字段... }, "server_pass": "..." }
//
// 同时兼容扁平式请求体（server_name / server_type / ... 直接平铺）。
func (app *App) handleHostCreate(c *gin.Context) {
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}

	configRaw, hasConfig := req["config"].(map[string]interface{})
	if !hasConfig {
		configRaw = req
	}

	// 名称与类型：优先顶层 name/type，其次配置内的 server_name/server_type
	serverName := configFieldString(req, "name")
	serverType := configFieldString(req, "type")
	if v := configFieldString(configRaw, "server_name"); v != "" {
		serverName = v
	}
	if v := configFieldString(configRaw, "server_type"); v != "" {
		serverType = v
	}

	if serverName == "" || serverType == "" {
		utils.BadRequest(c, "主机名称和类型不能为空")
		return
	}

	// 检查主机名是否已存在
	existing, _ := app.HostSvc.GetHost(serverName)
	if existing != nil {
		utils.BadRequest(c, "主机名称已存在")
		return
	}

	// 以 config 为准构造完整配置（字段名与 Python HSConfig 完全一致）
	cfg, err := buildHSConfig(configRaw)
	if err != nil {
		utils.BadRequest(c, "配置格式错误: "+err.Error())
		return
	}
	cfg.ServerName = serverName
	cfg.ServerType = serverType
	// 与 Python 一致：前端未显式提交 enable_host 时，新建主机默认启用
	if _, ok := configRaw["enable_host"]; !ok {
		cfg.EnableHost = true
	}
	// 顶层 server_pass 兜底（前端会同时提交顶层与 config 内两份）
	if cfg.ServerPass == "" {
		cfg.ServerPass = configFieldString(req, "server_pass")
	}
	if cfg.ServerPort == 0 {
		cfg.ServerPort = 22
	}

	start := time.Now()
	if err := app.HostSvc.CreateHost(cfg); err != nil {
		app.logEvent(c, "host", "创建主机").Host(serverName).
			Detail("server_type", serverType).Err(err).
			Errorf("创建主机失败：%v", err)
		utils.InternalError(c, "创建主机失败: "+err.Error())
		return
	}

	// 自动将新主机添加到所有管理员的assigned_hosts
	allUsers, err := app.DB.GetAllUsers()
	if err == nil {
		for _, u := range allUsers {
			if !isAdminUser(u) {
				continue
			}
			hosts := toStringSlice(u["assigned_hosts"])
			found := false
			for _, h := range hosts {
				if h == serverName {
					found = true
					break
				}
			}
			if found {
				continue
			}
			hosts = append(hosts, serverName)
			hostsJSON, _ := json.Marshal(hosts)
			if err := app.DB.UpdateUser(toIntVal(u["id"]), map[string]interface{}{"assigned_hosts": string(hostsJSON)}); err != nil {
				app.Logger.Event("host", "分配主机").Host(serverName).
					Err(err).Warn("分配主机到管理员失败: %v", err)
			}
		}
	}

	app.logEvent(c, "host", "创建主机").Host(serverName).
		Detail("server_type", serverType).
		Detail("server_addr", cfg.ServerAddr).
		Detail("server_port", cfg.ServerPort).
		Detail("enable", cfg.EnableHost).
		Duration(time.Since(start)).
		Info("创建主机成功：%s", serverName)
	utils.SuccessMsg(c, "主机创建成功", nil)
}

// handleHostUpdate PUT /api/server/update/:hs_name
// 请求体与 Python update_host 一致：{ "config": { ...HSConfig 字段... } }
// 未提交的字段沿用旧值；敏感字段（server_pass / system_maps / server_plan 等）
// 提交为空容器时同样保留旧值，避免前端未加载完整配置时把数据清空。
func (app *App) handleHostUpdate(c *gin.Context) {
	hsName := c.Param("hs_name")
	cfg, err := app.HostSvc.GetHost(hsName)
	if err != nil || cfg == nil {
		utils.NotFound(c, "主机不存在")
		return
	}

	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}

	configRaw, hasConfig := req["config"].(map[string]interface{})
	if !hasConfig {
		configRaw = req
	}
	if len(configRaw) == 0 {
		utils.BadRequest(c, "配置不能为空")
		return
	}

	// 用旧配置为基底做合并
	oldData, err := json.Marshal(cfg)
	if err != nil {
		utils.InternalError(c, "序列化主机配置失败")
		return
	}
	merged := make(map[string]interface{})
	_ = json.Unmarshal(oldData, &merged)

	for k, oldVal := range merged {
		newVal, exists := configRaw[k]
		if !exists {
			configRaw[k] = oldVal
			continue
		}
		// 敏感字段：前端提交了但为空（None/空串/空容器/0），视为未修改
		if protectedEmptyKeys[k] && isEmptyConfigValue(newVal) && !isEmptyConfigValue(oldVal) {
			configRaw[k] = oldVal
		}
	}

	// 主机名与类型不允许通过本接口变更
	configRaw["server_name"] = hsName
	configRaw["server_type"] = cfg.ServerType

	updated, err := buildHSConfig(configRaw)
	if err != nil {
		utils.BadRequest(c, "配置格式错误: "+err.Error())
		return
	}

	if err := app.HostSvc.UpdateHost(updated); err != nil {
		app.logEvent(c, "host", "更新主机").Host(hsName).Err(err).
			Errorf("更新主机失败：%v", err)
		utils.InternalError(c, "更新主机失败")
		return
	}
	app.logEvent(c, "host", "更新主机").Host(hsName).
		Detail("changed_fields", sortedKeys(configRaw)).
		Info("更新主机配置成功：%s", hsName)
	utils.SuccessMsg(c, "主机更新成功", nil)
}

// handleHostDelete DELETE /api/server/delete/:hs_name
func (app *App) handleHostDelete(c *gin.Context) {
	hsName := c.Param("hs_name")
	if err := app.HostSvc.DeleteHost(hsName); err != nil {
		app.logEvent(c, "host", "删除主机").Host(hsName).Err(err).
			Errorf("删除主机失败：%v", err)
		utils.InternalError(c, "删除主机失败")
		return
	}
	// 删除主机属于高影响操作：WARNING 级别确保在终端与数据库都留痕（结果仍为成功）
	app.logEvent(c, "host", "删除主机").Host(hsName).
		Emit(logger.LevelWarn, "已删除主机及其纳管记录：%s", hsName)
	utils.SuccessMsg(c, "主机删除成功", nil)
}

// handleHostPowers POST /api/server/powers/:hs_name
//
// 与 Python host_enable 一致：读取 enable 字段，**缺省为 true**。
// 此前用 bool 直接绑定，缺省即 false —— 前端若发来不认识的字段
// （例如 apis.ts hostPower 发送的 {action}），会被静默解释为「禁用主机」。
func (app *App) handleHostPowers(c *gin.Context) {
	hsName := c.Param("hs_name")
	var req struct {
		Enable *bool `json:"enable"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	enable := true
	if req.Enable != nil {
		enable = *req.Enable
	}
	if err := app.HostSvc.SetHostEnable(hsName, enable); err != nil {
		app.logEvent(c, "host", "主机启停").Host(hsName).
			Detail("enable", enable).Err(err).
			Errorf("设置主机启停状态失败：%v", err)
		utils.InternalError(c, "设置主机状态失败")
		return
	}
	status := "已禁用"
	if enable {
		status = "已启用"
	}
	app.logEvent(c, "host", "主机启停").Host(hsName).
		Detail("enable", enable).
		Info("主机 %s %s", hsName, status)
	utils.SuccessMsg(c, "主机"+status, nil)
}

// handleHostStatus GET /api/server/status/:hs_name
//
// 返回 {status:{...}, source, cached_at, age_seconds}。
// 前端读取的是 data.status（HostManage.tsx），因此必须包一层；
// 原实现直接返回 HWStatus，导致前端始终拿不到状态数据。
// 支持 ?refresh=true 强制刷新（与 Python 版一致）。
func (app *App) handleHostStatus(c *gin.Context) {
	hsName := c.Param("hs_name")
	refresh := strings.EqualFold(c.Query("refresh"), "true")

	payload, err := app.HostSvc.GetHostStatusPayload(hsName, refresh)
	if err != nil {
		utils.NotFound(c, err.Error())
		return
	}
	utils.Success(c, payload)
}

// handleHostPlanGet GET /api/server/plan/:hs_name
func (app *App) handleHostPlanGet(c *gin.Context) {
	hsName := c.Param("hs_name")
	plans, err := app.HostSvc.GetHostPlans(hsName)
	if err != nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	utils.Success(c, plans)
}

// handleHostPlanSet POST /api/server/plan/:hs_name
func (app *App) handleHostPlanSet(c *gin.Context) {
	hsName := c.Param("hs_name")
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	planName, _ := req["plan_name"].(string)
	if planName == "" {
		utils.BadRequest(c, "套餐名称不能为空")
		return
	}
	planCfg := model.NewVMConfig()
	if data, err := json.Marshal(req); err == nil {
		_ = json.Unmarshal(data, planCfg)
	}
	if err := app.HostSvc.SetHostPlan(hsName, planName, planCfg); err != nil {
		utils.InternalError(c, "设置套餐失败")
		return
	}
	utils.SuccessMsg(c, "套餐设置成功", nil)
}

// handleHostPlanDelete DELETE /api/server/plan/:hs_name/:plan_name
func (app *App) handleHostPlanDelete(c *gin.Context) {
	hsName := c.Param("hs_name")
	planName := c.Param("plan_name")
	if err := app.HostSvc.DeleteHostPlan(hsName, planName); err != nil {
		utils.InternalError(c, "删除套餐失败")
		return
	}
	utils.SuccessMsg(c, "套餐删除成功", nil)
}

// handleHostAreas GET /api/server/areas
func (app *App) handleHostAreas(c *gin.Context) {
	areas, err := app.HostSvc.GetHostAreas()
	if err != nil {
		utils.InternalError(c, "获取区域列表失败")
		return
	}
	if areas == nil {
		areas = []map[string]string{}
	}
	utils.Success(c, areas)
}

// handleHostPlans GET /api/server/plans/:hs_name
func (app *App) handleHostPlans(c *gin.Context) {
	hsName := c.Param("hs_name")
	plans, err := app.HostSvc.GetHostPlans(hsName)
	if err != nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	utils.Success(c, plans)
}

// handleHostPorts GET /api/server/ports/:hs_name
func (app *App) handleHostPorts(c *gin.Context) {
	hsName := c.Param("hs_name")
	ports, err := app.HostSvc.GetAvailablePorts(hsName)
	if err != nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	utils.Success(c, ports)
}

// handleHostBackupScan POST /api/server/backup/scan/:hs_name
func (app *App) handleHostBackupScan(c *gin.Context) {
	hsName := c.Param("hs_name")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在或未启用")
		return
	}
	result, err := d.ScanBackups()
	if err != nil {
		utils.InternalError(c, "扫描备份失败")
		return
	}
	utils.Success(c, result)
}

// ============================================================
// 辅助函数
// ============================================================

// protectedEmptyKeys 主机更新时「提交空值视为未修改」的敏感字段
// 与 Python update_host 的 protected_empty_keys 一致
var protectedEmptyKeys = map[string]bool{
	"system_maps": true,
	"images_maps": true,
	"ipaddr_maps": true,
	"ipaddr_ddns": true,
	"public_addr": true,
	"extend_data": true,
	"server_plan": true,
	"server_pass": true,
	"i_kuai_pass": true,
}

// configFieldString 从请求体中安全读取字符串字段
func configFieldString(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// buildHSConfig 由前端提交的 config 字典构造主机配置
// 字段名与 Python HSConfig 完全一致；
// system_maps / images_maps 前端以「名称 -> 值」映射提交，需先归一化为 []OSConfig
func buildHSConfig(raw map[string]interface{}) (*model.HSConfig, error) {
	normalized := make(map[string]interface{}, len(raw))
	for k, v := range raw {
		normalized[k] = v
	}
	service.NormalizeOSMaps(normalized)
	data, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	return model.HSConfigFromJSON(data)
}

// isAdminUser 判断用户map是否为管理员（兼容 bool / 数字两种存储形态）
func isAdminUser(u map[string]interface{}) bool {
	if v, ok := u["is_admin"].(bool); ok {
		return v
	}
	return toIntVal(u["is_admin"]) == 1
}

// toStringSlice 将 JSON 字符串 / []interface{} / []string 归一化为字符串切片
func toStringSlice(v interface{}) []string {
	switch val := v.(type) {
	case []string:
		return append([]string{}, val...)
	case []interface{}:
		out := make([]string, 0, len(val))
		for _, it := range val {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		var out []string
		if val != "" {
			_ = json.Unmarshal([]byte(val), &out)
		}
		if out == nil {
			out = []string{}
		}
		return out
	default:
		return []string{}
	}
}

// isEmptyConfigValue 判断配置值是否为空（对应 Python 的 (None, ”, {}, [], 0) 判定）
func isEmptyConfigValue(v interface{}) bool {
	switch val := v.(type) {
	case nil:
		return true
	case string:
		return val == ""
	case bool:
		return !val
	case float64:
		return val == 0
	case []interface{}:
		return len(val) == 0
	case map[string]interface{}:
		return len(val) == 0
	default:
		return false
	}
}

func toIntVal(v interface{}) int {
	switch val := v.(type) {
	case int:
		return val
	case int64:
		return int(val)
	case float64:
		return int(val)
	case string:
		n, _ := strconv.Atoi(val)
		return n
	}
	return 0
}

// 确保strconv包被使用
var _ = strconv.Itoa
