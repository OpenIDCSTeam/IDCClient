// Package api 系统管理和主机管理处理器
package api

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/auth"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/service"
	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
)

// ============================================================
// 系统管理 API
// ============================================================

// handleSystemEngine GET /api/system/engine
func (app *App) handleSystemEngine(c *gin.Context) {
	// 前端期望格式：{ engine_types: { "PromoxSetup": "Proxmox VE", ... } }
	engineTypes := map[string]string{
		"PromoxSetup": "Proxmox VE",
		"vSphereESXi": "VMware vSphere/ESXi",
		"LxContainer": "LXC/LXD 容器",
		"OCInterface": "Docker/OCI 容器",
		"HyperVSetup": "Microsoft Hyper-V",
		"QEMUServer":  "QEMU/KVM",
		"VMWareSetup": "VMware Workstation",
		"VirtualBoxs": "VirtualBox",
		"MemuAndroid": "MEmu Android",
		"QingzhouYun": "青州云",
		"SmolVM":      "SmolVM MicroVM Runtime",
	}
	utils.Success(c, map[string]interface{}{
		"engine_types": engineTypes,
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
func (app *App) handleLoggerDetail(c *gin.Context) {
	hsName := c.Query("hs_name")
	level := c.Query("level")
	limit := 100
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil {
			limit = n
		}
	}
	logs, err := app.Logger.GetLogs(hsName, level, limit)
	if err != nil {
		utils.InternalError(c, "获取日志失败")
		return
	}
	if logs == nil {
		logs = []map[string]interface{}{}
	}
	utils.Success(c, logs)
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
		TestEmail   string `json:"test_email"`
		Subject     string `json:"subject"`
		Body        string `json:"body"`
		ResendEmail string `json:"resend_email"`
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
func (app *App) handleRecalculateQuotas(c *gin.Context) {
	// 重新计算所有用户的资源使用量
	users, err := app.DB.GetAllUsers()
	if err != nil {
		utils.InternalError(c, "获取用户列表失败")
		return
	}

	for _, user := range users {
		username := model.SafeGetString(user, "username")
		if username == "" {
			continue
		}
		// 统计该用户的资源使用量
		usedCPU, usedRAM, usedSSD, usedGPU := 0, 0, 0, 0
		usedNatPorts, usedWebProxy := 0, 0

		hosts, _ := app.DB.GetAllHosts()
		for _, h := range hosts {
			vms, _ := app.DB.GetVMSaving(h.HsName)
			for _, vmJSON := range vms {
				var vmCfg model.VMConfig
				if err := json.Unmarshal([]byte(vmJSON), &vmCfg); err != nil {
					continue
				}
				// 检查是否是该用户的主VM
				owners := vmCfg.OwnAll
				if len(owners) == 0 {
					continue
				}
				// 获取第一个所有者（主所有者）
				var primaryOwner string
				for k := range owners {
					primaryOwner = k
					break
				}
				if primaryOwner != username {
					continue
				}
				usedCPU += vmCfg.CpuNum
				usedRAM += vmCfg.MemNum / 1024 // MB -> GB
				usedSSD += vmCfg.HddNum / 1024
				usedGPU += vmCfg.GpuMem / 1024
				usedNatPorts += len(vmCfg.NatAll)
				usedWebProxy += len(vmCfg.WebAll)
			}
		}

		id := model.SafeGetInt(user, "id")
		_ = app.DB.UpdateUser(id, map[string]interface{}{
			"used_cpu":       usedCPU,
			"used_ram":       usedRAM,
			"used_ssd":       usedSSD,
			"used_gpu":       usedGPU,
			"used_nat_ports": usedNatPorts,
			"used_web_proxy": usedWebProxy,
		})
	}
	utils.SuccessMsg(c, "配额重新计算完成", nil)
}

// ============================================================
// 主机管理 API
// ============================================================

// handleHostList GET /api/server/detail
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

	// 普通用户只能看到被分配的主机
	if !uc.IsAdmin {
		filtered := make(map[string]interface{})
		for hsName, cfg := range hosts {
			if service.CheckHostAccess(hsName, uc.UserData) {
				filtered[hsName] = cfg
			}
		}
		utils.Success(c, filtered)
		return
	}
	utils.Success(c, hosts)
}

// handleHostDetail GET /api/server/detail/:hs_name
func (app *App) handleHostDetail(c *gin.Context) {
	hsName := c.Param("hs_name")
	cfg, err := app.HostSvc.GetHost(hsName)
	if err != nil || cfg == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	utils.Success(c, cfg)
}

// handleHostCreate POST /api/server/create
func (app *App) handleHostCreate(c *gin.Context) {
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}

	serverName, _ := req["server_name"].(string)
	serverType, _ := req["server_type"].(string)
	serverAddr, _ := req["server_addr"].(string)

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

	cfg := model.NewHSConfig()
	cfg.ServerName = serverName
	cfg.ServerType = serverType
	cfg.ServerAddr = serverAddr
	cfg.ServerUser, _ = req["server_user"].(string)
	cfg.ServerPass, _ = req["server_pass"].(string)
	if port, ok := req["server_port"]; ok {
		cfg.ServerPort = toIntVal(port)
	}
	cfg.SystemPath, _ = req["system_path"].(string)
	cfg.ImagesPath, _ = req["images_path"].(string)
	cfg.BackupPath, _ = req["backup_path"].(string)
	cfg.NetworkNat, _ = req["network_nat"].(string)
	cfg.NetworkPub, _ = req["network_pub"].(string)
	cfg.PortsStart = toIntVal(req["ports_start"])
	cfg.PortsClose = toIntVal(req["ports_close"])
	cfg.EnableHost = true

	if err := app.HostSvc.CreateHost(cfg); err != nil {
		utils.InternalError(c, "创建主机失败: "+err.Error())
		return
	}

	// 自动将新主机添加到所有管理员的assigned_hosts
	allUsers, err := app.DB.GetAllUsers()
	if err == nil {
		for _, u := range allUsers {
			isAdmin, _ := u["is_admin"].(bool)
			if !isAdmin {
				if adminInt, ok := u["is_admin"].(int64); ok && adminInt == 1 {
					isAdmin = true
				}
			}
			if isAdmin {
				hosts := []string{}
				if h, ok := u["assigned_hosts"].([]string); ok {
					hosts = h
				} else if h, ok := u["assigned_hosts"].(string); ok && h != "" {
					_ = json.Unmarshal([]byte(h), &hosts)
				}
				found := false
				for _, h := range hosts {
					if h == serverName {
						found = true
						break
					}
				}
				if !found {
					hosts = append(hosts, serverName)
					hostsJSON, _ := json.Marshal(hosts)
					if uid, ok := u["id"].(int64); ok {
						app.DB.UpdateUser(int(uid), map[string]interface{}{"assigned_hosts": string(hostsJSON)})
					}
				}
			}
		}
	}

	utils.SuccessMsg(c, "主机创建成功", nil)
}

// handleHostUpdate PUT /api/server/update/:hs_name
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

	// 更新字段
	if v, ok := req["server_addr"].(string); ok {
		cfg.ServerAddr = v
	}
	if v, ok := req["server_user"].(string); ok {
		cfg.ServerUser = v
	}
	if v, ok := req["server_pass"].(string); ok {
		cfg.ServerPass = v
	}
	if v, ok := req["server_port"]; ok {
		cfg.ServerPort = toIntVal(v)
	}
	if v, ok := req["system_path"].(string); ok {
		cfg.SystemPath = v
	}
	if v, ok := req["images_path"].(string); ok {
		cfg.ImagesPath = v
	}
	if v, ok := req["backup_path"].(string); ok {
		cfg.BackupPath = v
	}
	if v, ok := req["network_nat"].(string); ok {
		cfg.NetworkNat = v
	}
	if v, ok := req["network_pub"].(string); ok {
		cfg.NetworkPub = v
	}
	if v, ok := req["ports_start"]; ok {
		cfg.PortsStart = toIntVal(v)
	}
	if v, ok := req["ports_close"]; ok {
		cfg.PortsClose = toIntVal(v)
	}

	if err := app.HostSvc.UpdateHost(cfg); err != nil {
		utils.InternalError(c, "更新主机失败")
		return
	}
	utils.SuccessMsg(c, "主机更新成功", nil)
}

// handleHostDelete DELETE /api/server/delete/:hs_name
func (app *App) handleHostDelete(c *gin.Context) {
	hsName := c.Param("hs_name")
	if err := app.HostSvc.DeleteHost(hsName); err != nil {
		utils.InternalError(c, "删除主机失败")
		return
	}
	utils.SuccessMsg(c, "主机删除成功", nil)
}

// handleHostPowers POST /api/server/powers/:hs_name
func (app *App) handleHostPowers(c *gin.Context) {
	hsName := c.Param("hs_name")
	var req struct {
		Enable bool `json:"enable"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	if err := app.HostSvc.SetHostEnable(hsName, req.Enable); err != nil {
		utils.InternalError(c, "设置主机状态失败")
		return
	}
	status := "已禁用"
	if req.Enable {
		status = "已启用"
	}
	utils.SuccessMsg(c, "主机"+status, nil)
}

// handleHostStatus GET /api/server/status/:hs_name
func (app *App) handleHostStatus(c *gin.Context) {
	hsName := c.Param("hs_name")
	status, err := app.HostSvc.GetHostStatus(hsName)
	if err != nil {
		// 从数据库获取最近状态
		statusList, dbErr := app.DB.GetHSStatus(hsName, 1)
		if dbErr != nil || len(statusList) == 0 {
			utils.Success(c, map[string]interface{}{})
			return
		}
		var statusData interface{}
		_ = json.Unmarshal([]byte(statusList[0]), &statusData)
		utils.Success(c, statusData)
		return
	}
	utils.Success(c, status)
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
