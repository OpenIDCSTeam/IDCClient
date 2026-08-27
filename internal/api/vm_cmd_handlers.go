// Package api 虚拟机命令执行和临时凭据 API 处理器
package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// handleVMCmdSend 下发命令到虚拟机
// POST /api/client/cmd_send/:hs_name/:vm_uuid
// Body: {"command": "echo hello", "timeout": 60}
func (app *App) handleVMCmdSend(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")

	// 检查主机访问权限
	username := c.GetString("username")
	if !app.HostSvc.CheckHostAccess(username, hsName) {
		utils.Forbidden(c)
		return
	}

	// 检查虚拟机所有权
	if !app.HostSvc.CheckVMOwnership(username, hsName, vmUUID) {
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
	var result map[string]interface{}
	if err := c.ShouldBindJSON(&result); err != nil {
		utils.BadRequest(c, "无效的结果数据")
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

	// 检查主机访问权限
	username := c.GetString("username")
	if !app.HostSvc.CheckHostAccess(username, hsName) {
		utils.Forbidden(c)
		return
	}

	// 检查虚拟机所有权
	if !app.HostSvc.CheckVMOwnership(username, hsName, vmUUID) {
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

	// 检查虚拟机是否存在
	if !app.HostSvc.VMExists(hsName, vmUUID) {
		utils.NotFound(c, "虚拟机不存在")
		return
	}

	// 生成虚拟用户名（temp_用户名_时间戳）
	virtualUser := fmt.Sprintf("temp_%s_%d", vmUUID[:8], time.Now().Unix())

	// 生成临时令牌（5分钟有效期）
	token := app.TempTokenStore.Generate(hsName, vmUUID, virtualUser)

	app.Logger.Log("temptoken", "INFO", "临时凭据", fmt.Sprintf("[临时凭据] 已生成临时令牌: %s/%s", hsName, vmUUID), true)

	utils.SuccessMsg(c, "临时凭据生成成功", gin.H{
		"token":       token,
		"hs_name":     hsName,
		"vm_uuid":     vmUUID,
		"expire_in":   300, // 秒
		"login_url":   fmt.Sprintf("/api/client/templogin?token=%s", token),
	})
}

// handleVMTempLogin 临时凭据登录跳转
// GET /api/client/templogin?token=xxx
// POST /api/client/templogin Body: {"token": "xxx"}
func (app *App) handleVMTempLogin(c *gin.Context) {
	// 支持 GET 和 POST
	token := c.Query("token")
	if token == "" && c.Request.Method == "POST" {
		var req struct {
			Token string `json:"token"`
		}
		if err := c.ShouldBindJSON(&req); err == nil {
			token = req.Token
		}
	}

	if token == "" {
		c.HTML(http.StatusBadRequest, "", "<h3>缺少 token 参数</h3>")
		return
	}

	// 消费临时令牌（一次性）
	hsName, vmUUID, virtualUser, ok := app.TempTokenStore.Consume(token)
	if !ok {
		c.HTML(http.StatusUnauthorized, "", "<h3>临时凭据无效或已过期</h3>")
		return
	}

	app.Logger.Log("temptoken", "INFO", "临时凭据", fmt.Sprintf("[临时凭据] 临时用户 %s 登录: %s/%s", virtualUser, hsName, vmUUID), true)

	// 创建临时会话
	session := sessions.Default(c)
	session.Set("username", virtualUser)
	session.Set("is_admin", false)
	session.Set("temp_login", true)
	session.Set("temp_hs_name", hsName)
	session.Set("temp_vm_uuid", vmUUID)
	if err := session.Save(); err != nil {
		app.Logger.Log("temptoken", "ERROR", "临时凭据", fmt.Sprintf("[临时凭据] 保存会话失败: %v", err), false)
		c.HTML(http.StatusInternalServerError, "", "<h3>登录失败</h3>")
		return
	}

	// 跳转到虚拟机控制台
	redirectURL := fmt.Sprintf("/#/console?hs_name=%s&vm_uuid=%s", hsName, vmUUID)
	
	// 返回 HTML 跳转页面
	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>登录中...</title>
    <script>
        window.location.href = '%s';
    </script>
</head>
<body>
    <h3>登录成功，正在跳转到虚拟机控制台...</h3>
    <p>如果未自动跳转，请<a href="%s">点击这里</a></p>
</body>
</html>`, redirectURL, redirectURL)
	
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, html)
}

// min 返回两个整数中的较小值
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
