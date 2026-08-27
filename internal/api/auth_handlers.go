// Package api 认证相关处理器
package api

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/auth"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
)

// handleLogin POST /api/login
func (app *App) handleLogin(c *gin.Context) {
	ip := auth.GetClientIP(c)

	// 检查IP限流
	if auth.CheckLoginLimit(ip) {
		utils.TooManyRequests(c, "登录失败次数过多，请300秒后再试")
		return
	}

	var req struct {
		LoginType string `json:"login_type"`
		Token     string `json:"token"`
		Username  string `json:"username"`
		Password  string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}

	var userInfo map[string]interface{}
	var isTokenLogin bool

	if req.LoginType == "token" {
		// Token登录
		sysToken := app.SystemSvc.GetToken()
		if req.Token == "" || req.Token != sysToken {
			auth.RecordLoginFailure(ip)
			utils.Fail(c, 401, "Token无效")
			return
		}
		isTokenLogin = true
		userInfo = map[string]interface{}{
			"id": 1, "username": "admin", "is_admin": true,
			"assigned_hosts": []string{},
		}
	} else {
		// 用户名密码登录
		if req.Username == "" || req.Password == "" {
			utils.BadRequest(c, "用户名和密码不能为空")
			return
		}
		user, err := app.DB.GetUserByUsername(req.Username)
		if err != nil || user == nil {
			auth.RecordLoginFailure(ip)
			utils.Fail(c, 401, "用户名或密码错误")
			return
		}
		// 检查账号是否启用
		if !model.SafeGetBool(user, "is_active") {
			utils.Fail(c, 401, "账号已被禁用")
			return
		}
		// 检查邮箱验证状态
		settings, _ := app.SystemSvc.GetSettings()
		if settings["email_verification_enabled"] == "1" {
			if !model.SafeGetBool(user, "email_verified") {
				utils.Fail(c, 403, "请先验证邮箱后再登录")
				return
			}
		}
		// 验证密码
		password := model.SafeGetString(user, "password")
		if !auth.CheckPassword(req.Password, password) {
			auth.RecordLoginFailure(ip)
			utils.Fail(c, 401, "用户名或密码错误")
			return
		}
		userInfo = user
		// 更新最后登录时间
		_ = app.DB.UpdateUserLastLogin(req.Username)
	}

	// 重置登录失败计数
	auth.ResetLoginAttempts(ip)

	// 设置Session（必须存储string类型，gob序列化interface{}需要注册）
	session := sessions.Default(c)
	username := model.SafeGetString(userInfo, "username")
	session.Set(auth.SessionKeyUser, username)
	if err := session.Save(); err != nil {
		utils.InternalError(c, "Session保存失败，请重试")
		return
	}

	// 构建重定向路径
	redirect := "/dashboard"
	isAdmin, _ := userInfo["is_admin"].(bool)
	if !isAdmin {
		redirect = "/user/dashboard"
	}

	// 构建响应数据
	respData := gin.H{
		"redirect":  redirect,
		"user_info": sanitizeUser(userInfo),
		"is_token":  isTokenLogin,
	}
	// Token登录时，将Bearer Token返回给前端存入localStorage，用于后续认证
	// 这样即使Session Cookie有问题，Bearer Token认证仍可正常工作
	if isTokenLogin {
		respData["token"] = app.SystemSvc.GetToken()
	}

	utils.Success(c, respData)
}

// handleLogout POST /api/logout
func (app *App) handleLogout(c *gin.Context) {
	session := sessions.Default(c)
	session.Clear()
	_ = session.Save()
	utils.SuccessMsg(c, "已退出登录", nil)
}

// handleRegister POST /api/register
func (app *App) handleRegister(c *gin.Context) {
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}

	// 参数验证
	if len(req.Username) < 3 || len(req.Username) > 20 {
		utils.BadRequest(c, "用户名长度必须在3-20字符之间")
		return
	}
	if !isValidEmail(req.Email) {
		utils.BadRequest(c, "邮箱格式不正确")
		return
	}
	if len(req.Password) < 6 {
		utils.BadRequest(c, "密码长度不能少于6字符")
		return
	}

	// 检查用户名是否已存在
	existing, _ := app.DB.GetUserByUsername(req.Username)
	if existing != nil {
		utils.BadRequest(c, "用户名已存在")
		return
	}
	// 检查邮箱是否已存在
	existingEmail, _ := app.DB.GetUserByEmail(req.Email)
	if existingEmail != nil {
		utils.BadRequest(c, "邮箱已被注册")
		return
	}

	// 哈希密码
	hashedPwd, err := auth.HashPassword(req.Password)
	if err != nil {
		utils.InternalError(c, "密码处理失败")
		return
	}

	// 创建用户
	userID, err := app.DB.CreateUser(req.Username, hashedPwd, req.Email, false)
	if err != nil {
		utils.InternalError(c, "创建用户失败")
		return
	}

	// 应用默认配额和权限
	defaultQuotas := app.SystemSvc.GetDefaultQuotas()
	defaultQuotas["is_active"] = 1
	defaultQuotas["verify_token"] = ""
	_ = app.DB.UpdateUser(int(userID), defaultQuotas)

	// 生成邮箱验证Token
	verifyToken, _ := auth.GenerateToken()
	_ = app.DB.UpdateUser(int(userID), map[string]interface{}{
		"verify_token": verifyToken,
	})

	// 发送验证邮件（异步）
	go func() {
		emailSvc := app.SystemSvc.GetEmailService()
		if emailSvc != nil {
			settings, _ := app.SystemSvc.GetSettings()
			baseURL, _ := settings["base_url"].(string)
			if baseURL == "" {
				baseURL = "http://localhost:1880"
			}
			verifyURL := fmt.Sprintf("%s/verify_email?token=%s", baseURL, verifyToken)
			_ = emailSvc.SendVerificationEmail(req.Email, req.Username, verifyURL)
		}
	}()

	utils.SuccessMsg(c, "注册成功，请查收验证邮件", gin.H{"user_id": userID})
}

// handleVerifyEmail GET /verify_email?token=<token>
func (app *App) handleVerifyEmail(c *gin.Context) {
	token := c.Query("token")
	if token == "" {
		utils.BadRequest(c, "验证Token不能为空")
		return
	}
	user, err := app.DB.GetUserByVerifyToken(token)
	if err != nil || user == nil {
		utils.BadRequest(c, "验证Token无效或已过期")
		return
	}
	id := model.SafeGetInt(user, "id")
	_ = app.DB.UpdateUser(id, map[string]interface{}{
		"email_verified": 1,
		"verify_token":   "",
	})
	// 重定向到登录页
	c.Redirect(302, "/login?verified=1")
}

// handleVerifyEmailChange GET /verify-email-change?token=<token>
func (app *App) handleVerifyEmailChange(c *gin.Context) {
	token := c.Query("token")
	if token == "" {
		utils.BadRequest(c, "验证Token不能为空")
		return
	}
	// Token格式: <userID>:<newEmail>:<token>
	parts := strings.SplitN(token, ":", 3)
	if len(parts) != 3 {
		utils.BadRequest(c, "验证Token格式无效")
		return
	}
	user, err := app.DB.GetUserByVerifyToken(token)
	if err != nil || user == nil {
		utils.BadRequest(c, "验证Token无效或已过期")
		return
	}
	id, _ := user["id"].(int)
	newEmail := parts[1]
	_ = app.DB.UpdateUser(id, map[string]interface{}{
		"email":        newEmail,
		"verify_token": "",
	})
	c.Redirect(302, "/profile?email_changed=1")
}

// handleForgotPassword POST /api/forgot-password
func (app *App) handleForgotPassword(c *gin.Context) {
	var req struct {
		Email string `json:"email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Email == "" {
		utils.BadRequest(c, "邮箱不能为空")
		return
	}

	user, _ := app.DB.GetUserByEmail(req.Email)
	// 无论用户是否存在，都返回成功（防止邮箱枚举）
	if user != nil {
		resetToken, _ := auth.GenerateToken()
		id, _ := user["id"].(int)
		_ = app.DB.UpdateUser(id, map[string]interface{}{
			"reset_token": resetToken,
		})
		go func() {
			emailSvc := app.SystemSvc.GetEmailService()
			if emailSvc != nil {
				settings, _ := app.SystemSvc.GetSettings()
				baseURL, _ := settings["base_url"].(string)
				if baseURL == "" {
					baseURL = "http://localhost:1880"
				}
				resetLink := fmt.Sprintf("%s/reset-password?token=%s", baseURL, resetToken)
				username := model.SafeGetString(user, "username")
				_ = emailSvc.SendPasswordResetEmail(req.Email, username, resetLink)
			}
		}()
	}
	utils.SuccessMsg(c, "如果邮箱存在，重置链接已发送", nil)
}

// handleResetPassword POST /api/system/reset-password
func (app *App) handleResetPassword(c *gin.Context) {
	var req struct {
		Token           string `json:"token"`
		NewPassword     string `json:"new_password"`
		ConfirmPassword string `json:"confirm_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	if req.NewPassword != req.ConfirmPassword {
		utils.BadRequest(c, "两次密码不一致")
		return
	}
	if len(req.NewPassword) < 6 {
		utils.BadRequest(c, "密码长度不能少于6字符")
		return
	}

	user, err := app.DB.GetUserByResetToken(req.Token)
	if err != nil || user == nil {
		utils.BadRequest(c, "重置Token无效或已过期")
		return
	}

	hashedPwd, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		utils.InternalError(c, "密码处理失败")
		return
	}
	id := model.SafeGetInt(user, "id")
	_ = app.DB.UpdateUser(id, map[string]interface{}{
		"password":    hashedPwd,
		"reset_token": "",
	})
	utils.SuccessMsg(c, "密码重置成功", nil)
}

// handleChangePassword POST /api/users/change-password
func (app *App) handleChangePassword(c *gin.Context) {
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}
	var req struct {
		NewPassword     string `json:"new_password"`
		ConfirmPassword string `json:"confirm_password"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	if req.NewPassword != req.ConfirmPassword {
		utils.BadRequest(c, "两次密码不一致")
		return
	}
	if len(req.NewPassword) < 6 {
		utils.BadRequest(c, "密码长度不能少于6字符")
		return
	}

	user, _ := app.DB.GetUserByUsername(uc.Username)
	if user == nil {
		utils.NotFound(c, "用户不存在")
		return
	}
	hashedPwd, _ := auth.HashPassword(req.NewPassword)
	id := model.SafeGetInt(user, "id")
	_ = app.DB.UpdateUser(id, map[string]interface{}{"password": hashedPwd})
	utils.SuccessMsg(c, "密码修改成功", nil)
}

// handleChangeEmail POST /api/users/change-email
func (app *App) handleChangeEmail(c *gin.Context) {
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}
	var req struct {
		NewEmail string `json:"new_email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || !isValidEmail(req.NewEmail) {
		utils.BadRequest(c, "邮箱格式不正确")
		return
	}

	// 检查邮箱是否已被使用
	existing, _ := app.DB.GetUserByEmail(req.NewEmail)
	if existing != nil {
		utils.BadRequest(c, "邮箱已被使用")
		return
	}

	user, _ := app.DB.GetUserByUsername(uc.Username)
	if user == nil {
		utils.NotFound(c, "用户不存在")
		return
	}

	// 生成验证Token（格式: <userID>:<newEmail>:<token>）
	tokenPart, _ := auth.GenerateToken()
	id, _ := user["id"].(int)
	verifyToken := fmt.Sprintf("%d:%s:%s", id, req.NewEmail, tokenPart)
	_ = app.DB.UpdateUser(id, map[string]interface{}{"verify_token": verifyToken})

	// 发送验证邮件
	go func() {
		emailSvc := app.SystemSvc.GetEmailService()
		if emailSvc != nil {
			settings, _ := app.SystemSvc.GetSettings()
			baseURL, _ := settings["base_url"].(string)
			if baseURL == "" {
				baseURL = "http://localhost:1880"
			}
			verifyURL := fmt.Sprintf("%s/verify-email-change?token=%s", baseURL, verifyToken)
			_ = emailSvc.SendEmailChangeVerification(req.NewEmail, uc.Username, verifyURL)
		}
	}()
	utils.SuccessMsg(c, "验证邮件已发送到新邮箱", nil)
}

// ============================================================
// 用户管理 API
// ============================================================

// handleUserCurrent GET /api/users/current
func (app *App) handleUserCurrent(c *gin.Context) {
	uc := auth.GetUserContext(c)
	if uc == nil {
		utils.Unauthorized(c)
		return
	}
	user, err := app.DB.GetUserByUsername(uc.Username)
	if err != nil || user == nil {
		utils.NotFound(c, "用户不存在")
		return
	}
	utils.Success(c, sanitizeUser(user))
}

// handleUserList GET /api/users
func (app *App) handleUserList(c *gin.Context) {
	users, err := app.DB.GetAllUsers()
	if err != nil {
		utils.InternalError(c, "获取用户列表失败")
		return
	}
	result := make([]map[string]interface{}, 0, len(users))
	for _, u := range users {
		result = append(result, sanitizeUser(u))
	}
	utils.Success(c, result)
}

// handleUserCreate POST /api/users
func (app *App) handleUserCreate(c *gin.Context) {
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}
	username, _ := req["username"].(string)
	email, _ := req["email"].(string)
	password, _ := req["password"].(string)
	isAdmin, _ := req["is_admin"].(bool)

	if username == "" || email == "" || password == "" {
		utils.BadRequest(c, "用户名、邮箱和密码不能为空")
		return
	}

	hashedPwd, _ := auth.HashPassword(password)
	userID, err := app.DB.CreateUser(username, hashedPwd, email, isAdmin)
	if err != nil {
		utils.InternalError(c, "创建用户失败: "+err.Error())
		return
	}

	// 先应用默认配额作为基础
	defaultQuotas := app.SystemSvc.GetDefaultQuotas()

	// 更新管理员指定的字段（会覆盖默认值中的同名字段）
	updateFields := buildUserUpdateFields(req)
	for k, v := range updateFields {
		defaultQuotas[k] = v
	}
	if len(defaultQuotas) > 0 {
		_ = app.DB.UpdateUser(int(userID), defaultQuotas)
	}

	utils.SuccessMsg(c, "用户创建成功", gin.H{"user_id": userID})
}

// handleUserDetail GET /api/users/:user_id
func (app *App) handleUserDetail(c *gin.Context) {
	id := parseIntParam(c, "user_id")
	if id <= 0 {
		utils.BadRequest(c, "用户ID无效")
		return
	}
	user, err := app.DB.GetUserByID(id)
	if err != nil || user == nil {
		utils.NotFound(c, "用户不存在")
		return
	}
	utils.Success(c, sanitizeUser(user))
}

// handleUserUpdate PUT /api/users/:user_id
func (app *App) handleUserUpdate(c *gin.Context) {
	id := parseIntParam(c, "user_id")
	if id <= 0 {
		utils.BadRequest(c, "用户ID无效")
		return
	}
	var req map[string]interface{}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "请求参数错误")
		return
	}

	// 如果包含密码，先哈希
	if pwd, ok := req["password"].(string); ok && pwd != "" {
		hashedPwd, _ := auth.HashPassword(pwd)
		req["password"] = hashedPwd
	}

	updateFields := buildUserUpdateFields(req)
	if err := app.DB.UpdateUser(id, updateFields); err != nil {
		utils.InternalError(c, "更新用户失败")
		return
	}
	utils.SuccessMsg(c, "用户更新成功", nil)
}

// handleUserDelete DELETE /api/users/:user_id
func (app *App) handleUserDelete(c *gin.Context) {
	id := parseIntParam(c, "user_id")
	if id <= 0 {
		utils.BadRequest(c, "用户ID无效")
		return
	}
	if err := app.DB.DeleteUser(id); err != nil {
		utils.InternalError(c, "删除用户失败")
		return
	}
	utils.SuccessMsg(c, "用户删除成功", nil)
}

// ============================================================
// 辅助函数
// ============================================================

var emailRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

func isValidEmail(email string) bool {
	return emailRegex.MatchString(email)
}

// sanitizeUser 过滤用户敏感字段，并将int类型bool字段转为bool
func sanitizeUser(user map[string]interface{}) map[string]interface{} {
	// 需要转为bool的字段
	boolFields := map[string]bool{
		"is_admin": true, "is_active": true, "email_verified": true,
		"can_create_vm": true, "can_delete_vm": true, "can_modify_vm": true,
	}
	result := make(map[string]interface{})
	for k, v := range user {
		if k == "password" || k == "verify_token" || k == "reset_token" {
			continue
		}
		// int(0/1) -> bool
		if boolFields[k] {
			switch val := v.(type) {
			case int:
				result[k] = val != 0
			case int64:
				result[k] = val != 0
			case float64:
				result[k] = val != 0
			default:
				result[k] = v
			}
			continue
		}
		result[k] = v
	}
	return result
}

// buildUserUpdateFields 从请求构建用户更新字段
func buildUserUpdateFields(req map[string]interface{}) map[string]interface{} {
	allowed := map[string]bool{
		"is_admin": true, "is_active": true, "email_verified": true,
		"can_create_vm": true, "can_delete_vm": true, "can_modify_vm": true,
		"quota_cpu": true, "quota_ram": true, "quota_ssd": true, "quota_gpu": true,
		"quota_nat_ports": true, "quota_web_proxy": true, "quota_nat_ips": true,
		"quota_pub_ips": true, "quota_bandwidth_up": true, "quota_bandwidth_down": true,
		"quota_traffic": true, "assigned_hosts": true, "email": true, "password": true,
	}
	fields := make(map[string]interface{})
	for k, v := range req {
		if !allowed[k] {
			continue
		}
		// assigned_hosts需要序列化为JSON
		if k == "assigned_hosts" {
			data, _ := json.Marshal(v)
			fields[k] = string(data)
			continue
		}
		// bool转int（SQLite存储）
		if b, ok := v.(bool); ok {
			if b {
				fields[k] = 1
			} else {
				fields[k] = 0
			}
			continue
		}
		fields[k] = v
	}
	return fields
}

// parseIntParam 解析整数路径参数
func parseIntParam(c *gin.Context, name string) int {
	val := c.Param(name)
	var id int
	fmt.Sscanf(val, "%d", &id)
	return id
}

// ============================================================
// 国际化 API
// ============================================================

var supportedLanguages = []map[string]string{
	{"code": "zh-cn", "name": "简体中文"},
	{"code": "zh-tw", "name": "繁體中文"},
	{"code": "en-us", "name": "English"},
	{"code": "ja-jp", "name": "日本語"},
	{"code": "ko-kr", "name": "한국어"},
	{"code": "ar-ar", "name": "العربية"},
	{"code": "de-de", "name": "Deutsch"},
	{"code": "es-es", "name": "Español"},
	{"code": "fr-fr", "name": "Français"},
	{"code": "it-it", "name": "Italiano"},
	{"code": "pt-br", "name": "Português"},
}

// handleI18nLanguages GET /api/i18n/languages
func (app *App) handleI18nLanguages(c *gin.Context) {
	languages := app.I18nMgr.GetLanguages()
	utils.Success(c, languages)
}

// handleI18nTranslations GET /api/i18n/translations/:lang_code
func (app *App) handleI18nTranslations(c *gin.Context) {
	langCode := c.Param("lang_code")
	translations := app.I18nMgr.GetTranslations(langCode)
	utils.Success(c, translations)
}

// handleTurnstileConfig GET /api/public/turnstile-config
// 获取Turnstile验证码公开配置（无需认证），仅返回是否启用和站点密钥，不返回密钥
func (app *App) handleTurnstileConfig(c *gin.Context) {
	settings, _ := app.SystemSvc.GetSettings()

	enabled := false
	if v, ok := settings["turnstile_enabled"]; ok {
		switch val := v.(type) {
		case string:
			enabled = val == "1" || val == "true"
		case bool:
			enabled = val
		case float64:
			enabled = val == 1
		}
	}
	siteKey, _ := settings["turnstile_site_key"].(string)

	utils.Success(c, gin.H{
		"enabled":  enabled,
		"site_key": siteKey,
	})
}
