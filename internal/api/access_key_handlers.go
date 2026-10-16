// Package api 虚拟机访问密钥 API
//
// 访问密钥（16 位大小写字母 + 数字）是绑定到单台虚拟机的长期凭据，提供三个入口：
//   - 登录页「访问密钥登录」：POST /api/login {login_type:"access_key", access_key:"..."}
//   - URL 带密钥跳转：GET /api/client/keylogin?key=xxx（可选 #k=xxx 中间页模式）
//   - 密钥管理：GET/POST/DELETE /api/client/access-key/:hs_name/:vm_uuid
//
// 三个入口登录后建立的会话与「临时凭据」（/api/client/temptoken + /api/client/templogin）
// 完全一致：同一个虚拟用户、同一个权限掩码（35287），且仅能访问签发时绑定的主机与虚拟机。
package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/auth"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/service"
	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
)

// ============================================================
// 权限与公共辅助
// ============================================================

// canManageVMAccessKey 判断当前用户是否可查看/生成/撤销指定虚拟机的访问密钥。
//
// 访问密钥等价于该虚拟机的长期登录凭据，且持有的会话权限可能高于受限共享者，
// 因此仅放行管理员与拥有完整权限的主所有者，避免低权限共享者借此提权。
func (app *App) canManageVMAccessKey(uc *auth.UserContext, vm *model.VMConfig) bool {
	if uc == nil || vm == nil {
		return false
	}
	if uc.IsAdmin || uc.IsToken {
		return true
	}
	if uc.TempLogin || vm.OwnAll == nil {
		return false
	}
	mask, ok := vm.OwnAll[uc.Username]
	if !ok {
		return false
	}
	return mask.ToBits() == model.FullUserMask().ToBits()
}

// tempLoginUserInfo 构造受限登录（临时凭据 / 访问密钥）的用户信息。
// 字段与 Python 版 UserManager.build_temp_user_response 一致，
// 前端持久化后即可直接进入虚拟机页面。
func tempLoginUserInfo(username, hsName, vmUUID string) map[string]interface{} {
	info := map[string]interface{}{
		"id":             0,
		"username":       username,
		"email":          "",
		"is_admin":       false,
		"is_active":      true,
		"is_token_login": false,
		"temp_login":     true,
		"temp_hs_name":   hsName,
		"temp_vm_uuid":   vmUUID,
		"assigned_hosts": []string{hsName},
		// 受限身份不具备用户级创建/删除权限
		"can_create_vm": false,
		"can_modify_vm": false,
		"can_delete_vm": false,
	}
	for _, key := range []string{
		"quota_cpu", "quota_ram", "quota_ssd", "quota_gpu",
		"quota_nat_ports", "quota_web_proxy", "quota_nat_ips", "quota_pub_ips",
		"quota_bandwidth_up", "quota_bandwidth_down", "quota_traffic",
		"used_cpu", "used_ram", "used_ssd", "used_gpu",
		"used_nat_ports", "used_web_proxy", "used_nat_ips", "used_pub_ips",
		"used_bandwidth_up", "used_bandwidth_down", "used_traffic",
	} {
		info[key] = 0
	}
	return info
}

// vmConsolePath 返回虚拟机管理页面的前端路由（与 Python 版跳转目标一致）。
func vmConsolePath(hsName, vmUUID string) string {
	return "/hosts/" + url.PathEscape(hsName) + "/vms/" + url.PathEscape(vmUUID)
}

// startVMLimitedSession 建立仅允许访问单台虚拟机的受限会话。
//
// 会话形态与 AuthMiddleware 的 temp_login 分支严格对应：
// 用户名使用虚拟用户（{hs_name}-{vm_uuid}），并记录主机/虚拟机范围，
// HostVMAccessMiddleware 会据此拒绝该会话访问其它资源。
func (app *App) startVMLimitedSession(c *gin.Context, hsName, vmUUID, virtualUser string) error {
	session := sessions.Default(c)
	session.Set(auth.SessionKeyUser, virtualUser)
	session.Set(auth.SessionKeyTempLogin, true)
	session.Set(auth.SessionKeyTempHost, hsName)
	session.Set(auth.SessionKeyTempVM, vmUUID)
	return session.Save()
}

// renderVMLoginRedirect 输出跳转页：先写入前端持久化状态（localStorage 的 user-storage），
// 再跳转到目标虚拟机页面。
//
// 前端 zustand 的登录态来自 localStorage，仅设置 Session Cookie 无法通过
// ProtectedRoute 校验（会被重定向回登录页），因此必须同时注入前端状态。
func (app *App) renderVMLoginRedirect(c *gin.Context, hsName, vmUUID, virtualUser string) {
	target := vmConsolePath(hsName, vmUUID)

	userStorage := map[string]interface{}{
		"state": map[string]interface{}{
			"user":            tempLoginUserInfo(virtualUser, hsName, vmUUID),
			"token":           nil,
			"isAuthenticated": true,
		},
		"version": 0,
	}
	storageJSON, err := json.Marshal(userStorage)
	if err != nil {
		storageJSON = []byte(`{"state":{"user":null,"token":null,"isAuthenticated":false},"version":0}`)
	}

	// 通过 JSON 序列化生成 JS 字符串字面量，避免主机名/标识中的特殊字符破坏脚本
	targetJS, _ := json.Marshal(target)
	storageJS, _ := json.Marshal(string(storageJSON))

	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>登录中...</title>
</head>
<body>
    <h3>登录成功，正在跳转到虚拟机控制台...</h3>
    <p>如果未自动跳转，请<a href="%s">点击这里</a></p>
    <script>
    try {
        localStorage.setItem('user-storage', %s);
        localStorage.removeItem('token');
        sessionStorage.removeItem('token');
    } catch (e) {}
    window.location.replace(%s);
    </script>
</body>
</html>`, target, storageJS, targetJS)

	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(http.StatusOK, html)
}

// renderVMLoginError 输出纯 HTML 错误页（浏览器直接访问 URL 时不会看到 JSON 结构）。
func renderVMLoginError(c *gin.Context, status int, msg string) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.String(status, `<!DOCTYPE html><html><head><meta charset="utf-8"><title>登录失败</title></head><body><h3>`+msg+`</h3></body></html>`)
}

// vmAccessKeyPayload 组装访问密钥接口的统一响应体。
func (app *App) vmAccessKeyPayload(hsName, vmUUID, accessKey string, enabled bool) gin.H {
	payload := gin.H{
		"hs_name":    hsName,
		"vm_uuid":    vmUUID,
		"enabled":    enabled,
		"access_key": "",
		"login_url":  "",
		"length":     service.AccessKeyLength,
	}
	if !enabled || accessKey == "" {
		return payload
	}
	payload["access_key"] = accessKey
	payload["login_url"] = fmt.Sprintf("/api/client/keylogin?key=%s", url.QueryEscape(accessKey))
	return payload
}

// ============================================================
// 访问密钥管理接口
// ============================================================

// handleVMGetAccessKey 查询虚拟机访问密钥（未启用时返回 enabled=false）。
// GET /api/client/access-key/:hs_name/:vm_uuid
func (app *App) handleVMGetAccessKey(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	uc := auth.GetUserContext(c)

	vm := app.HostSvc.MustGetVMConfig(hsName, vmUUID)
	if vm == nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	if !app.canManageVMAccessKey(uc, vm) {
		utils.Fail(c, 403, "仅管理员或主所有者可管理访问密钥")
		return
	}

	key, enabled, err := app.AccessKeySvc.Get(hsName, vmUUID)
	if err != nil {
		utils.InternalError(c, "读取访问密钥失败: "+err.Error())
		return
	}
	utils.Success(c, app.vmAccessKeyPayload(hsName, vmUUID, key, enabled))
}

// handleVMGenerateAccessKey 生成（或重置）虚拟机访问密钥。
// POST /api/client/access-key/:hs_name/:vm_uuid
//
// 重置后旧密钥立即失效，因此仅在显式调用时生成，不提供「读取即生成」的隐式语义。
func (app *App) handleVMGenerateAccessKey(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	uc := auth.GetUserContext(c)

	// 与 temptoken 一致：标识会参与用户名拼接与日志，先做格式校验
	if !vmUUIDLooksSafe(vmUUID) {
		utils.BadRequest(c, "虚拟机标识无效")
		return
	}
	if !app.HostSvc.VMExists(hsName, vmUUID) {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	vm := app.HostSvc.MustGetVMConfig(hsName, vmUUID)
	if !app.canManageVMAccessKey(uc, vm) {
		utils.Fail(c, 403, "仅管理员或主所有者可管理访问密钥")
		return
	}

	createdBy := ""
	if uc != nil {
		createdBy = uc.Username
	}
	key, err := app.AccessKeySvc.Reset(hsName, vmUUID, createdBy)
	if err != nil {
		utils.InternalError(c, "生成访问密钥失败: "+err.Error())
		return
	}

	// 同步补齐虚拟用户，保证密钥登录后的权限与临时凭据一致
	if _, ensureErr := app.HostSvc.EnsureVMTempVirtualUser(hsName, vmUUID); ensureErr != nil {
		app.Logger.Log(hsName, "WARN", "访问密钥",
			fmt.Sprintf("[访问密钥] 补齐虚拟用户失败 %s/%s: %v", hsName, vmUUID, ensureErr), false)
	}

	app.Logger.Log("accesskey", "INFO", "访问密钥",
		fmt.Sprintf("[访问密钥] 已生成访问密钥: %s/%s", hsName, vmUUID), true)

	utils.SuccessMsg(c, "访问密钥生成成功", app.vmAccessKeyPayload(hsName, vmUUID, key, true))
}

// handleVMDeleteAccessKey 撤销虚拟机访问密钥。
// DELETE /api/client/access-key/:hs_name/:vm_uuid
func (app *App) handleVMDeleteAccessKey(c *gin.Context) {
	hsName := c.Param("hs_name")
	vmUUID := c.Param("vm_uuid")
	uc := auth.GetUserContext(c)

	vm := app.HostSvc.MustGetVMConfig(hsName, vmUUID)
	if vm == nil {
		utils.NotFound(c, "虚拟机不存在")
		return
	}
	if !app.canManageVMAccessKey(uc, vm) {
		utils.Fail(c, 403, "仅管理员或主所有者可管理访问密钥")
		return
	}

	removed, err := app.AccessKeySvc.Revoke(hsName, vmUUID)
	if err != nil {
		utils.InternalError(c, "撤销访问密钥失败: "+err.Error())
		return
	}
	app.Logger.Log("accesskey", "INFO", "访问密钥",
		fmt.Sprintf("[访问密钥] 已撤销访问密钥: %s/%s", hsName, vmUUID), true)

	msg := "访问密钥未启用"
	if removed {
		msg = "访问密钥已撤销"
	}
	utils.SuccessMsg(c, msg, app.vmAccessKeyPayload(hsName, vmUUID, "", false))
}

// ============================================================
// URL 带密钥跳转
// ============================================================

// handleAccessKeyLogin 处理 /api/login 的「访问密钥」登录分支。
//
// 登录成功后返回 redirect（虚拟机管理页面路由）与受限用户信息，
// 前端据此直接进入该虚拟机的管理页面，无需再选择主机/虚拟机。
func (app *App) handleAccessKeyLogin(c *gin.Context, ip, accessKey string) {
	if accessKey == "" {
		utils.BadRequest(c, "请输入访问密钥")
		return
	}
	if !service.IsValidAccessKey(accessKey) {
		auth.RecordLoginFailure(ip)
		utils.Fail(c, 401, "访问密钥格式不正确（应为 16 位字母或数字）")
		return
	}

	hsName, vmUUID, ok := app.AccessKeySvc.Resolve(accessKey)
	if !ok || !app.HostSvc.VMExists(hsName, vmUUID) {
		auth.RecordLoginFailure(ip)
		utils.Fail(c, 401, "访问密钥无效或已失效")
		return
	}

	virtualUser, err := app.HostSvc.EnsureVMTempVirtualUser(hsName, vmUUID)
	if err != nil {
		app.Logger.Log("accesskey", "ERROR", "访问密钥",
			fmt.Sprintf("[访问密钥] 准备虚拟用户失败 %s/%s: %v", hsName, vmUUID, err), false)
		utils.Fail(c, 500, "登录失败: "+err.Error())
		return
	}

	if err := app.startVMLimitedSession(c, hsName, vmUUID, virtualUser); err != nil {
		app.Logger.Log("accesskey", "ERROR", "访问密钥", fmt.Sprintf("[访问密钥] 保存会话失败: %v", err), false)
		utils.InternalError(c, "Session保存失败，请重试")
		return
	}

	auth.ResetLoginAttempts(ip)
	app.Logger.Log("accesskey", "INFO", "访问密钥",
		fmt.Sprintf("[访问密钥] 使用访问密钥登录: %s/%s", hsName, vmUUID), true)

	utils.Success(c, gin.H{
		"redirect":      vmConsolePath(hsName, vmUUID),
		"user_info":     tempLoginUserInfo(virtualUser, hsName, vmUUID),
		"is_temp_login": true,
		"temp_login":    true,
		"is_token":      false,
		"hs_name":       hsName,
		"vm_uuid":       vmUUID,
	})
}

// handleVMAccessKeyLogin 使用 URL 携带的访问密钥直接登录并跳转到虚拟机管理页面。
//
//	GET  /api/client/keylogin?key=xxxx        —— 直链，密钥会进入服务端访问日志
//	POST /api/client/keylogin {"key":"xxxx"}  —— 密钥在请求体中，不写入访问日志
//	GET  /api/client/keylogin                 —— 返回中间页，由 JS 从 #k=xxx 读取后 POST，
//	                                             服务端日志中永远看不到密钥
func (app *App) handleVMAccessKeyLogin(c *gin.Context) {
	ip := auth.GetClientIP(c)
	// 与登录页共用同一套 IP 限流：URL 携带的密钥同样是可被枚举的凭据，
	// 不限制尝试次数会让暴力破解有可乘之机。
	if auth.CheckLoginLimit(ip) {
		renderVMLoginError(c, http.StatusTooManyRequests, "尝试次数过多，请300秒后再试")
		return
	}

	key := strings.TrimSpace(c.Query("key"))
	if key == "" {
		key = strings.TrimSpace(c.Query("access_key"))
	}
	if key == "" && c.Request.Method == http.MethodPost {
		var req struct {
			Key       string `json:"key"`
			AccessKey string `json:"access_key"`
		}
		_ = c.ShouldBindJSON(&req)
		key = strings.TrimSpace(req.Key)
		if key == "" {
			key = strings.TrimSpace(req.AccessKey)
		}
	}

	if key == "" {
		// 无密钥：返回中间页，由浏览器从 URL 片段（#k=xxx）读取后 POST 自身，
		// 避免密钥出现在服务端访问日志与 Referer 中。
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
  var key = params['k'] || params['key'];
  if (!key) { document.body.innerText = '缺少访问密钥'; return; }
  fetch('/api/client/keylogin', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ access_key: key })
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

	if !service.IsValidAccessKey(key) {
		auth.RecordLoginFailure(ip)
		renderVMLoginError(c, http.StatusBadRequest, "访问密钥格式不正确（应为 16 位字母或数字）")
		return
	}

	hsName, vmUUID, ok := app.AccessKeySvc.Resolve(key)
	if !ok {
		auth.RecordLoginFailure(ip)
		renderVMLoginError(c, http.StatusUnauthorized, "访问密钥无效或已失效")
		return
	}
	if !app.HostSvc.VMExists(hsName, vmUUID) {
		renderVMLoginError(c, http.StatusNotFound, "访问密钥对应的虚拟机不存在")
		return
	}

	virtualUser, err := app.HostSvc.EnsureVMTempVirtualUser(hsName, vmUUID)
	if err != nil {
		app.Logger.Log("accesskey", "ERROR", "访问密钥",
			fmt.Sprintf("[访问密钥] 准备虚拟用户失败 %s/%s: %v", hsName, vmUUID, err), false)
		renderVMLoginError(c, http.StatusInternalServerError, "登录失败，请稍后重试")
		return
	}

	if err := app.startVMLimitedSession(c, hsName, vmUUID, virtualUser); err != nil {
		app.Logger.Log("accesskey", "ERROR", "访问密钥",
			fmt.Sprintf("[访问密钥] 保存会话失败: %v", err), false)
		renderVMLoginError(c, http.StatusInternalServerError, "登录失败，请稍后重试")
		return
	}

	auth.ResetLoginAttempts(ip)
	app.Logger.Log("accesskey", "INFO", "访问密钥",
		fmt.Sprintf("[访问密钥] 使用访问密钥登录: %s/%s", hsName, vmUUID), true)

	app.renderVMLoginRedirect(c, hsName, vmUUID, virtualUser)
}
