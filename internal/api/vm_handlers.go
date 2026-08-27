// Package api 虚拟机管理、网络、存储、备份、设备直通处理器
package api

import (
	"encoding/json"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/auth"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/service"
	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
)

// ============================================================
// 虚拟机管理 API
// ============================================================

// handleVMList GET /api/client/detail/:hs_name
func (app *App) handleVMList(c *gin.Context) {
	hsName := c.Param("hs_name")
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}
	if !uc.IsAdmin && !service.CheckHostAccess(hsName, uc.UserData) {
		utils.Forbidden(c)
		return
	}

	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		// 从数据库获取
		vmsJSON, err := app.DB.GetVMSaving(hsName)
		if err != nil {
			utils.InternalError(c, "获取虚拟机列表失败")
			return
		}
		result := make(map[string]interface{})
		for uuid, vmJSON := range vmsJSON {
			var vm model.VMConfig
			if err := json.Unmarshal([]byte(vmJSON), &vm); err == nil {
				result[uuid] = vm
			}
		}
		utils.Success(c, result)
		return
	}

	vms, err := d.GetVMList()
	if err != nil {
		utils.InternalError(c, "获取虚拟机列表失败")
		return
	}
	utils.Success(c, vms)
}

// handleVMDetail GET /api/client/detail/:hs_name/:vm_uuid
func (app *App) handleVMDetail(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}

	// 检查主机访问权限
	username := c.GetString("username")
	if !app.HostSvc.CheckHostAccess(username, hsName) {
		utils.Forbidden(c)
		return
	}

	// 检查虚拟机所有权（非管理员需要检查）
	if !uc.IsAdmin && !app.HostSvc.CheckVMOwnership(username, hsName, vmUUID) {
		utils.Fail(c, 403, "无权访问此虚拟机")
		return
	}

	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		vmJSON, err := app.DB.GetVMConfig(hsName, vmUUID)
		if err != nil || vmJSON == "" {
			utils.NotFound(c, "虚拟机不存在")
			return
		}
		var vm model.VMConfig
		if err := json.Unmarshal([]byte(vmJSON), &vm); err != nil {
			utils.InternalError(c, "解析虚拟机配置失败")
			return
		}
		utils.Success(c, vm)
		return
	}

	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	utils.Success(c, vm)
}

// handleVMCreate POST /api/client/create/:hs_name
func (app *App) handleVMCreate(c *gin.Context) {
	hsName := c.Param("hs_name")
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}

	// 检查主机访问权限
	username := c.GetString("username")
	if !app.HostSvc.CheckHostAccess(username, hsName) {
		utils.Forbidden(c)
		return
	}

	// 权限检查
	if ok, msg := service.CheckVMPermission("create", uc.UserData); !ok {
		utils.Fail(c, 403, msg)
		return
	}

	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}

	vmUUID, _ := req["vm_uuid"].(string)
	if vmUUID == "" {
		utils.BadRequest(c, "虚拟机名称不能为空")
		return
	}

	cfg := model.NewVMConfig()
	cfg.VmUUID = vmUUID
	cfg.VmFlag = model.VMPowerUnknown
	if v, ok := req["vm_name"].(string); ok {
		_ = v // vm_name作为显示名称，存储在vm_uuid中
	}
	if v, ok := req["os_name"].(string); ok {
		cfg.OsName = v
	}
	if v, ok := req["cpu_num"]; ok {
		cfg.CpuNum = toIntVal(v)
	}
	if v, ok := req["mem_num"]; ok {
		cfg.MemNum = toIntVal(v)
	}
	if v, ok := req["hdd_num"]; ok {
		cfg.HddNum = toIntVal(v)
	}
	if v, ok := req["gpu_num"]; ok {
		cfg.GpuMem = toIntVal(v)
	}
	if v, ok := req["speed_u"]; ok {
		cfg.SpeedU = toIntVal(v)
	}
	if v, ok := req["speed_d"]; ok {
		cfg.SpeedD = toIntVal(v)
	}

	// 补全网卡、NAT、反向代理等字段（从请求中解析）
	if v, ok := req["nic_all"].(map[string]interface{}); ok {
		cfg.NicAll = parseNicAll(v)
	}
	if v, ok := req["nat_all"].(map[string]interface{}); ok {
		// NatAll 是 []PortData 类型，需要转换
		natMap := parseNatAll(v)
		var natList []model.PortData
		for _, pd := range natMap {
			natList = append(natList, pd)
		}
		cfg.NatAll = natList
	}
	if v, ok := req["web_all"].(map[string]interface{}); ok {
		cfg.WebAll = parseWebAll(v)
	}

	// 设置所有者
	cfg.OwnAll = map[string]model.UserMask{
		uc.Username: model.FullUserMask(),
	}

	// 创建虚拟用户（用于财务系统临时凭据访问）
	// 虚拟用户名格式：{hs_name}-{vm_uuid}
	virtualUser := hsName + "-" + vmUUID
	cfg.OwnAll[virtualUser] = model.VirtualUserMask()

	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在或未启用")
		return
	}

	result, err := d.CreateVM(cfg)
	if err != nil {
		utils.InternalError(c, "创建虚拟机失败: "+err.Error())
		return
	}
	if !result.Success {
		utils.Fail(c, 500, result.Message)
		return
	}

	// 保存到数据库
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, cfg)
	utils.SuccessMsg(c, "虚拟机创建成功", nil)
}

// handleVMReinstall POST /api/client/reinstall/:hs_name/:vm_uuid
func (app *App) handleVMReinstall(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}

	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在或未启用")
		return
	}

	// 获取当前VM配置
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}

	result, err := d.ReinstallVM(vm)
	if err != nil {
		utils.InternalError(c, "重装系统失败: "+err.Error())
		return
	}
	if !result.Success {
		utils.Fail(c, 500, result.Message)
		return
	}

	utils.SuccessMsg(c, "系统重装成功", nil)
}

// handleVMUpdate PUT /api/client/update/:hs_name/:vm_uuid
func (app *App) handleVMUpdate(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}

	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在或未启用")
		return
	}

	oldVM, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}

	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}

	// 更新配置
	newVM := *oldVM
	if v, ok := req["cpu_num"]; ok {
		newVM.CpuNum = toIntVal(v)
	}
	if v, ok := req["mem_num"]; ok {
		newVM.MemNum = toIntVal(v)
	}
	if v, ok := req["hdd_num"]; ok {
		newVM.HddNum = toIntVal(v)
	}
	if v, ok := req["speed_u"]; ok {
		newVM.SpeedU = toIntVal(v)
	}
	if v, ok := req["speed_d"]; ok {
		newVM.SpeedD = toIntVal(v)
	}
	if v, ok := req["os_name"].(string); ok {
		newVM.OsName = v
	}

	result, err := d.UpdateVM(&newVM, oldVM)
	if err != nil {
		utils.InternalError(c, "更新虚拟机失败")
		return
	}
	if !result.Success {
		utils.Fail(c, 500, result.Message)
		return
	}

	_ = app.DB.UpsertVMConfig(hsName, vmUUID, &newVM)
	utils.SuccessMsg(c, "虚拟机更新成功", nil)
}

// handleVMDelete DELETE /api/client/delete/:hs_name/:vm_uuid
func (app *App) handleVMDelete(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}

	if ok, _ := service.CheckVMPermission("delete", uc.UserData); !ok {
		utils.Forbidden(c)
		return
	}

	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在或未启用")
		return
	}

	result, err := d.DeleteVM(vmUUID, true)
	if err != nil {
		utils.InternalError(c, "删除虚拟机失败")
		return
	}
	if !result.Success {
		utils.Fail(c, 500, result.Message)
		return
	}

	_ = app.DB.DeleteVMConfig(hsName, vmUUID)
	utils.SuccessMsg(c, "虚拟机删除成功", nil)
}

// handleVMPowers POST /api/client/powers/:hs_name/:vm_uuid
func (app *App) handleVMPowers(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")

	var req struct {
		Action string `json:"action"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}

	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在或未启用")
		return
	}

	action := model.VMPowers(req.Action)
	result, err := d.PowerControl(vmUUID, action)
	if err != nil {
		utils.InternalError(c, "电源操作失败")
		return
	}
	if !result.Success {
		utils.Fail(c, 500, result.Message)
		return
	}

	// 更新数据库中的VM状态
	vm, _ := d.GetVMDetail(vmUUID)
	if vm != nil {
		_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	}
	utils.SuccessMsg(c, "电源操作已执行", nil)
}

// handleVMRemote GET /api/client/remote/:hs_name/:vm_uuid
func (app *App) handleVMRemote(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")

	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在或未启用")
		return
	}

	ipAddr := c.ClientIP()
	result, err := d.GetVNCInfo(vmUUID, ipAddr)
	if err != nil {
		utils.InternalError(c, "获取控制台信息失败")
		return
	}
	if !result.Success {
		utils.Fail(c, 500, result.Message)
		return
	}
	utils.Success(c, result.Results)
}

// handleVMScreenshot GET /api/client/screenshot/:hs_name/:vm_uuid
func (app *App) handleVMScreenshot(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")

	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在或未启用")
		return
	}

	screenshot, err := d.GetScreenshot(vmUUID)
	if err != nil {
		utils.InternalError(c, "获取截图失败")
		return
	}
	utils.Success(c, gin.H{"screenshot": screenshot})
}

// handleVMPassword POST /api/client/password/:hs_name/:vm_uuid
func (app *App) handleVMPassword(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")

	var req struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Password == "" {
		utils.BadRequest(c, "密码不能为空")
		return
	}

	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在或未启用")
		return
	}

	result, err := d.SetPassword(vmUUID, req.Password)
	if err != nil {
		utils.InternalError(c, "设置密码失败")
		return
	}
	if !result.Success {
		utils.Fail(c, 500, result.Message)
		return
	}
	utils.SuccessMsg(c, "密码设置成功", nil)
}

// handleVMStatus GET /api/client/status/:hs_name/:vm_uuid
func (app *App) handleVMStatus(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")

	startTS := int64(0)
	endTS := int64(0)
	if s := c.Query("start"); s != "" {
		startTS, _ = strconv.ParseInt(s, 10, 64)
	}
	if e := c.Query("end"); e != "" {
		endTS, _ = strconv.ParseInt(e, 10, 64)
	}

	statusList, err := app.DB.GetVMStatus(hsName, vmUUID, startTS, endTS)
	if err != nil {
		utils.InternalError(c, "获取虚拟机状态失败")
		return
	}

	result := []interface{}{}
	for _, s := range statusList {
		var data interface{}
		if err := json.Unmarshal([]byte(s), &data); err == nil {
			result = append(result, data)
		}
	}
	utils.Success(c, result)
}

// parseNicAll 解析网卡配置（简化实现，返回空map）
func parseNicAll(data map[string]interface{}) map[string]model.NCConfig {
	result := make(map[string]model.NCConfig)
	// 实际项目中需要完整解析，这里简化
	return result
}

// parseNatAll 解析NAT端口转发配置（简化实现，返回空map）
func parseNatAll(data map[string]interface{}) map[string]model.PortData {
	result := make(map[string]model.PortData)
	// 实际项目中需要完整解析，这里简化
	return result
}

// parseWebAll 解析反向代理配置
func parseWebAll(data map[string]interface{}) []model.WebProxy {
	var result []model.WebProxy
	for _, v := range data {
		if webData, ok := v.(map[string]interface{}); ok {
			web := model.WebProxy{}
			if domain, ok := webData["domain"].(string); ok {
				web.WebAddr = domain
			}
			if port, ok := webData["port"]; ok {
				web.LanPort = toIntVal(port)
			}
			if addr, ok := webData["lan_addr"].(string); ok {
				web.LanAddr = addr
			}
			result = append(result, web)
		}
	}
	return result
}

// handleVMScan POST /api/client/scaner/:hs_name
func (app *App) handleVMScan(c *gin.Context) {
	hsName := c.Param("hs_name")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在或未启用")
		return
	}
	result, err := d.ScanVMs()
	if err != nil {
		utils.InternalError(c, "扫描虚拟机失败")
		return
	}
	utils.Success(c, result)
}

// handleVMUpload POST /api/client/upload（公开接口，虚拟机上报状态）
func (app *App) handleVMUpload(c *gin.Context) {
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	hsName, _ := req["hs_name"].(string)
	vmUUID, _ := req["vm_uuid"].(string)
	if hsName == "" || vmUUID == "" {
		utils.BadRequest(c, "hs_name和vm_uuid不能为空")
		return
	}
	acStatus, _ := req["ac_status"].(string)
	now := time.Now().Unix()
	_ = app.DB.AddVMStatus(hsName, vmUUID, req, acStatus, now, 0)
	utils.SuccessMsg(c, "状态已上报", nil)
}

// ============================================================
// NAT端口转发 API
// ============================================================

// handleNATGet GET /api/client/natget/:hs_name/:vm_uuid
func (app *App) handleNATGet(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	utils.Success(c, vm.NatAll)
}

// handleNATAdd POST /api/client/natadd/:hs_name/:vm_uuid
func (app *App) handleNATAdd(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req model.PortData
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	vm.NatAll = append(vm.NatAll, req)
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	utils.SuccessMsg(c, "NAT规则添加成功", nil)
}

// handleNATDelete DELETE /api/client/natdel/:hs_name/:vm_uuid/:rule_index
func (app *App) handleNATDelete(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	ruleIndex, _ := strconv.Atoi(c.Param("rule_index"))

	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	if ruleIndex < 0 || ruleIndex >= len(vm.NatAll) {
		utils.BadRequest(c, "规则索引无效")
		return
	}
	vm.NatAll = append(vm.NatAll[:ruleIndex], vm.NatAll[ruleIndex+1:]...)
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	utils.SuccessMsg(c, "NAT规则删除成功", nil)
}

// ============================================================
// IP地址管理 API
// ============================================================

// handleIPList GET /api/client/ipaddr/detail/:hs_name/:vm_uuid
func (app *App) handleIPList(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	utils.Success(c, vm.NicAll)
}

// handleIPCreate POST /api/client/ipaddr/create/:hs_name/:vm_uuid
func (app *App) handleIPCreate(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req model.NCConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	if req.NicName == "" {
		req.NicName = "nic" + strconv.Itoa(len(vm.NicAll))
	}
	vm.NicAll[req.NicName] = req
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	utils.SuccessMsg(c, "网卡添加成功", nil)
}

// handleIPDelete DELETE /api/client/ipaddr/delete/:hs_name/:vm_uuid/:ip_index
func (app *App) handleIPDelete(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	ipIndex := c.Param("ip_index")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	delete(vm.NicAll, ipIndex)
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	utils.SuccessMsg(c, "网卡删除成功", nil)
}

// handleIPUpdate PUT /api/hosts/:hs_name/vms/:vm_uuid/ip_addresses/:nic_name
func (app *App) handleIPUpdate(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	nicName := c.Param("nic_name")
	var req model.NCConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	req.NicName = nicName
	vm.NicAll[nicName] = req
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	utils.SuccessMsg(c, "网卡更新成功", nil)
}

// handleIPDeleteByName DELETE /api/hosts/:hs_name/vms/:vm_uuid/ip_addresses/:nic_name
func (app *App) handleIPDeleteByName(c *gin.Context) {
	c.Params = append(c.Params, gin.Param{Key: "ip_index", Value: c.Param("nic_name")})
	app.handleIPDelete(c)
}

// ============================================================
// 反向代理 API（用户级）
// ============================================================

// handleProxyUserList GET /api/client/proxys/list
func (app *App) handleProxyUserList(c *gin.Context) {
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}
	result := []interface{}{}
	hosts, _ := app.DB.GetAllHosts()
	for _, h := range hosts {
		if !uc.IsAdmin && !service.CheckHostAccess(h.HsName, uc.UserData) {
			continue
		}
		d := app.HostSvc.GetDriver(h.HsName)
		if d == nil {
			continue
		}
		vms := d.GetVMSaving()
		for vmUUID, vm := range vms {
			for _, proxy := range vm.WebAll {
				result = append(result, map[string]interface{}{
					"hs_name": h.HsName, "vm_uuid": vmUUID, "proxy": proxy,
				})
			}
		}
	}
	utils.Success(c, result)
}

// handleProxyDetail GET /api/client/proxys/detail/:hs_name/:vm_uuid
func (app *App) handleProxyDetail(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	utils.Success(c, vm.WebAll)
}

// handleProxyCreate POST /api/client/proxys/create/:hs_name/:vm_uuid
func (app *App) handleProxyCreate(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req model.WebProxy
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	vm.WebAll = append(vm.WebAll, req)
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	utils.SuccessMsg(c, "代理添加成功", nil)
}

// handleProxyDelete DELETE /api/client/proxys/delete/:hs_name/:vm_uuid/:proxy_index
func (app *App) handleProxyDelete(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	proxyIndex, _ := strconv.Atoi(c.Param("proxy_index"))
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	if proxyIndex < 0 || proxyIndex >= len(vm.WebAll) {
		utils.BadRequest(c, "代理索引无效")
		return
	}
	vm.WebAll = append(vm.WebAll[:proxyIndex], vm.WebAll[proxyIndex+1:]...)
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	utils.SuccessMsg(c, "代理删除成功", nil)
}

// ============================================================
// 反向代理 API（管理员级）
// ============================================================

func (app *App) handleProxyAdminList(c *gin.Context) {
	app.handleProxyUserList(c)
}

func (app *App) handleProxyAdminListByHost(c *gin.Context) {
	app.handleProxyDetail(c)
}

func (app *App) handleProxyAdminDetail(c *gin.Context) {
	app.handleProxyDetail(c)
}

func (app *App) handleProxyAdminCreate(c *gin.Context) {
	app.handleProxyCreate(c)
}

// handleProxyAdminUpdate PUT /api/admin/proxys/update/:hs_name/:vm_uuid/:proxy_index
func (app *App) handleProxyAdminUpdate(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	proxyIndex, _ := strconv.Atoi(c.Param("proxy_index"))
	var req model.WebProxy
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	if proxyIndex < 0 || proxyIndex >= len(vm.WebAll) {
		utils.BadRequest(c, "代理索引无效")
		return
	}
	vm.WebAll[proxyIndex] = req
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	utils.SuccessMsg(c, "代理更新成功", nil)
}

func (app *App) handleProxyAdminDelete(c *gin.Context) {
	app.handleProxyDelete(c)
}

// ============================================================
// 存储管理 API
// ============================================================

func (app *App) handleHDDList(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	utils.Success(c, vm.HddAll)
}

func (app *App) handleHDDMount(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req model.SDConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	result, err := d.MountHDD(vmUUID, &req, true)
	if err != nil {
		utils.InternalError(c, "挂载磁盘失败")
		return
	}
	vm, _ := d.GetVMDetail(vmUUID)
	if vm != nil {
		_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	}
	utils.Success(c, result)
}

func (app *App) handleHDDUnmount(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req model.SDConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	result, err := d.MountHDD(vmUUID, &req, false)
	if err != nil {
		utils.InternalError(c, "卸载磁盘失败")
		return
	}
	vm, _ := d.GetVMDetail(vmUUID)
	if vm != nil {
		_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	}
	utils.Success(c, result)
}

func (app *App) handleHDDTransfer(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req struct {
		HddName  string `json:"hdd_name"`
		TargetVM string `json:"target_vm"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	hdd := &model.SDConfig{HddName: req.HddName}
	result, err := d.TransferHDD(vmUUID, hdd, req.TargetVM)
	if err != nil {
		utils.InternalError(c, "移交磁盘失败")
		return
	}
	utils.Success(c, result)
}

func (app *App) handleHDDDelete(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req struct {
		HddName string `json:"hdd_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	result, err := d.DeleteHDD(vmUUID, req.HddName)
	if err != nil {
		utils.InternalError(c, "删除磁盘失败")
		return
	}
	vm, _ := d.GetVMDetail(vmUUID)
	if vm != nil {
		_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	}
	utils.Success(c, result)
}

func (app *App) handleISOList(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	utils.Success(c, vm.IsoAll)
}

func (app *App) handleISOMount(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req model.IMConfig
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	result, err := d.MountISO(vmUUID, &req, true)
	if err != nil {
		utils.InternalError(c, "挂载ISO失败")
		return
	}
	vm, _ := d.GetVMDetail(vmUUID)
	if vm != nil {
		_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	}
	utils.Success(c, result)
}

func (app *App) handleISOUnmount(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	isoName := c.Param("iso_name")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	iso := &model.IMConfig{IsoName: isoName}
	result, err := d.MountISO(vmUUID, iso, false)
	if err != nil {
		utils.InternalError(c, "卸载ISO失败")
		return
	}
	vm, _ := d.GetVMDetail(vmUUID)
	if vm != nil {
		_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	}
	utils.Success(c, result)
}

func (app *App) handleUSBMount(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req struct {
		UsbKey string `json:"usb_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	result, err := d.MountUSB(vmUUID, req.UsbKey, true)
	if err != nil {
		utils.InternalError(c, "挂载USB失败")
		return
	}
	utils.Success(c, result)
}

func (app *App) handleUSBDelete(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	usbKey := c.Param("usb_key")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	result, err := d.MountUSB(vmUUID, usbKey, false)
	if err != nil {
		utils.InternalError(c, "卸载USB失败")
		return
	}
	utils.Success(c, result)
}

// ============================================================
// 备份管理 API
// ============================================================

func (app *App) handleBackupList(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	utils.Success(c, vm.Backups)
}

func (app *App) handleBackupCreate(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req struct {
		Tips string `json:"tips"`
	}
	_ = c.ShouldBindJSON(&req)
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	result, err := d.CreateBackup(vmUUID, req.Tips)
	if err != nil {
		utils.InternalError(c, "创建备份失败")
		return
	}
	utils.Success(c, result)
}

func (app *App) handleBackupRestore(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req struct {
		BackupName string `json:"backup_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.BackupName == "" {
		utils.BadRequest(c, "备份文件名不能为空")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	result, err := d.RestoreBackup(vmUUID, req.BackupName)
	if err != nil {
		utils.InternalError(c, "还原备份失败")
		return
	}
	utils.Success(c, result)
}

func (app *App) handleBackupDelete(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req struct {
		BackupName string `json:"backup_name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.BackupName == "" {
		utils.BadRequest(c, "备份文件名不能为空")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	result, err := d.DeleteBackup(vmUUID, req.BackupName)
	if err != nil {
		utils.InternalError(c, "删除备份失败")
		return
	}
	utils.Success(c, result)
}

// ============================================================
// 设备直通 API
// ============================================================

func (app *App) handleOSImages(c *gin.Context) {
	hsName := c.Param("hs_name")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	images, err := d.GetOSImages()
	if err != nil {
		utils.InternalError(c, "获取镜像列表失败")
		return
	}
	utils.Success(c, images)
}

func (app *App) handleGPUList(c *gin.Context) {
	hsName := c.Param("hs_name")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	gpus, err := d.GetGPUList()
	if err != nil {
		utils.InternalError(c, "获取GPU列表失败")
		return
	}
	utils.Success(c, gpus)
}

func (app *App) handlePCIList(c *gin.Context) {
	hsName := c.Param("hs_name")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	pcis, err := d.GetPCIList()
	if err != nil {
		utils.InternalError(c, "获取PCI列表失败")
		return
	}
	utils.Success(c, pcis)
}

func (app *App) handlePCISetup(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req struct {
		PciKey  string `json:"pci_key"`
		GpuUUID string `json:"gpu_uuid"`
		GpuMdev string `json:"gpu_mdev"`
		GpuHint string `json:"gpu_hint"`
		Action  string `json:"action"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	cfg := &model.VFConfig{
		PciKey: req.PciKey, GpuUUID: req.GpuUUID,
		GpuMdev: req.GpuMdev, GpuHint: req.GpuHint,
	}
	add := req.Action == "add"
	result, err := d.SetupPCI(vmUUID, cfg, req.PciKey, add)
	if err != nil {
		utils.InternalError(c, "PCI直通操作失败")
		return
	}
	utils.Success(c, result)
}

func (app *App) handleUSBList(c *gin.Context) {
	hsName := c.Param("hs_name")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	usbs, err := d.GetUSBList()
	if err != nil {
		utils.InternalError(c, "获取USB列表失败")
		return
	}
	utils.Success(c, usbs)
}

func (app *App) handleUSBSetup(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req struct {
		UsbKey  string `json:"usb_key"`
		VidUUID string `json:"vid_uuid"`
		PidUUID string `json:"pid_uuid"`
		UsbHint string `json:"usb_hint"`
		Action  string `json:"action"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	info := &model.USBInfos{
		UsbKey: req.UsbKey, VidUUID: req.VidUUID,
		PidUUID: req.PidUUID, UsbHint: req.UsbHint,
	}
	add := req.Action == "add"
	result, err := d.SetupUSB(vmUUID, info, req.UsbKey, add)
	if err != nil {
		utils.InternalError(c, "USB直通操作失败")
		return
	}
	utils.Success(c, result)
}

// ============================================================
// EFI启动项 API
// ============================================================

func (app *App) handleEFIList(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	utils.Success(c, vm.EfiAll)
}

func (app *App) handleEFISetup(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req struct {
		EfiList []model.BootOpts `json:"efi_list"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	vm.EfiAll = req.EfiList
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	utils.SuccessMsg(c, "启动顺序设置成功", nil)
}

// ============================================================
// 虚拟机所有者 API
// ============================================================

func (app *App) handleOwnerList(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	utils.Success(c, vm.OwnAll)
}

func (app *App) handleOwnerAdd(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req struct {
		Username string `json:"username"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Username == "" {
		utils.BadRequest(c, "用户名不能为空")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	vm.OwnAll[req.Username] = model.FullUserMask()
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	utils.SuccessMsg(c, "所有者添加成功", nil)
}

func (app *App) handleOwnerDelete(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req struct {
		Username string `json:"username"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Username == "" {
		utils.BadRequest(c, "用户名不能为空")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	delete(vm.OwnAll, req.Username)
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	utils.SuccessMsg(c, "所有者删除成功", nil)
}

func (app *App) handleOwnerPermission(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req struct {
		Username string          `json:"username"`
		Mask     model.UserMask  `json:"mask"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Username == "" {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	vm.OwnAll[req.Username] = req.Mask
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	utils.SuccessMsg(c, "权限更新成功", nil)
}

func (app *App) handleOwnerTransfer(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req struct {
		NewOwner string `json:"new_owner"`
		KeepOld  bool   `json:"keep_old"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.NewOwner == "" {
		utils.BadRequest(c, "新所有者不能为空")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}

	// 重建所有者map，新所有者排第一
	newOwners := map[string]model.UserMask{
		req.NewOwner: model.FullUserMask(),
	}
	if req.KeepOld {
		for k, v := range vm.OwnAll {
			if k != req.NewOwner {
				newOwners[k] = v
			}
		}
	}
	vm.OwnAll = newOwners
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	utils.SuccessMsg(c, "所有权移交成功", nil)
}

// 确保time包被使用（handleVMUpload中使用了time.Now()）
var _ = time.Now
