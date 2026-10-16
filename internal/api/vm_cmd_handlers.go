// Package api 虚拟机命令执行和临时凭据 API 处理器
package api

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/auth"
	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// handleVMCmdSend 下发命令到虚拟机
// POST /api/client/cmd_send/:hs_name/:vm_uuid
// Body: {"command": "echo hello", "timeout": 60}
func (app *App) handleVMCmdSend(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")

	// 授权已由 HostVMAccessMiddleware 统一执行，这里按统一规则再复核一次
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}
	if !app.canOperateVM(uc, hsName, vmUUID) {
		utils.Fail(c, 403, "无权操作此虚拟机")
		return
	}

	var req struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Command == "" {
		utils.BadRequest(c, "命令不能为空")
		return
	}
	if req.Timeout == 0 {
		req.Timeout = 60
	}

	// 生成命令ID
	cmdID := uuid.New().String()

	// 构造命令数据
	cmdData := map[string]interface{}{
		"cmd_id":  cmdID,
		"command": req.Command,
		"timeout": req.Timeout,
	}

	// 存储到虚拟机配置
	if err := app.HostSvc.SetVMCmd(hsName, vmUUID, cmdData); err != nil {
		utils.InternalError(c, "下发命令失败: "+err.Error())
		return
	}

	// 记录日志
	app.Logger.Log("cmd", "INFO", "远程命令", fmt.Sprintf("[远程命令] 已下发命令到 %s/%s: %s", hsName, vmUUID, req.Command[:min(50, len(req.Command))]), true)

	utils.SuccessMsg(c, "命令已下发，等待虚拟机握手执行", gin.H{
		"cmd_id":  cmdID,
		"command": req.Command,
		"timeout": req.Timeout,
		"status":  "pending",
	})
}

// handleVMCmdResult 接收命令执行结果（CloudInit回传，无需认证）
// POST /api/client/cmd_result
// Body: {"cmd_id": "...", "command": "...", "success": true, "exit_code": 0, ...}
func (app *App) handleVMCmdResult(c *gin.Context) {
	// 公开接口：限制请求体大小，避免被用于内存放大
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256*1024)

	var result map[string]interface{}
	if err := c.ShouldBindJSON(&result); err != nil {
		utils.BadRequest(c, "无效的结果数据或请求体过大")
		return
	}

	cmdID, ok := result["cmd_id"].(string)
	if !ok || cmdID == "" {
		utils.BadRequest(c, "缺少cmd_id参数")
		return
	}

	command, _ := result["command"].(string)
	success, _ := result["success"].(bool)
	exitCode, _ := result["exit_code"].(float64)

	app.Logger.Log("cmd", "INFO", "远程命令", fmt.Sprintf("[远程命令] 收到命令执行结果: cmd_id=%s, success=%v", cmdID, success), success)

	// 根据 cmd_id 匹配虚拟机并存储结果
	hsName, vmUUID, found := app.HostSvc.FindVMByCmdID(cmdID)
	if !found {
		app.Logger.Log("cmd", "WARNING", "远程命令", fmt.Sprintf("[远程命令] 未找到cmd_id=%s对应的虚拟机，结果丢弃", cmdID), false)
		utils.NotFound(c, "未找到对应的虚拟机")
		return
	}

	// 保存结果到vm_cmd_result
	if err := app.HostSvc.SetVMCmdResult(hsName, vmUUID, result); err != nil {
		app.Logger.Log("cmd", "ERROR", "远程命令", fmt.Sprintf("[远程命令] 保存结果失败: %v", err), false)
		utils.InternalError(c, "保存结果失败")
		return
	}

	// 记录系统日志
	logLevel := "INFO"
	if !success {
		logLevel = "ERROR"
	}
	logMessage := fmt.Sprintf("虚拟机 %s 命令执行%s: %s", vmUUID, map[bool]string{true: "成功", false: "失败"}[success], command[:min(100, len(command))])
	
	app.Logger.Log(hsName, logLevel, "远程命令执行", logMessage, success)

	app.Logger.Log("cmd", "INFO", "远程命令", fmt.Sprintf("[远程命令] 结果已存储: %s/%s, 退出码=%.0f", hsName, vmUUID, exitCode), true)
	utils.SuccessMsg(c, "命令结果已接收", nil)
}

// handleVMCmdStatus 获取命令执行状态
// GET /api/client/cmd_status/:hs_name/:vm_uuid
func (app *App) handleVMCmdStatus(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")

	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}
	if !app.canOperateVM(uc, hsName, vmUUID) {
		utils.Fail(c, 403, "无权操作此虚拟机")
		return
	}

	pendingCmd, lastResult, err := app.HostSvc.GetVMCmdStatus(hsName, vmUUID)
	if err != nil {
		utils.NotFound(c, err.Error())
		return
	}

	utils.SuccessMsg(c, "获取成功", gin.H{
		"pending_cmd": pendingCmd,
		"last_result": lastResult,
	})
}

// handleVMTempToken 生成临时访问凭据
// GET /api/client/temptoken/:hs_name/:vm_uuid
// 需要 Bearer Token 认证（用于外部系统集成）
//
// 响应同时兼容两套字段命名（历史遗留的外部插件/自动化插件都在使用）：
//   - Python 版：temp_token / expire（FSPlugins/OpenIDC-IDCSmart、OpenIDC-SwapIDC）
//   - Go 版旧字段：token / expire_in / login_url
//
// 虚拟用户名固定为 {hs_name}-{vm_uuid}，与 Python 版一致：
// 前端 user_permissions 直接读取 own_all[虚拟用户]，命名不一致会拿到 0 权限。
func (app *App) handleVMTempToken(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")

	// 检查 Bearer Token
	authHeader := c.GetHeader("Authorization")
	systemToken := app.SystemSvc.GetToken()
	if authHeader != "Bearer "+systemToken {
		utils.Unauthorized(c)
		return
	}

	// 校验标识格式：该标识会参与用户名拼接与日志记录
	if !vmUUIDLooksSafe(vmUUID) {
		utils.BadRequest(c, "虚拟机标识无效")
		return
	}

	// 检查虚拟机是否存在（注意：配置缺失时必须返回不存在）
	if !app.HostSvc.VMExists(hsName, vmUUID) {
		utils.NotFound(c, "虚拟机不存在")
		return
	}

	// 虚拟用户名与访问密钥登录保持一致，并确保 own_all 中存在对应权限掩码
	virtualUser, err := app.HostSvc.EnsureVMTempVirtualUser(hsName, vmUUID)
	if err != nil {
		utils.NotFound(c, err.Error())
		return
	}

	// 生成临时令牌（5分钟有效期，一次性使用）
	token, expire := app.TempTokenStore.GenerateWithExpire(hsName, vmUUID, virtualUser)

	app.Logger.Log("temptoken", "INFO", "临时凭据", fmt.Sprintf("[临时凭据] 已生成临时令牌: %s/%s", hsName, vmUUID), true)

	loginURL := fmt.Sprintf("/api/client/templogin?token=%s", url.QueryEscape(token))
	utils.SuccessMsg(c, "临时凭据生成成功", gin.H{
		// Python 兼容字段（外部插件读取 temp_token / expire）
		"temp_token": token,
		"expire":     expire.Unix(),
		// Go 版历史字段，保持向后兼容
		"token":     token,
		"expire_in": int(time.Until(expire).Seconds()),
		"login_url": loginURL,
		"jump_url":  loginURL,
		"hs_name":   hsName,
		"vm_uuid":   vmUUID,
	})
}

// handleVMTempLogin 临时凭据登录跳转
//
//	GET  /api/client/templogin?token=xxx      —— 直链（token 会进入访问日志）
//	POST /api/client/templogin {"token":""}   —— token 在请求体中，不写入访问日志
//	GET  /api/client/templogin                —— 返回中间页，由 JS 从 #t=xxx 读取后 POST
func (app *App) handleVMTempLogin(c *gin.Context) {
	token := c.Query("token")
	if token == "" && c.Request.Method == http.MethodPost {
		var req struct {
			Token string `json:"token"`
		}
		if err := c.ShouldBindJSON(&req); err == nil {
			token = req.Token
		}
	}

	if token == "" {
		// 无 token：返回 hash 中间页，token 由 JS 从 location.hash 读取后 POST，
		// 避免一次性凭据出现在服务端访问日志与浏览器历史中。
		landing := `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>正在跳转...</title></head>
<body>
<script>
(function () {
  var params = {};
  location.hash.replace(/^#/, '').split('&').forEach(function (kv) {
    var p = kv.split('=');
    if (p[0]) { params[p[0]] = decodeURIComponent(p[1] || ''); }
  });
  var token = params['t'] || params['token'];
  if (!token) { document.body.innerText = '缺少临时凭据'; return; }
  fetch('/api/client/templogin', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token: token })
  }).then(function (r) { return r.text(); })
    .then(function (html) { document.open(); document.write(html); document.close(); })
    .catch(function (e) { document.body.innerText = '登录失败: ' + e; });
})();
</script>
</body>
</html>`
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, landing)
		return
	}

	// 消费临时令牌（一次性）
	hsName, vmUUID, virtualUser, ok := app.TempTokenStore.Consume(token)
	if !ok {
		renderVMLoginError(c, http.StatusUnauthorized, "临时凭据无效或已过期")
		return
	}
	if !app.HostSvc.VMExists(hsName, vmUUID) {
		renderVMLoginError(c, http.StatusNotFound, "临时凭据对应的虚拟机不存在")
		return
	}

	app.Logger.Log("temptoken", "INFO", "临时凭据", fmt.Sprintf("[临时凭据] 临时用户 %s 登录: %s/%s", virtualUser, hsName, vmUUID), true)

	// 建立受限会话并跳转到虚拟机管理页面（与访问密钥登录共用同一套逻辑）
	if err := app.startVMLimitedSession(c, hsName, vmUUID, virtualUser); err != nil {
		app.Logger.Log("temptoken", "ERROR", "临时凭据", fmt.Sprintf("[临时凭据] 保存会话失败: %v", err), false)
		renderVMLoginError(c, http.StatusInternalServerError, "登录失败")
		return
	}

	app.renderVMLoginRedirect(c, hsName, vmUUID, virtualUser)
}

// min 返回两个整数中的较小值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
