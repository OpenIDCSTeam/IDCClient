// Package api 虚拟机管理、网络、存储、备份、设备直通处理器
package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/auth"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/driver"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/logger"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/service"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/task"
	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
)

// ============================================================
// 虚拟机管理 API
// ============================================================

// loadVMConfig 读取虚拟机配置：数据库为权威，驱动内存覆盖运行时状态。
//
// 与 handleVMList 的策略完全一致（"以数据库为基础，再用驱动内存覆盖运行时状态"），
// 也是 Python 各处理器「server.data_get() 后读 server.vm_saving」的等价实现。
//
// 为什么不能直接用 d.GetVMDetail：
//
//	驱动的 VmSaving 只有两种来源 —— 启动时 SetVMSaving 从数据库恢复，
//	或平台扫描刷新。后者的致命问题在于：web_all（反向代理）、own_all（所有者）、
//	backups 等是 IDCClient 自有的业务元数据，平台（PVE/ESXi/云）根本不认识，
//	一旦平台扫描刷新覆盖了 VmSaving，这些字段就会变成空或被截断的旧值。
//	此时：
//	  - 反向代理刚创建成功、数据库已落库，详情接口却只回旧的 1 条；
//	  - 所有者/权限修改后回读不到；
//	  - 平台临时不可达 / 驱动刚创建时，整片子资源接口统一 404
//	    「虚拟机不存在」，而列表页却能正常显示，表现为
//	    「看得见但点进去所有操作都失败」。
//
// 因此这里改为：数据库记录作为业务字段的权威来源，
// 仅用驱动内存覆盖运行时状态（电源状态、软删除标记、平台扩展数据如 vmid）。
func (app *App) loadVMConfig(hsName, vmUUID string) (*model.VMConfig, error) {
	d := app.HostSvc.GetDriver(hsName)

	var live *model.VMConfig
	if d != nil {
		if cfg, err := d.GetVMDetail(vmUUID); err == nil {
			live = cfg
		}
	}

	// 注意：必须直接读数据库，不能用 HostSvc.MustGetVMConfig ——
	// 后者内部同样是「驱动优先」，会让上面的数据库权威策略失效。
	var base *model.VMConfig
	if raw, err := app.DB.GetVMConfig(hsName, vmUUID); err == nil && raw != "" {
		if cfg, decErr := service.DecodeVMConfig(raw); decErr == nil {
			base = cfg
		}
	}

	switch {
	case base != nil:
		// 用驱动侧的运行时字段覆盖数据库记录
		if live != nil {
			base.VmFlag = live.VmFlag
			base.VmDeleted = live.VmDeleted
			if live.ExtData != nil {
				base.ExtData = live.ExtData
			}
		}
		return base, nil
	case live != nil:
		// 数据库尚无记录（平台新发现、尚未落库）：退回驱动内存
		return live, nil
	default:
		return nil, fmt.Errorf("虚拟机不存在: %s/%s", hsName, vmUUID)
	}
}

// handleVMList GET /api/client/detail/:hs_name
//
// 返回结构与 Python 版 get_vms 一致：{uuid: {uuid, config, status, user_permissions}}。
// 前端通过 vm.config.own_all 判断所有者、通过 vm.status[0] 展示监控数据，
// 因此不能再返回扁平的 VMConfig。
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

	// 禁用主机不返回虚拟机列表（与 Python 版一致）
	if row, err := app.DB.GetHost(hsName); err != nil || row == nil || row.EnableHost == 0 {
		utils.Success(c, map[string]interface{}{})
		return
	}

	// 以数据库为基础，再用驱动内存覆盖运行时状态。
	// 与 Python 版 get_vms 先执行 data_get() 再列出的语义一致：
	// 只依赖驱动内存会在驱动刚创建/平台扫描失败时返回空列表。
	vms := make(map[string]*model.VMConfig)
	if rawMap, err := app.DB.GetVMSaving(hsName); err == nil {
		for uuid, raw := range rawMap {
			if cfg, decodeErr := service.DecodeVMConfig(raw); decodeErr == nil && cfg != nil {
				vms[uuid] = cfg
			}
		}
	}
	if d := app.HostSvc.GetDriver(hsName); d != nil {
		for uuid, live := range d.GetVMSaving() {
			if live == nil {
				continue
			}
			if base, ok := vms[uuid]; ok {
				// 保留数据库中的业务字段，仅同步平台侧运行时状态
				base.VmFlag = live.VmFlag
				base.VmDeleted = live.VmDeleted
				continue
			}
			vms[uuid] = live
		}
	}

	isSuperUser := uc.IsAdmin || uc.IsToken
	result := make(map[string]interface{}, len(vms))
	for uuid, vm := range vms {
		if vm == nil || vm.VmDeleted {
			// 平台侧已不存在（扫描标记软删除）的虚拟机不再返回
			continue
		}
		// 普通用户只能看到自己拥有的虚拟机，
		// 否则同主机内其它用户的系统/VNC 口令会被一并返回。
		if !isSuperUser {
			if vm.OwnAll == nil {
				continue
			}
			if _, owns := vm.OwnAll[uc.Username]; !owns {
				continue
			}
		}
		result[uuid] = app.buildVMView(hsName, uuid, vm, uc)
	}
	utils.Success(c, result)
}

// handleVMDetail GET /api/client/detail/:hs_name/:vm_uuid
//
// 返回结构与 Python 版 get_vm_detail 一致：
// {uuid, config, status, user_permissions, is_admin, current_user}
func (app *App) handleVMDetail(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}

	// 授权由 HostVMAccessMiddleware 统一执行，此处按统一规则复核
	if !app.canOperateVM(uc, hsName, vmUUID) {
		utils.Fail(c, 403, "无权访问此虚拟机")
		return
	}

	vm := app.HostSvc.MustGetVMConfig(hsName, vmUUID)
	if vm == nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}

	view := app.buildVMView(hsName, vmUUID, vm, uc)
	view["current_user"] = uc.Username
	utils.Success(c, view)
}

// handleVMCreate POST /api/client/create/:hs_name
func (app *App) handleVMCreate(c *gin.Context) {
	hsName := c.Param("hs_name")
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}

	// 主机访问权限已由 HostVMAccessMiddleware 统一校验，
	// 这里复用以数据库身份为准的检查（原实现读取不存在的 gin 键，恒为空）。
	if !app.HostSvc.CheckHostAccess(uc.Username, hsName) {
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
	if v, ok := req["nat_all"].([]interface{}); ok {
		cfg.NatAll = parseNatList(v)
	} else if v, ok := req["nat_all"].(map[string]interface{}); ok {
		cfg.NatAll = parseNatListFromMap(v)
	}
	if v, ok := req["web_all"]; ok {
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

	if app.HostSvc.GetDriver(hsName) == nil {
		utils.NotFound(c, "主机不存在或未启用")
		return
	}

	// 配额校验：与 Python 版 _validate_vm_resources 等价，在提交任务前拒绝
	if err := app.checkQuotaForUser(uc, cfg); err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}

	params, err := marshalTaskParams(map[string]interface{}{
		taskParamVMConfig:  cfg,
		taskParamQuotaUser: uc.Username,
	})
	if err != nil {
		utils.InternalError(c, "任务参数序列化失败")
		return
	}

	taskID, err := app.submitVMTask("创建虚拟机 "+vmUUID, task.TaskTypeVMCreate,
		uc.Username, hsName, vmUUID, params)
	if err != nil {
		app.logEvent(c, "vm", "提交创建任务").Host(hsName).VM(vmUUID, cfg.OsName).
			Err(err).Errorf("提交虚拟机创建任务失败：%v", err)
		utils.Fail(c, 400, err.Error())
		return
	}
	app.logEvent(c, "vm", "提交创建任务").Host(hsName).VM(vmUUID, cfg.OsName).
		Details(vmSpecDetails(cfg)).
		Detail("task_id", taskID).
		Info("已提交虚拟机创建任务：%s", vmUUID)
	utils.SuccessMsg(c, "虚拟机创建任务已提交", gin.H{"task_id": taskID})
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

	if app.HostSvc.GetDriver(hsName) == nil {
		utils.NotFound(c, "主机不存在或未启用")
		return
	}
	if !app.HostSvc.VMExists(hsName, vmUUID) {
		utils.NotFound(c, "虚拟机不存在")
		return
	}

	taskID, err := app.submitVMTask("重装系统 "+vmUUID, task.TaskTypeVMReinstall,
		uc.Username, hsName, vmUUID, nil)
	if err != nil {
		app.logEvent(c, "vm", "提交重装任务").Host(hsName).VM(vmUUID, "").
			Err(err).Errorf("提交系统重装任务失败：%v", err)
		utils.Fail(c, 400, err.Error())
		return
	}
	app.logEvent(c, "vm", "提交重装任务").Host(hsName).VM(vmUUID, "").
		Detail("task_id", taskID).
		Info("已提交系统重装任务：%s", vmUUID)
	utils.SuccessMsg(c, "系统重装任务已提交", gin.H{"task_id": taskID})
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

	oldVM, err := app.loadVMConfig(hsName, vmUUID)
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

	// 配额校验：扩容场景必须在提交任务前拒绝
	if err := app.checkQuotaForUser(uc, &newVM); err != nil {
		utils.Fail(c, 400, err.Error())
		return
	}

	params, err := marshalTaskParams(map[string]interface{}{
		taskParamVMConfig:  &newVM,
		taskParamOldConfig: oldVM,
		taskParamQuotaUser: uc.Username,
	})
	if err != nil {
		utils.InternalError(c, "任务参数序列化失败")
		return
	}

	taskID, err := app.submitVMTask("更新虚拟机 "+vmUUID, task.TaskTypeVMUpdate,
		uc.Username, hsName, vmUUID, params)
	if err != nil {
		app.logEvent(c, "vm", "提交更新任务").Host(hsName).VM(vmUUID, newVM.OsName).
			Err(err).Errorf("提交虚拟机更新任务失败：%v", err)
		utils.Fail(c, 400, err.Error())
		return
	}
	app.logEvent(c, "vm", "提交更新任务").Host(hsName).VM(vmUUID, newVM.OsName).
		Detail("old", vmSpecDetails(oldVM)).
		Detail("new", vmSpecDetails(&newVM)).
		Detail("task_id", taskID).
		Info("已提交虚拟机更新任务：%s", vmUUID)
	utils.SuccessMsg(c, "虚拟机更新任务已提交", gin.H{"task_id": taskID})
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
	if app.HostSvc.GetDriver(hsName) == nil {
		utils.NotFound(c, "主机不存在或未启用")
		return
	}

	// 与 Python 版一致：删除以最后一个所有者为准；
	// 管理员删除他人虚拟机时必须显式确认主所有者，避免误删。
	vmCfg := app.HostSvc.MustGetVMConfig(hsName, vmUUID)
	if vmCfg == nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	primaryOwner := service.PrimaryOwner(hsName, vmUUID, vmCfg.OwnAll)

	if !uc.IsAdmin && primaryOwner != "" && primaryOwner != uc.Username {
		utils.Fail(c, 403, "只有主所有者可以删除该虚拟机")
		return
	}
	if uc.IsAdmin && primaryOwner != "" && primaryOwner != uc.Username {
		confirm := strings.TrimSpace(c.Query("confirm_owner"))
		if confirm == "" {
			if v, ok := c.GetPostForm("confirm_owner"); ok {
				confirm = strings.TrimSpace(v)
			}
		}
		// 前端 axios 把确认值放在 DELETE 请求体里
		// （apis.ts deleteVM: http.delete(url, { data: { confirm_owner } })），
		// 只读 query/form 会永远拿不到，导致管理员无法删除他人虚拟机。
		if confirm == "" {
			var body struct {
				ConfirmOwner string `json:"confirm_owner"`
			}
			if err := c.ShouldBindBodyWith(&body, binding.JSON); err == nil {
				confirm = strings.TrimSpace(body.ConfirmOwner)
			}
		}
		if confirm != primaryOwner {
			// 与 Python 版一致：需要二次确认时返回提示信息而非直接执行
			utils.Fail(c, 400, "该虚拟机属于其它用户，请在请求中携带 confirm_owner="+primaryOwner)
			return
		}
	}

	// record_only=true：只移除本机纳管记录（例如扫描时被错前缀纳入的历史记录），
	// 不提交平台删除任务，因此不会影响平台上真实存在的虚拟机。
	if v := strings.ToLower(strings.TrimSpace(c.Query("record_only"))); v == "1" || v == "true" {
		if d := app.HostSvc.GetOrCreateDriver(hsName); d != nil {
			d.RemoveVMSaving(vmUUID)
		}
		if err := app.DB.DeleteVMConfig(hsName, vmUUID); err != nil {
			utils.InternalError(c, "移除虚拟机记录失败: "+err.Error())
			return
		}
		_ = app.DB.DeleteVMStatus(hsName, vmUUID)
		// 仅移除纳管记录不会动平台虚拟机，但它会让该虚拟机从本系统「消失」：
		// 用 WARNING 级别留痕（结果仍为成功），便于事后追溯是谁移除的。
		app.logEvent(c, "vm", "移除纳管记录").Host(hsName).VM(vmUUID, "").
			Emit(logger.LevelWarn, "仅移除纳管记录（未删除平台虚拟机）: %s", vmUUID)
		utils.SuccessMsg(c, "已移除虚拟机记录（平台虚拟机未删除）", gin.H{"vm_uuid": vmUUID, "record_only": true})
		return
	}

	params, err := marshalTaskParams(map[string]interface{}{
		taskParamQuotaUser:    primaryOwner,
		taskParamRemoveBackup: true,
	})
	if err != nil {
		utils.InternalError(c, "任务参数序列化失败")
		return
	}

	taskID, err := app.submitVMTask("删除虚拟机 "+vmUUID, task.TaskTypeVMDelete,
		uc.Username, hsName, vmUUID, params)
	if err != nil {
		app.logEvent(c, "vm", "提交删除任务").Host(hsName).VM(vmUUID, "").
			Err(err).Errorf("提交虚拟机删除任务失败：%v", err)
		utils.Fail(c, 400, err.Error())
		return
	}
	// 删除请求附带主所有者信息，便于区分「本人删除」与「管理员代删」
	app.logEvent(c, "vm", "提交删除任务").Host(hsName).VM(vmUUID, "").
		Detail("primary_owner", primaryOwner).
		Detail("task_id", taskID).
		Emit(logger.LevelWarn, "已提交虚拟机删除任务：%s", vmUUID)
	utils.SuccessMsg(c, "虚拟机删除任务已提交", gin.H{"task_id": taskID})
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
		app.logEvent(c, "vm", "电源操作").Host(hsName).VM(vmUUID, "").
			Detail("power_action", string(action)).Err(err).
			Errorf("虚拟机电源操作失败：%s (%v)", action, err)
		utils.InternalError(c, "电源操作失败")
		return
	}
	if !result.Success {
		app.logEvent(c, "vm", "电源操作").Host(hsName).VM(vmUUID, "").
			Detail("power_action", string(action)).
			Detail("platform_message", result.Message).
			Errorf("虚拟机电源操作被平台拒绝：%s", result.Message)
		utils.Fail(c, 500, result.Message)
		return
	}

	// 更新数据库中的VM状态
	vm, _ := app.loadVMConfig(hsName, vmUUID)
	if vm != nil {
		_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	}

	// 软关机 / 软重启是异步操作：平台受理后状态不会立刻变化，
	// 需要后台监控直到实际状态稳定（对应 Python 版 BasicServer.soft_pwr）。
	if action == model.VMPowerSClose || action == model.VMPowerSReset {
		type softPowerStarter interface {
			SoftPowerMonitor(vmUUID string, expected model.VMPowers)
		}
		if sp, ok := d.(softPowerStarter); ok {
			sp.SoftPowerMonitor(vmUUID, model.VMPowerOnStop)
		}
	}

	app.logEvent(c, "vm", "电源操作").Host(hsName).VM(vmUUID, "").
		Detail("power_action", string(action)).
		Info("虚拟机电源操作已执行：%s", action)
	utils.SuccessMsg(c, "电源操作已执行", nil)
}

// handleVMRemote GET /api/client/remote/:hs_name/:vm_uuid
//
// 返回结构：
//
//	{ console_url, terminal_url, type, host, port, password, ... }
//
// Python 版 VMRemote 返回的是「可直接打开的 noVNC 地址」字符串
// （形如 http://<public>:<remote_port>/vnc.html?autoconnect=true&path=websockify?token=xxx），
// 前端读取 `data.console_url` 后直接 window.open。
// 此前 Go 版只把驱动的 Results({type,host,port,password}) 原样返回，
// 前端取 console_url 恒为 undefined，表现为「获取的控制台地址无效」。
// 这里在保留原始字段的前提下，额外组装 console_url / terminal_url。
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

	// ZMessage.Results 是 interface{}：按 Python 契约应为 dict，
	// 但个别驱动可能返回字符串（直接给地址）或其它形态，这里统一归一化。
	results := map[string]interface{}{}
	switch v := result.Results.(type) {
	case map[string]interface{}:
		results = v
	case string:
		// 驱动直接返回了控制台地址（如 VMware/Proxmox 的 noVNC 链接）
		if strings.HasPrefix(v, "http") {
			results["console_url"] = v
		}
	default:
		// 尝试通过 JSON 往返转换（如 struct 形态）
		if raw, err := json.Marshal(v); err == nil {
			_ = json.Unmarshal(raw, &results)
		}
	}

	// 保留驱动原始结果（type/host/port/password/ticket 等）
	out := make(map[string]interface{}, len(results)+4)
	for k, v := range results {
		out[k] = v
	}
	// 驱动可能直接给 message 就是地址（Python 版 VMRemote 的返回风格）
	if _, ok := out["console_url"]; !ok {
		if m := strings.TrimSpace(result.Message); strings.HasPrefix(m, "http") {
			out["console_url"] = m
		}
	}

	// 拼装 noVNC 控制台地址
	if _, ok := out["console_url"]; !ok {
		if consoleURL := app.buildConsoleURL(d, hsName, vmUUID, results); consoleURL != "" {
			out["console_url"] = consoleURL
		}
	}

	utils.Success(c, out)
}

// consoleHostFromRequest 推导控制台对外可访问的主机名。
//
// 优先级：请求 Host（生产同源部署时最准确） > 主机配置 public_addr[0]。
// 需要去掉端口部分，因为我们只取主机名再拼 remote_port。
func consoleHostFromRequest(c *gin.Context, cfg *model.HSConfig) string {
	if c != nil && c.Request != nil && c.Request.Host != "" {
		if h, _, err := net.SplitHostPort(c.Request.Host); err == nil && h != "" {
			return h
		}
		return c.Request.Host
	}
	if cfg != nil {
		for _, a := range cfg.PublicAddr {
			if a != "" && a != "localhost" && a != "127.0.0.1" {
				return a
			}
		}
		if len(cfg.PublicAddr) > 0 {
			return cfg.PublicAddr[0]
		}
	}
	return "127.0.0.1"
}

// buildConsoleURL 组装 noVNC 访问地址（与 Python VMRemote 行为一致）。
//
// 当驱动直接给出了可用地址（Results.url / console_url）时优先采用；
// 否则依据 remote_port + VNC 主机端口注册一次性 token 并拼装。
func (app *App) buildConsoleURL(d driver.HostDriver, hsName, vmUUID string, results map[string]interface{}) string {
	// 1. 驱动已给出完整地址
	for _, key := range []string{"console_url", "url"} {
		if v, ok := results[key].(string); ok && strings.HasPrefix(v, "http") {
			return v
		}
	}

	cfg := d.GetConfig()
	host, port := "", 0
	if v, ok := results["host"].(string); ok {
		host = v
	}
	switch v := results["port"].(type) {
	case int:
		port = v
	case int64:
		port = int(v)
	case float64:
		port = int(v)
	}
	if host == "" || port <= 0 {
		return ""
	}

	// VNC 代理端口：默认 6080（与 cmd/server/main.go 中 NewVNCProxyServer(6080) 一致）
	proxyPort := vncProxyPort
	scheme := "http"
	consoleHost := consoleHostFromRequest(nil, cfg)

	// 注册一次性 token，供 6080 的 /websockify 校验
	token := registerVNCBridgeToken(hsName, vmUUID, host, port)

	return fmt.Sprintf("%s://%s:%d/vnc.html?autoconnect=true&path=websockify&token=%s",
		scheme, consoleHost, proxyPort, token)
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
//
// 返回结构与 Python get_vm_status 一致：
//
//	{ power_status: "STARTED", history: [ {HWStatus}, ... ] }
//
// 此前返回的是裸数组，前端两处消费因此失效：
//   - DockDetail.tsx 读 data.power_status 恒为 undefined（电源态刷新逻辑失效）；
//   - DockDetail.tsx loadMonitorData 读 data?.history 恒为 undefined，
//     导致性能/资源/网络监控图表永远为空。
//
// 时间范围：前端传 ?limit=<分钟>（Python 同名字段），
// 同时兼容显式的 start/end 时间戳。
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
	// limit=分钟数（默认 30 分钟），与 Python request.args.get('limit', type=int, default=30) 对齐
	if startTS == 0 {
		limitMin := 30
		if v := c.Query("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limitMin = n
			}
		}
		now := time.Now().Unix()
		startTS = now - int64(limitMin)*60
		if endTS == 0 {
			endTS = now
		}
	}

	statusList, err := app.DB.GetVMStatus(hsName, vmUUID, startTS, endTS)
	if err != nil {
		utils.InternalError(c, "获取虚拟机状态失败")
		return
	}

	history := []interface{}{}
	for _, s := range statusList {
		var data interface{}
		if err := json.Unmarshal([]byte(s), &data); err == nil {
			history = append(history, data)
		}
	}

	// 最新电源状态取自 VMConfig.vm_flag（与 Python 一致）
	powerStatus := string(model.VMPowerUnknown)
	if vmCfg := app.HostSvc.MustGetVMConfig(hsName, vmUUID); vmCfg != nil && vmCfg.VmFlag != "" {
		powerStatus = string(vmCfg.VmFlag)
	}

	utils.Success(c, map[string]interface{}{
		"power_status": powerStatus,
		"history":      history,
	})
}

// parseNicAll 解析网卡配置（map: 网卡名 -> 配置）
func parseNicAll(data map[string]interface{}) map[string]model.NCConfig {
	result := make(map[string]model.NCConfig, len(data))
	for name, v := range data {
		m, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		var nic model.NCConfig
		if b, err := json.Marshal(m); err == nil {
			_ = json.Unmarshal(b, &nic)
		}
		if nic.NicName == "" {
			nic.NicName = name
		}
		result[name] = nic
	}
	return result
}

// parseNatList 解析NAT端口转发列表（数组形式）
func parseNatList(list []interface{}) []model.PortData {
	result := make([]model.PortData, 0, len(list))
	for _, v := range list {
		m, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		var pd model.PortData
		if b, err := json.Marshal(m); err == nil {
			_ = json.Unmarshal(b, &pd)
		}
		result = append(result, pd)
	}
	return result
}

// parseNatListFromMap 解析NAT端口转发（map 形式）
func parseNatListFromMap(data map[string]interface{}) []model.PortData {
	list := make([]interface{}, 0, len(data))
	for _, v := range data {
		list = append(list, v)
	}
	return parseNatList(list)
}

// parseWebAll 解析反向代理配置（数组或 map 形式）
func parseWebAll(data interface{}) []model.WebProxy {
	var list []interface{}
	switch v := data.(type) {
	case []interface{}:
		list = v
	case map[string]interface{}:
		for _, item := range v {
			list = append(list, item)
		}
	default:
		return nil
	}

	result := make([]model.WebProxy, 0, len(list))
	for _, v := range list {
		m, ok := v.(map[string]interface{})
		if !ok {
			continue
		}
		var web model.WebProxy
		if b, err := json.Marshal(m); err == nil {
			_ = json.Unmarshal(b, &web)
		}
		result = append(result, web)
	}
	return result
}

// handleVMScan POST /api/client/scaner/:hs_name
//
// 与 Python 版 VScanner 语义对齐，并修复原先的两个缺陷：
//  1. 扫描结果**持久化到数据库**。原实现只写入驱动内存，
//     一旦进程重启或列表接口走数据库分支，扫描到的虚拟机就会消失，
//     表现为「扫描不到虚拟机」。
//  2. 返回结构化统计（识别/新增/落库/恢复数量），并对失败原因可见：
//     平台命令失败时驱动会静默返回内存列表，导致「扫描完成：0 台」无法排查。
func (app *App) handleVMScan(c *gin.Context) {
	hsName := c.Param("hs_name")

	row, err := app.DB.GetHost(hsName)
	if err != nil || row == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	if row.EnableHost == 0 {
		utils.BadRequest(c, "主机已被禁用，无法扫描虚拟机")
		return
	}

	d := app.HostSvc.GetOrCreateDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在或未启用")
		return
	}

	msg, scanErr := d.ScanVMs()
	if scanErr != nil {
		utils.InternalError(c, "扫描虚拟机失败: "+scanErr.Error())
		return
	}
	if msg != nil && !msg.Success {
		utils.Fail(c, 500, msg.Message)
		return
	}

	// 平台侧枚举结果（含内存态，用于恢复此前被标记删除的虚拟机）
	platformVMs, listErr := d.GetVMList()
	if listErr != nil {
		utils.InternalError(c, "读取平台虚拟机列表失败: "+listErr.Error())
		return
	}

	snapshot := d.GetVMSaving()
	recovered := 0
	for uuid, vm := range snapshot {
		if vm == nil || !vm.VmDeleted {
			continue
		}
		if _, exists := platformVMs[uuid]; exists {
			d.SetVMDeleted(uuid, false)
			recovered++
		}
	}

	// 扫描结果必须落库：这是「扫描后能看到虚拟机」的关键一步
	persisted := 0
	for uuid, vm := range d.GetVMSaving() {
		if vm == nil {
			continue
		}
		if err := app.DB.UpsertVMConfig(hsName, uuid, vm); err != nil {
			app.Logger.Log(hsName, "ERROR", "扫描虚拟机",
				fmt.Sprintf("保存虚拟机 %s 失败: %v", uuid, err), false)
			continue
		}
		persisted++
	}

	added := 0
	if msg != nil {
		if results, ok := msg.Results.(map[string]interface{}); ok {
			if v, ok := results["added"].(int); ok {
				added = v
			}
		}
	}

	app.Logger.Log(hsName, "INFO", "扫描虚拟机",
		fmt.Sprintf("扫描完成：识别 %d 台，落库 %d 台，恢复 %d 台", len(platformVMs), persisted, recovered), true)

	utils.SuccessMsg(c, "扫描完成", gin.H{
		"scanned":       len(platformVMs),
		"added":         added,
		"persisted":     persisted,
		"recovered":     recovered,
		"prefix_filter": row.FilterName,
	})
}

// handleVMCleanup POST /api/client/scaner/:hs_name/cleanup
//
// 清理该主机下「不匹配 filter_name 前缀」的历史纳管记录。
//
// 背景：早期版本的 Proxmox / ESXi / Hyper-V / Workstation 驱动在扫描时
// 没有按 filter_name 过滤，把平台上其它虚拟机也纳入了本机；
// 修复扫描逻辑后，这些旧记录仍留在数据库与驱动内存中。
//
// 该接口**只删除本机记录**（vm_saving / vm_status 与驱动内存），
// 不做任何平台操作，因此不会影响平台上真实存在的虚拟机。
// ?dry_run=true 只预览将删除的记录，供界面二次确认。
func (app *App) handleVMCleanup(c *gin.Context) {
	hsName := c.Param("hs_name")

	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}
	if ok, _ := service.CheckVMPermission("delete", uc.UserData); !ok {
		utils.Forbidden(c)
		return
	}
	if !uc.IsAdmin && !service.CheckHostAccess(hsName, uc.UserData) {
		utils.Forbidden(c)
		return
	}

	row, err := app.DB.GetHost(hsName)
	if err != nil || row == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	prefix := strings.TrimSpace(row.FilterName)
	if prefix == "" {
		// 前缀为空时「不匹配」没有判定依据，拒绝执行以避免误删全部记录
		utils.BadRequest(c, "主机未配置「过滤器名称」，为避免误删暂不执行清理")
		return
	}

	// 候选集合 = 数据库记录 ∪ 驱动内存记录
	candidates := make(map[string]*model.VMConfig)
	rawMap, err := app.DB.GetVMSaving(hsName)
	if err != nil {
		utils.InternalError(c, "读取虚拟机记录失败: "+err.Error())
		return
	}
	for uuid, raw := range rawMap {
		cfg, decodeErr := service.DecodeVMConfig(raw)
		if decodeErr != nil || cfg == nil {
			// 无法解析的记录保守保留，避免误删
			continue
		}
		candidates[uuid] = cfg
	}

	d := app.HostSvc.GetOrCreateDriver(hsName)
	if d != nil {
		for uuid, cfg := range d.GetVMSaving() {
			if cfg == nil {
				continue
			}
			if _, exists := candidates[uuid]; !exists {
				candidates[uuid] = cfg
			}
		}
	}

	removedList := make([]string, 0, len(candidates))
	for uuid, cfg := range candidates {
		if driver.MatchFilterPrefix(prefix, cfg) {
			continue
		}
		removedList = append(removedList, uuid)
	}
	sort.Strings(removedList)

	if v := strings.ToLower(strings.TrimSpace(c.Query("dry_run"))); v == "1" || v == "true" {
		utils.Success(c, gin.H{
			"host_name":     hsName,
			"prefix_filter": prefix,
			"count":         len(removedList),
			"items":         removedList,
			"dry_run":       true,
		})
		return
	}

	removed := 0
	for _, uuid := range removedList {
		if err := app.DB.DeleteVMConfig(hsName, uuid); err != nil {
			app.Logger.Log(hsName, "ERROR", "清理虚拟机记录",
				fmt.Sprintf("删除记录 %s 失败: %v", uuid, err), false)
			continue
		}
		_ = app.DB.DeleteVMStatus(hsName, uuid)
		if d != nil {
			d.RemoveVMSaving(uuid)
		}
		removed++
	}
	// 顺带清理 vm_status 中已无归属记录的孤儿数据
	if n, cleanupErr := app.DB.CleanupDeletedVMStatus(); cleanupErr != nil {
		app.Logger.Log(hsName, "WARN", "清理虚拟机记录",
			fmt.Sprintf("清理孤儿状态记录失败: %v", cleanupErr), false)
	} else if n > 0 {
		app.Logger.Log(hsName, "INFO", "清理虚拟机记录",
			fmt.Sprintf("清理孤儿状态记录 %d 条", n), false)
	}

	app.Logger.Log(hsName, "INFO", "清理虚拟机记录",
		fmt.Sprintf("按前缀 %q 清理 %d 条不匹配记录（仅记录，未删除平台虚拟机）", prefix, removed), true)

	utils.SuccessMsg(c, fmt.Sprintf("已清理 %d 条不匹配前缀的记录", removed), gin.H{
		"host_name":     hsName,
		"prefix_filter": prefix,
		"removed":       removed,
		"items":         removedList,
	})
}

// vmConfigScore 评估虚拟机记录的业务数据完整度，用于合并重复记录时决定保留哪条。
func vmConfigScore(cfg *model.VMConfig) int {
	if cfg == nil {
		return -1
	}
	score := 0
	if len(cfg.OwnAll) > 0 {
		score += 10
	}
	if cfg.OsName != "" {
		score += 5
	}
	if cfg.OsPass != "" {
		score += 3
	}
	if len(cfg.NicAll) > 0 {
		score += 3
	}
	if cfg.VmDeleted {
		score--
	}
	return score + 3*len(cfg.Backups) + 2*(len(cfg.NatAll)+len(cfg.WebAll)+
		len(cfg.HddAll)+len(cfg.IsoAll)+len(cfg.PciAll)+len(cfg.UsbAll))
}

// mergeVMConfigInto 把 src 中「dst 尚缺」的信息补进 dst（dst 优先，不覆盖已有值）。
//
// 合并重复记录时，被删除的一条往往带有另一条缺失的业务字段
// （所有者、端口映射、备份列表等），这里逐字段互补以避免丢数据。
func mergeVMConfigInto(dst, src *model.VMConfig) {
	if dst == nil || src == nil {
		return
	}
	dv, sv := reflect.ValueOf(dst).Elem(), reflect.ValueOf(src).Elem()
	t := dv.Type()
	for i := 0; i < dv.NumField(); i++ {
		name := t.Field(i).Name
		// ExtData 为运行时缓存，VmUUID/VmDeleted 跟随保留者，均不参与合并
		if name == "ExtData" || name == "VmUUID" || name == "VmDeleted" {
			continue
		}
		df, sf := dv.Field(i), sv.Field(i)
		if !df.CanSet() {
			continue
		}
		switch df.Kind() {
		case reflect.Map:
			if sf.IsNil() || sf.Len() == 0 {
				continue
			}
			if df.IsNil() {
				df.Set(reflect.MakeMap(df.Type()))
			}
			for _, k := range sf.MapKeys() {
				if !df.MapIndex(k).IsValid() {
					df.SetMapIndex(k, sf.MapIndex(k))
				}
			}
		case reflect.Slice:
			if sf.IsNil() || sf.Len() == 0 {
				continue
			}
			if name == "Backups" {
				// 备份按名称去重后再追加
				existing := make(map[string]bool, df.Len())
				for j := 0; j < df.Len(); j++ {
					existing[df.Index(j).FieldByName("BackupName").String()] = true
				}
				for j := 0; j < sf.Len(); j++ {
					backupName := sf.Index(j).FieldByName("BackupName").String()
					if backupName == "" || existing[backupName] {
						continue
					}
					existing[backupName] = true
					df.Set(reflect.Append(df, sf.Index(j)))
				}
				continue
			}
			df.Set(reflect.AppendSlice(df, sf))
		case reflect.String:
			if df.String() == "" {
				df.SetString(sf.String())
			}
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			if df.Int() == 0 {
				df.SetInt(sf.Int())
			}
		case reflect.Float32, reflect.Float64:
			if df.Float() == 0 {
				df.SetFloat(sf.Float())
			}
		case reflect.Bool:
			if !df.Bool() {
				df.SetBool(sf.Bool())
			}
		}
	}
}

// handleVMDedupe POST /api/client/scaner/:hs_name/dedupe
//
// 合并同一主机下由「命名变体」造成的重复虚拟机记录。
//
// 背景：Proxmox 平台上的名称使用连字符，而早期 Go 驱动在扫描时把 "-"
// 替换成 "_" 作为 vm_uuid，导致同一台虚拟机同时存在 pve-xxx 与 pve_xxx
// 两条记录。这里按命名变体分组，保留业务数据更完整的一条（另一条缺失的
// 字段会被补齐），删除多余的记录。
//
// 只删除本机记录（vm_saving / vm_status 与驱动内存），不做平台操作；
// ?dry_run=true 仅预览将合并的分组。
func (app *App) handleVMDedupe(c *gin.Context) {
	hsName := c.Param("hs_name")

	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}
	if ok, _ := service.CheckVMPermission("delete", uc.UserData); !ok {
		utils.Forbidden(c)
		return
	}
	if !uc.IsAdmin && !service.CheckHostAccess(hsName, uc.UserData) {
		utils.Forbidden(c)
		return
	}

	row, err := app.DB.GetHost(hsName)
	if err != nil || row == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	if row.ServerType != "PromoxSetup" {
		utils.BadRequest(c, "该主机类型不存在命名变体重复问题")
		return
	}

	// 候选集合 = 数据库记录 ∪ 驱动内存记录
	candidates := make(map[string]*model.VMConfig)
	rawMap, err := app.DB.GetVMSaving(hsName)
	if err != nil {
		utils.InternalError(c, "读取虚拟机记录失败: "+err.Error())
		return
	}
	for uuid, raw := range rawMap {
		cfg, decodeErr := service.DecodeVMConfig(raw)
		if decodeErr != nil || cfg == nil {
			continue
		}
		candidates[uuid] = cfg
	}
	d := app.HostSvc.GetOrCreateDriver(hsName)
	if d != nil {
		for uuid, cfg := range d.GetVMSaving() {
			if cfg == nil {
				continue
			}
			if _, exists := candidates[uuid]; !exists {
				candidates[uuid] = cfg
			}
		}
	}

	// 按命名变体分组（"_" 与 "-" 视为等价）
	groups := make(map[string][]string)
	for uuid := range candidates {
		normalized := strings.ReplaceAll(uuid, "_", "-")
		groups[normalized] = append(groups[normalized], uuid)
	}

	type dedupeGroup struct {
		Keep   string   `json:"keep"`
		Remove []string `json:"remove"`
		Score  int      `json:"score"`
	}
	plans := make([]dedupeGroup, 0, len(groups))
	merged := make(map[string]*model.VMConfig)

	for _, keys := range groups {
		if len(keys) < 2 {
			continue
		}
		sort.Slice(keys, func(i, j int) bool {
			si, sj := vmConfigScore(candidates[keys[i]]), vmConfigScore(candidates[keys[j]])
			if si != sj {
				return si > sj
			}
			// 同分优先保留连字符形式（与平台名称一致），再按字典序稳定排序
			hi, hj := !strings.Contains(keys[i], "_"), !strings.Contains(keys[j], "_")
			if hi != hj {
				return hi
			}
			return keys[i] < keys[j]
		})

		keep := keys[0]
		cfg := candidates[keep]
		if cfg == nil {
			cfg = model.NewVMConfig()
			cfg.VmUUID = keep
		}
		remove := make([]string, 0, len(keys)-1)
		for _, other := range keys[1:] {
			mergeVMConfigInto(cfg, candidates[other])
			remove = append(remove, other)
		}
		merged[keep] = cfg
		plans = append(plans, dedupeGroup{Keep: keep, Remove: remove, Score: vmConfigScore(candidates[keep])})
	}

	sort.Slice(plans, func(i, j int) bool { return plans[i].Keep < plans[j].Keep })

	if v := strings.ToLower(strings.TrimSpace(c.Query("dry_run"))); v == "1" || v == "true" {
		utils.Success(c, gin.H{
			"host_name":   hsName,
			"count":       len(plans),
			"groups":      plans,
			"dry_run":     true,
			"server_type": row.ServerType,
		})
		return
	}

	mergedCount := 0
	removed := 0
	for _, p := range plans {
		if err := app.DB.UpsertVMConfig(hsName, p.Keep, merged[p.Keep]); err != nil {
			app.Logger.Log(hsName, "ERROR", "合并重复记录",
				fmt.Sprintf("写回保留记录 %s 失败: %v", p.Keep, err), false)
			continue
		}
		for _, uuid := range p.Remove {
			_ = app.DB.DeleteVMConfig(hsName, uuid)
			_ = app.DB.DeleteVMStatus(hsName, uuid)
			if d != nil {
				d.RemoveVMSaving(uuid)
			}
			removed++
		}
		if d != nil {
			d.UpsertVMSaving(p.Keep, merged[p.Keep])
		}
		mergedCount++
	}

	app.Logger.Log(hsName, "INFO", "合并重复记录",
		fmt.Sprintf("合并命名变体重复记录 %d 组，移除多余记录 %d 条（仅本机记录）", mergedCount, removed), true)

	utils.SuccessMsg(c, fmt.Sprintf("已合并 %d 组重复记录，移除 %d 条多余记录", mergedCount, removed), gin.H{
		"host_name": hsName,
		"merged":    mergedCount,
		"removed":   removed,
		"groups":    plans,
	})
}

// handleVMUpload POST /api/client/upload（公开接口，虚拟机上报状态）
//
// 该接口无需认证，因此必须自我约束：
//  1. 限制请求体大小，避免被用于内存放大；
//  2. 校验标识格式并确认虚拟机已登记，避免任意数据写入状态表。
func (app *App) handleVMUpload(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256*1024)

	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误或请求体过大")
		return
	}
	hsName, _ := req["hs_name"].(string)
	vmUUID, _ := req["vm_uuid"].(string)
	if hsName == "" || vmUUID == "" {
		utils.BadRequest(c, "hs_name和vm_uuid不能为空")
		return
	}
	if !vmUUIDLooksSafe(vmUUID) || !vmUUIDLooksSafe(hsName) {
		utils.BadRequest(c, "标识格式无效")
		return
	}
	// 仅接受已登记虚拟机的上报，拒绝为任意名称创建状态记录
	if !app.HostSvc.VMExists(hsName, vmUUID) {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	acStatus, _ := req["ac_status"].(string)
	now := time.Now().Unix()
	if err := app.DB.AddVMStatus(hsName, vmUUID, req, acStatus, now, 0); err != nil {
		utils.InternalError(c, "状态保存失败")
		return
	}
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	// 先下发（平台通道可能自动分配外网端口），再记录到配置
	userID := 0
	if uc := auth.GetUserContext(c); uc != nil {
		userID = model.SafeGetInt(uc.UserData, "id")
	}
	// 先下发，成功后才落库：原实现在下发失败时仍写入配置并返回成功类响应，
	// 造成规则表、虚拟机配置与实际转发状态三方不一致。
	if dispatchErr := app.NATService.ApplyPortMap(hsName, vmUUID, &req, userID); dispatchErr != nil {
		app.Logger.Log(hsName, "ERROR", "NAT端口映射", "下发失败: "+dispatchErr.Error(), false)
		utils.Fail(c, 502, "端口映射下发失败，未写入配置: "+dispatchErr.Error())
		return
	}

	vm.NatAll = append(vm.NatAll, req)
	if _, err := d.UpdateVM(vm, vm); err != nil {
		app.Logger.Log(hsName, "WARNING", "NAT端口映射", "更新驱动配置失败: "+err.Error(), false)
	}
	if err := app.DB.UpsertVMConfig(hsName, vmUUID, vm); err != nil {
		app.Logger.Log(hsName, "ERROR", "NAT端口映射", "保存虚拟机配置失败: "+err.Error(), false)
		utils.Fail(c, 500, "端口已下发但配置保存失败，请联系管理员核对")
		return
	}
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	if ruleIndex < 0 || ruleIndex >= len(vm.NatAll) {
		utils.BadRequest(c, "规则索引无效")
		return
	}
	removed := vm.NatAll[ruleIndex]
	vm.NatAll = append(vm.NatAll[:ruleIndex], vm.NatAll[ruleIndex+1:]...)
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)

	// 下发路由器删除
	if err := app.NATService.RevokePortMap(hsName, vmUUID, removed); err != nil {
		app.Logger.Log(hsName, "WARNING", "NAT端口映射", "下发路由器删除失败: "+err.Error(), false)
	}
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
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

// staticBindRequest 静态绑定请求体（字段名与 Python 版 NCConfig 对齐）
type staticBindRequest struct {
	IP4Addr string `json:"ip4_addr"`
	MacAddr string `json:"mac_addr"`
	DNS1    string `json:"dns1"`
	DNS2    string `json:"dns2"`
}

// handleIPBind POST /api/client/ipaddr/bind/:hs_name/:vm_uuid
//
// 在爱快路由器上下发静态绑定（DHCP 静态地址 + ARP），对应 Python 版
// BasicServer.NetiKuai(flag=true)。两种用法：
//   - 请求体给出 ip4_addr + mac_addr：只下发这一条；
//   - 请求体为空：按虚拟机 nic_all 中所有已配置 IP/MAC 的网卡批量下发
//     （对应 Python 版 IPBinder_ROS 遍历 nic_all 的行为）。
func (app *App) handleIPBind(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")

	var req staticBindRequest
	_ = c.ShouldBindJSON(&req)

	if app.NATService == nil {
		utils.InternalError(c, "NAT服务未初始化")
		return
	}

	if strings.TrimSpace(req.IP4Addr) != "" && strings.TrimSpace(req.MacAddr) != "" {
		// 未配置爱快时 BindStatic 会静默跳过，这里必须先判定，
		// 否则接口会把「什么都没做」回成成功。
		if !app.NATService.StaticBindConfigured(hsName) {
			utils.Fail(c, 400, "主机 "+hsName+" 未配置爱快路由器，无法下发静态绑定")
			return
		}
		if err := app.NATService.BindStatic(hsName, vmUUID, req.IP4Addr, req.MacAddr, req.DNS1, req.DNS2); err != nil {
			app.Logger.Log(hsName, "ERROR", "静态IP绑定", "下发失败: "+err.Error(), false)
			utils.Fail(c, 502, "静态绑定下发失败: "+err.Error())
			return
		}
		utils.SuccessMsg(c, "静态绑定成功", nil)
		return
	}

	vm := app.HostSvc.MustGetVMConfig(hsName, vmUUID)
	if vm == nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	res, err := app.NATService.BindStaticForVM(hsName, vmUUID, vm)
	if err != nil {
		app.Logger.Log(hsName, "ERROR", "静态IP绑定", "批量下发失败: "+err.Error(), false)
		utils.Fail(c, 502, "静态绑定下发失败: "+err.Error())
		return
	}
	if res.NotConfigured {
		utils.Fail(c, 400, "主机 "+hsName+" 未配置爱快路由器，无法下发静态绑定")
		return
	}
	if res.Bound == 0 {
		// 网卡缺少 IP/MAC 时无法下发，必须明确报错而不是返回「成功」
		app.Logger.Log(hsName, "WARNING", "静态IP绑定", "无可绑定的网卡: "+res.Summary(), false)
		utils.Fail(c, 400, "没有可绑定的网卡（请先为网卡配置 IP 与 MAC）: "+res.Summary())
		return
	}
	utils.SuccessMsg(c, "静态绑定成功: "+res.Summary(), nil)
}

// handleIPUnbind DELETE /api/client/ipaddr/bind/:hs_name/:vm_uuid
//
// 解除爱快路由器上的静态绑定（DHCP 静态地址 + ARP），对应 Python 版
// BasicServer.NetiKuai(flag=false)。ip4_addr/mac_addr 可放在请求体或查询串中；
// 两者都未提供时，按虚拟机 nic_all 中的网卡逐条解绑。
// 记录不存在视为已解绑，接口保持幂等。
func (app *App) handleIPUnbind(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")

	var req staticBindRequest
	_ = c.ShouldBindJSON(&req)
	if strings.TrimSpace(req.IP4Addr) == "" {
		req.IP4Addr = c.Query("ip4_addr")
	}
	if strings.TrimSpace(req.MacAddr) == "" {
		req.MacAddr = c.Query("mac_addr")
	}

	if app.NATService == nil {
		utils.InternalError(c, "NAT服务未初始化")
		return
	}

	if strings.TrimSpace(req.IP4Addr) != "" || strings.TrimSpace(req.MacAddr) != "" {
		if err := app.NATService.UnbindStatic(hsName, req.IP4Addr, req.MacAddr); err != nil {
			app.Logger.Log(hsName, "ERROR", "静态IP绑定", "解绑失败: "+err.Error(), false)
			utils.Fail(c, 502, "静态绑定解绑失败: "+err.Error())
			return
		}
		utils.SuccessMsg(c, "静态绑定已解绑", nil)
		return
	}

	vm := app.HostSvc.MustGetVMConfig(hsName, vmUUID)
	if vm == nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	if err := app.NATService.UnbindStaticForVM(hsName, vm); err != nil {
		app.Logger.Log(hsName, "ERROR", "静态IP绑定", "批量解绑失败: "+err.Error(), false)
		utils.Fail(c, 502, "静态绑定解绑失败: "+err.Error())
		return
	}
	utils.SuccessMsg(c, "静态绑定已解绑", nil)
}

// ============================================================
// 反向代理 API（用户级）
// ============================================================

// proxyRequest 反代配置的入参。
//
// 同时接受两套字段名，避免前端/接口文档口径不一致导致静默失效：
//   - domain / backend_ip / backend_port / ssl_enabled / description
//     （Python add_vm_proxy_config 与前端 apis.ts createWebProxy 使用）
//   - web_addr / lan_addr / lan_port / is_https / web_tips
//     （model.WebProxy 的内部字段名）
//
// 此前只绑定后者，前端发来的 domain 等字段全部落成零值，
// 结果是域名被写成空串、端口写成 0，代理功能静默失效。
type proxyRequest struct {
	Domain      string `json:"domain"`
	BackendIP   string `json:"backend_ip"`
	BackendPort *int   `json:"backend_port"`
	SSLEnabled  *bool  `json:"ssl_enabled"`
	Description string `json:"description"`

	WebAddr string `json:"web_addr"`
	LanAddr string `json:"lan_addr"`
	LanPort *int   `json:"lan_port"`
	IsHTTPS *bool  `json:"is_https"`
	WebTips string `json:"web_tips"`
}

// toWebProxy 归一化为内部模型（Python 口径优先）
func (r proxyRequest) toWebProxy() model.WebProxy {
	p := model.WebProxy{}
	if r.Domain != "" {
		p.WebAddr = r.Domain
	} else {
		p.WebAddr = r.WebAddr
	}
	if r.BackendIP != "" {
		p.LanAddr = r.BackendIP
	} else {
		p.LanAddr = r.LanAddr
	}
	switch {
	case r.BackendPort != nil:
		p.LanPort = *r.BackendPort
	case r.LanPort != nil:
		p.LanPort = *r.LanPort
	default:
		p.LanPort = 80
	}
	switch {
	case r.SSLEnabled != nil:
		p.IsHTTPS = *r.SSLEnabled
	case r.IsHTTPS != nil:
		p.IsHTTPS = *r.IsHTTPS
	}
	if r.Description != "" {
		p.WebTips = r.Description
	} else {
		p.WebTips = r.WebTips
	}
	return p
}

// proxyToFrontendMap 把内部模型摊平为前端/Python 期望的字段名。
//
// Python get_vm_proxy_configs / list_all_proxys_unified 下发的是
// domain / backend_ip / backend_port / ssl_enabled / description，
// 前端 HttpProxys.tsx、DockDetail.tsx 均按这套字段渲染。
// 这里额外带上 proxy_index 与两个别名（port / proxy_*），
// 兼容旧页面里 rule.port || rule.proxy_port 之类的取值方式。
func proxyToFrontendMap(p model.WebProxy, index int, hsName, vmUUID, vmName string) map[string]interface{} {
	m := map[string]interface{}{
		"domain":       p.WebAddr,
		"backend_ip":   p.LanAddr,
		"backend_port": p.LanPort,
		"ssl_enabled":  p.IsHTTPS,
		"description":  p.WebTips,
		"proxy_index":  index,
		"enabled":      true,
		// 别名：兼容不同页面写法
		"port":         p.LanPort,
		"proxy_domain": p.WebAddr,
		"proxy_port":   p.LanPort,
		"host_name":    hsName,
		"vm_uuid":      vmUUID,
	}
	if vmName != "" {
		m["vm_name"] = vmName
	} else {
		m["vm_name"] = vmUUID
	}
	return m
}

// handleProxyUserList GET /api/client/proxys/list
//
// 与 Python list_all_user_proxys 一致：{list:[...], total:n}
// 且每项为摊平字段，而非嵌套 {hs_name, vm_uuid, proxy:{...}}。
func (app *App) handleProxyUserList(c *gin.Context) {
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}
	list := []interface{}{}
	hosts, _ := app.DB.GetAllHosts()
	for _, h := range hosts {
		if !uc.IsAdmin && !service.CheckHostAccess(h.HsName, uc.UserData) {
			continue
		}
		d := app.HostSvc.GetDriver(h.HsName)
		if d == nil {
			continue
		}
		for vmUUID, vm := range d.GetVMSaving() {
			for i, proxy := range vm.WebAll {
				list = append(list, proxyToFrontendMap(proxy, i, h.HsName, vmUUID, ""))
			}
		}
	}
	utils.Success(c, map[string]interface{}{"list": list, "total": len(list)})
}

// handleProxyDetail GET /api/client/proxys/detail/:hs_name/:vm_uuid
//
// 与 Python get_vm_proxy_configs 一致：返回**裸数组**（不是 {list,total}）。
// DockDetail.tsx 用 Array.isArray(response.data) 判断，
// DockDetailV2.tsx 同理，返回对象会导致列表恒为空。
func (app *App) handleProxyDetail(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := app.loadVMConfig(hsName, vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	list := make([]interface{}, 0, len(vm.WebAll))
	for i, proxy := range vm.WebAll {
		list = append(list, proxyToFrontendMap(proxy, i, hsName, vmUUID, ""))
	}
	utils.Success(c, list)
}

// handleProxyCreate POST /api/client/proxys/create/:hs_name/:vm_uuid
func (app *App) handleProxyCreate(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var raw proxyRequest
	if err := c.ShouldBindJSON(&raw); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	req := raw.toWebProxy()
	if req.WebAddr == "" {
		utils.BadRequest(c, "域名不能为空")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := app.loadVMConfig(hsName, vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	// 域名重复校验（与 Python ProxyMap 的校验语义一致）
	for _, w := range vm.WebAll {
		if w.WebAddr == req.WebAddr {
			utils.BadRequest(c, "域名已存在: "+req.WebAddr)
			return
		}
	}

	vm.WebAll = append(vm.WebAll, req)
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)

	// 记录代理并下发（平台通道优先，否则重载 Caddy；失败不阻断）
	userID := 0
	if uc := auth.GetUserContext(c); uc != nil {
		userID = model.SafeGetInt(uc.UserData, "id")
	}
	_ = app.DB.UpsertWebProxy(hsName, vmUUID, userID, req.WebAddr, req.WebAddr, req.LanAddr, req.LanPort, req.IsHTTPS, req.WebTips)
	if err := app.ProxyService.ApplyProxy(hsName, vmUUID, req, true); err != nil {
		app.Logger.Log(hsName, "WARNING", "反向代理", "下发失败: "+err.Error(), false)
		utils.SuccessMsg(c, "代理已记录，但下发失败: "+err.Error(), nil)
		return
	}
	utils.SuccessMsg(c, "代理添加成功", nil)
}

// handleProxyUpdate PUT /api/client/proxys/update/:hs_name/:vm_uuid/:proxy_index
// 以及 PUT /api/admin/proxys/update/:hs_name/:vm_uuid/:proxy_index
//
// 对应 Python admin_update_proxy：先撤下旧代理，再写入新配置。
func (app *App) handleProxyUpdate(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	proxyIndex, _ := strconv.Atoi(c.Param("proxy_index"))
	var raw proxyRequest
	if err := c.ShouldBindJSON(&raw); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := app.loadVMConfig(hsName, vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	if proxyIndex < 0 || proxyIndex >= len(vm.WebAll) {
		utils.BadRequest(c, "代理索引无效")
		return
	}

	old := vm.WebAll[proxyIndex]
	// 未提供的字段沿用旧值（与 Python admin_update_proxy 的 getattr 兜底一致）
	req := raw.toWebProxy()
	if req.WebAddr == "" {
		req.WebAddr = old.WebAddr
	}
	if req.LanAddr == "" {
		req.LanAddr = old.LanAddr
	}
	if raw.BackendPort == nil && raw.LanPort == nil {
		req.LanPort = old.LanPort
	}
	if raw.SSLEnabled == nil && raw.IsHTTPS == nil {
		req.IsHTTPS = old.IsHTTPS
	}
	if req.WebTips == "" {
		req.WebTips = old.WebTips
	}
	if req.WebAddr == "" {
		utils.BadRequest(c, "域名不能为空")
		return
	}
	// 换域名时不能与其他规则冲突
	for i, w := range vm.WebAll {
		if i != proxyIndex && w.WebAddr == req.WebAddr {
			utils.BadRequest(c, "域名已存在: "+req.WebAddr)
			return
		}
	}

	vm.WebAll[proxyIndex] = req
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)

	// 撤下旧规则、下发新规则
	if old.WebAddr != req.WebAddr {
		_ = app.DB.DeleteWebProxy(hsName, vmUUID, old.WebAddr)
	}
	if err := app.ProxyService.ApplyProxy(hsName, vmUUID, old, false); err != nil {
		app.Logger.Log(hsName, "WARNING", "反向代理", "撤销旧规则失败: "+err.Error(), false)
	}
	userID := 0
	if uc := auth.GetUserContext(c); uc != nil {
		userID = model.SafeGetInt(uc.UserData, "id")
	}
	_ = app.DB.UpsertWebProxy(hsName, vmUUID, userID, req.WebAddr, req.WebAddr, req.LanAddr, req.LanPort, req.IsHTTPS, req.WebTips)
	if err := app.ProxyService.ApplyProxy(hsName, vmUUID, req, true); err != nil {
		app.Logger.Log(hsName, "WARNING", "反向代理", "下发失败: "+err.Error(), false)
		utils.SuccessMsg(c, "代理已更新，但下发失败: "+err.Error(), nil)
		return
	}
	utils.SuccessMsg(c, "代理更新成功", nil)
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	if proxyIndex < 0 || proxyIndex >= len(vm.WebAll) {
		utils.BadRequest(c, "代理索引无效")
		return
	}
	removed := vm.WebAll[proxyIndex]
	vm.WebAll = append(vm.WebAll[:proxyIndex], vm.WebAll[proxyIndex+1:]...)
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)

	// 删除代理记录并下发
	_ = app.DB.DeleteWebProxy(hsName, vmUUID, removed.WebAddr)
	if err := app.ProxyService.ApplyProxy(hsName, vmUUID, removed, false); err != nil {
		app.Logger.Log(hsName, "WARNING", "反向代理", "下发失败: "+err.Error(), false)
	}
	utils.SuccessMsg(c, "代理删除成功", nil)
}

// ============================================================
// 反向代理 API（管理员级）
// ============================================================

func (app *App) handleProxyAdminList(c *gin.Context) {
	app.handleProxyUserList(c)
}

// handleProxyAdminListByHost GET /api/admin/proxys/list/:hs_name
//
// 与 Python admin_list_host_proxys 一致：{list:[...], total:n}
// （注意：与 client 的 detail 接口不同，这里不是裸数组）
func (app *App) handleProxyAdminListByHost(c *gin.Context) {
	hsName := c.Param("hs_name")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	list := []interface{}{}
	for vmUUID, vm := range d.GetVMSaving() {
		for i, proxy := range vm.WebAll {
			list = append(list, proxyToFrontendMap(proxy, i, hsName, vmUUID, ""))
		}
	}
	utils.Success(c, map[string]interface{}{"list": list, "total": len(list)})
}

// handleProxyAdminDetail GET /api/admin/proxys/detail/:hs_name/:vm_uuid
//
// 与 Python admin_get_vm_proxys 一致：{list:[...], total:n}
func (app *App) handleProxyAdminDetail(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := app.loadVMConfig(hsName, vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	list := make([]interface{}, 0, len(vm.WebAll))
	for i, proxy := range vm.WebAll {
		list = append(list, proxyToFrontendMap(proxy, i, hsName, vmUUID, ""))
	}
	utils.Success(c, map[string]interface{}{"list": list, "total": len(list)})
}

func (app *App) handleProxyAdminCreate(c *gin.Context) {
	app.handleProxyCreate(c)
}

// handleProxyAdminUpdate PUT /api/admin/proxys/update/:hs_name/:vm_uuid/:proxy_index
func (app *App) handleProxyAdminUpdate(c *gin.Context) {
	app.handleProxyUpdate(c)
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
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
	vm, _ := app.loadVMConfig(hsName, vmUUID)
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
	vm, _ := app.loadVMConfig(hsName, vmUUID)
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

	// 取真实磁盘配置（含设备位 / 卷标识），否则平台驱动无法精确定位磁盘
	vm, err := app.loadVMConfig(hsName, vmUUID)
	if err != nil || vm == nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	hddConf, ok := vm.HddAll[req.HddName]
	if !ok {
		utils.BadRequest(c, "待移交磁盘不存在")
		return
	}
	hdd := hddConf

	// 移交前置校验（对应 Python 版 BasicServer.HDDCheck）：
	// 目标虚拟机需存在、磁盘必须处于「未挂载」状态，否则会出现两台虚拟机同时占用同一块盘。
	if check, checkErr := d.HDDCheck(vmUUID, &hdd, req.TargetVM); checkErr != nil {
		utils.InternalError(c, "移交校验失败")
		return
	} else if check != nil && !check.Success {
		utils.Fail(c, 400, check.Message)
		return
	}

	result, err := d.TransferHDD(vmUUID, &hdd, req.TargetVM)
	if err != nil {
		utils.InternalError(c, "移交磁盘失败")
		return
	}
	if result != nil && !result.Success {
		utils.Fail(c, 500, result.Message)
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
	vm, _ := app.loadVMConfig(hsName, vmUUID)
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
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
	vm, _ := app.loadVMConfig(hsName, vmUUID)
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
	vm, _ := app.loadVMConfig(hsName, vmUUID)
	if vm != nil {
		_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	}
	utils.Success(c, result)
}

// handleUSBMount POST /api/client/usb/mount/:hs_name/:vm_uuid
//
// 与 Python mount_vm_usb 一致：接受 {usb_key} 或 {usb_vid, usb_pid, usb_remark}；
// 只给 VID/PID 时由后端生成 usb_key（前端 apis.ts addUSB 发送的正是后者）。
func (app *App) handleUSBMount(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req struct {
		UsbKey    string `json:"usb_key"`
		UsbVid    string `json:"usb_vid"`
		UsbPid    string `json:"usb_pid"`
		UsbRemark string `json:"usb_remark"`
		UsbHint   string `json:"usb_hint"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	usbKey := strings.TrimSpace(req.UsbKey)
	if usbKey == "" {
		if req.UsbVid == "" || req.UsbPid == "" {
			utils.BadRequest(c, "VID和PID不能为空")
			return
		}
		// 与 Python 一致：VID:PID:备注 派生稳定 key，重复挂载同一设备不会产生多条记录
		hint := req.UsbRemark
		if hint == "" {
			hint = req.UsbHint
		}
		usbKey = fmt.Sprintf("%s:%s:%s", req.UsbVid, req.UsbPid, hint)
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	result, err := d.MountUSB(vmUUID, usbKey, true)
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	utils.Success(c, vm.Backups)
}

// backupRequest 备份相关入参。
// 同时接受前端字段（vm_tips / vm_back，见 apis.ts）与内部字段名（tips / backup_name）。
// 此前只认内部字段名，导致「还原/删除备份」必定 400（备份文件名不能为空），
// 创建备份的备注也总是丢失。
type backupRequest struct {
	Tips       string `json:"tips"`
	VMTips     string `json:"vm_tips"`
	BackupName string `json:"backup_name"`
	VMBack     string `json:"vm_back"`
}

// name 归一化备份文件名
func (r backupRequest) name() string {
	if r.BackupName != "" {
		return r.BackupName
	}
	return r.VMBack
}

// tipText 归一化备份备注
func (r backupRequest) tipText() string {
	if r.Tips != "" {
		return r.Tips
	}
	return r.VMTips
}

func (app *App) handleBackupCreate(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req backupRequest
	_ = c.ShouldBindJSON(&req)
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	result, err := d.CreateBackup(vmUUID, req.tipText())
	if err != nil {
		utils.InternalError(c, "创建备份失败")
		return
	}
	utils.Success(c, result)
}

func (app *App) handleBackupRestore(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req backupRequest
	if err := c.ShouldBindJSON(&req); err != nil || req.name() == "" {
		utils.BadRequest(c, "备份文件名不能为空")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	result, err := d.RestoreBackup(vmUUID, req.name())
	if err != nil {
		utils.InternalError(c, "还原备份失败")
		return
	}
	utils.Success(c, result)
}

func (app *App) handleBackupDelete(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	var req backupRequest
	// DELETE 的 body 可能为空，先尝试解析 JSON，再回退到 query
	if err := c.ShouldBindJSON(&req); err != nil {
		req.BackupName = c.Query("backup_name")
		req.VMBack = c.Query("vm_back")
	}
	if req.name() == "" {
		utils.BadRequest(c, "备份文件名不能为空")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	result, err := d.DeleteBackup(vmUUID, req.name())
	if err != nil {
		utils.InternalError(c, "删除备份失败")
		return
	}
	utils.Success(c, result)
}

// ============================================================
// 设备直通 API
// ============================================================

// handleOSImages GET /api/client/os-images/:hs_name
// 返回结构与 Python get_os_images 一致：
//
//	{ host_name, server_type, filter_name, system_maps, images_maps,
//	  ban_init, ban_edit, messages, tab_lock, enable_host, ipaddr_maps, ipaddr_ddns, public_addr }
//
// 前端用它同时获取：虚拟机 UUID 前缀（filter_name）、系统镜像下拉（system_maps）、
// 表单禁用项（ban_init/ban_edit/tab_lock）、平台注意事项（messages）与启用状态（enable_host）。
// 直接读主机配置而非驱动，禁用主机同样可返回（与 Python 行为一致）。
func (app *App) handleOSImages(c *gin.Context) {
	hsName := c.Param("hs_name")

	uc := auth.GetUserContext(c)
	if uc != nil && !uc.IsAdmin && !uc.TempLogin {
		if !service.CheckHostAccess(hsName, uc.UserData) {
			utils.Forbidden(c)
			return
		}
	}

	cfg, err := app.HostSvc.GetHost(hsName)
	if err != nil || cfg == nil {
		utils.NotFound(c, "主机不存在")
		return
	}

	meta, _ := driver.EngineMetaOf(cfg.ServerType)

	utils.Success(c, map[string]interface{}{
		"host_name":   hsName,
		"server_type": cfg.ServerType,
		"filter_name": cfg.FilterName,
		"system_maps": service.OSListToArray(cfg.SystemMaps),
		"images_maps": service.OSListToArray(cfg.ImagesMaps),
		"ban_init":    meta.BanInit,
		"ban_edit":    meta.BanEdit,
		"messages":    meta.Messages,
		"tab_lock":    meta.TabLock,
		"enable_host": cfg.EnableHost,
		"ipaddr_maps": cfg.IPAddrMaps,
		"ipaddr_ddns": cfg.IPAddrDDNS,
		"public_addr": cfg.PublicAddr,
	})
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
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
	vm, err := app.loadVMConfig(hsName, vmUUID)
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
	// 兼容两种下发方式（与 Python update_vm_owner_permission 一致）：
	//   - permission: 16 位整型掩码（前端 updateVMOwnerPermission 发送的数字）
	//   - mask:       位字段对象（Go 内部/接口文档使用）
	var req struct {
		Username   string                 `json:"username"`
		Mask       *model.UserMask        `json:"mask"`
		Permission *int                   `json:"permission"`
		Extra      map[string]interface{} `json:"-"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Username == "" {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	// 归一化出最终掩码
	var mask model.UserMask
	switch {
	case req.Permission != nil:
		mask = model.UserMaskFromBits(*req.Permission)
	case req.Mask != nil:
		mask = *req.Mask
	default:
		utils.BadRequest(c, "权限掩码不能为空")
		return
	}
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := app.loadVMConfig(hsName, vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	if vm.OwnAll == nil {
		vm.OwnAll = map[string]model.UserMask{}
	}
	if _, ok := vm.OwnAll[req.Username]; !ok {
		utils.BadRequest(c, "用户不是所有者")
		return
	}
	vm.OwnAll[req.Username] = mask
	_, _ = d.UpdateVM(vm, vm)
	_ = app.DB.UpsertVMConfig(hsName, vmUUID, vm)
	utils.SuccessMsg(c, "权限更新成功", nil)
}

func (app *App) handleOwnerTransfer(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	// 兼容两套字段名（与 Python transfer_vm_ownership 一致）：
	//   keep_access      —— 前端 keepAccessChecked（Python 使用的名字）
	//   keep_old         —— Go 内部/接口文档使用的名字
	//   confirm_transfer —— 前端二次确认勾选；Python 未勾选直接 400
	var req struct {
		NewOwner        string `json:"new_owner"`
		KeepOld         bool   `json:"keep_old"`
		KeepAccess      bool   `json:"keep_access"`
		ConfirmTransfer *bool  `json:"confirm_transfer"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.NewOwner == "" {
		utils.BadRequest(c, "新所有者不能为空")
		return
	}
	// 前端会显式传 confirm_transfer，未勾选时应拒绝（与 Python 行为一致）
	if req.ConfirmTransfer != nil && !*req.ConfirmTransfer {
		utils.BadRequest(c, "必须确认移交所有权")
		return
	}
	keepOld := req.KeepOld || req.KeepAccess
	d := app.HostSvc.GetDriver(hsName)
	if d == nil {
		utils.NotFound(c, "主机不存在")
		return
	}
	vm, err := app.loadVMConfig(hsName, vmUUID)
	if err != nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}

	// 重建所有者map，新所有者排第一
	newOwners := map[string]model.UserMask{
		req.NewOwner: model.FullUserMask(),
	}
	if keepOld {
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
