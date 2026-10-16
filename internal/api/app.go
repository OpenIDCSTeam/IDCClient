// Package api HTTP路由与处理器 - 应用容器
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/auth"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/i18n"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/logger"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/service"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/task"
)

// sessionSecretGlobalKey 会话签名密钥在 hs_global 表中的键名
const sessionSecretGlobalKey = "session_secret"

// loadSessionSecret 返回持久化的会话签名密钥。
// 密钥首次启动时随机生成并写入数据库，避免使用源码中的固定常量：
// 固定密钥一旦泄露，任何人都能伪造包含任意用户名的会话 Cookie。
func loadSessionSecret(db *data.DB) []byte {
	if v, err := db.GetGlobal(sessionSecretGlobalKey); err == nil && v != "" {
		if raw, decErr := hex.DecodeString(v); decErr == nil && len(raw) >= 32 {
			return raw
		}
	}

	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// 随机源不可用属于极端异常：退化为进程内确定值，
		// 会话在重启后失效，但不会退化成可预测的公开常量。
		sum := sha256.Sum256([]byte(fmt.Sprintf("openidcs-session-%d", time.Now().UnixNano())))
		log.Printf("[session] 生成会话密钥失败，本次启动使用临时密钥: %v", err)
		return sum[:]
	}

	if err := db.SetGlobal(sessionSecretGlobalKey, hex.EncodeToString(secret)); err != nil {
		// 保存失败时仍使用本次生成的密钥，仅导致重启后需要重新登录。
		log.Printf("[session] 持久化会话密钥失败，本次启动使用临时密钥: %v", err)
	}
	return secret
}

func init() {
	// 注册gob类型，确保Session Cookie能正确序列化/反序列化
	gob.Register("")
	gob.Register(map[string]interface{}{})
	gob.Register([]interface{}{})
	gob.Register([]string{})
}

// currentUsername 返回当前登录用户名（未认证时为空串）。
// 用于业务日志的「操作人」字段，避免日志里只有动作没有责任人。
func currentUsername(c *gin.Context) string {
	if uc := auth.GetUserContext(c); uc != nil {
		return uc.Username
	}
	return ""
}

// clientIPOf 返回请求来源 IP（业务日志的「来源」字段）。
func clientIPOf(c *gin.Context) string {
	return auth.GetClientIP(c)
}

// logEvent 创建带操作人与来源 IP 的业务日志事件。
func (app *App) logEvent(c *gin.Context, module, action string) *logger.Event {
	return app.Logger.Event(module, action).
		User(currentUsername(c)).
		IP(clientIPOf(c))
}

// App 应用容器，持有所有依赖
type App struct {
	DB         *data.DB
	HostSvc    *service.HostService
	SystemSvc  *service.SystemService
	NATService *service.NATService
	// IPAllocSvc 网卡 IP/MAC 自动分配（对应 Python BasicServer.NetCheck）
	// 创建/更新虚拟机前必须执行，否则网卡缺少 IP/MAC，
	// 后续 DHCP/ARP 静态绑定会被跳过
	IPAllocSvc     *service.IPAllocService
	ProxyService   *service.ProxyService
	I18nMgr        *i18n.Manager
	Logger         *logger.Logger
	TaskManager    *task.Manager
	TempTokenStore *service.TempTokenStore
	// AccessKeySvc 虚拟机访问密钥服务（16 位长期凭据，登录/URL 跳转共用）
	AccessKeySvc *service.AccessKeyService
	// QuotaSvc 配额服务：API 与调度器共用同一实例，避免两套互斥量各自为政
	QuotaSvc *service.QuotaService
	// ReconcileSvc 外部资源对账服务（outbox 补偿 + 删除墓碑扫描）
	ReconcileSvc *service.ReconcileService
	// DeleteWorkflow 带墓碑的幂等删除流程
	DeleteWorkflow *service.DeleteVMWorkflow
	Router         *gin.Engine
}

// NewApp 创建应用容器并注册所有路由
func NewApp(db *data.DB, dataDir string) *App {
	app := &App{
		DB:        db,
		HostSvc:   service.NewHostService(db),
		SystemSvc: service.NewSystemService(db),
		Logger:    logger.NewLogger(db, dataDir),
	}
	app.NATService = service.NewNATService(db)
	app.IPAllocSvc = service.NewIPAllocService(db)
	app.ProxyService = service.NewProxyService(db, dataDir)
	// 平台自有下发通道（如青州云）优先：注入驱动解析器（未加载时惰性创建）
	app.NATService.SetDriverResolver(app.HostSvc.GetOrCreateDriver)
	app.ProxyService.SetDriverResolver(app.HostSvc.GetOrCreateDriver)

	// 初始化异步任务管理器（4个工作线程）
	taskMgr := task.NewManager(db, 4)
	// 注入业务日志：任务提交/开始/进度/完成/失败/取消都会落到终端、文件与数据库
	taskMgr.SetLogger(app.Logger)
	if err := taskMgr.Start(); err != nil {
		panic("启动任务管理器失败: " + err.Error())
	}
	app.TaskManager = taskMgr

	// 初始化国际化翻译模块（从 Python HostConfig/translates 目录加载 .po 文件）
	i18nMgr, err := i18n.NewManager("../../HostAgent/HostConfig/translates")
	if err != nil {
		i18nMgr, _ = i18n.NewManager("./HostConfig/translates")
	}
	app.I18nMgr = i18nMgr

	// 初始化临时凭据存储
	app.TempTokenStore = service.NewTempTokenStore()

	// 初始化虚拟机访问密钥服务
	app.AccessKeySvc = service.NewAccessKeyService(db)

	// 初始化配额服务（与调度器共享同一实例）
	app.QuotaSvc = service.NewQuotaService(db)

	// 外部资源对账：无法随本地事务一起提交的外部调用改为持久化重试
	app.ReconcileSvc = service.NewReconcileService(db)
	app.ReconcileSvc.SetNATService(app.NATService)
	app.ReconcileSvc.SetProxyService(app.ProxyService)

	// 删除工作流：带墓碑记录删除意图与步骤进度，支持重启后断点续做
	app.DeleteWorkflow = service.NewDeleteVMWorkflow(db, app.HostSvc)
	app.DeleteWorkflow.NATSvc = app.NATService
	app.DeleteWorkflow.ProxySvc = app.ProxyService
	app.DeleteWorkflow.QuotaSvc = app.QuotaSvc
	app.DeleteWorkflow.Reconcile = app.ReconcileSvc

	// 创建Gin引擎
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(auth.RecoveryMiddleware())
	r.Use(gin.Logger())

	// CORS中间件（允许前端携带Cookie）
	r.Use(func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			origin = "*"
		}
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS,PATCH")
		// X-CSRF-Token 必须列入白名单：前端对所有
		// POST/PUT/DELETE 都会带上该头，一旦跨端口/跨域直连，
		// 缺失会导致浏览器预检失败（请求直接被拦，连 403 都看不到）。
		c.Header("Access-Control-Allow-Headers", "Content-Type,Authorization,X-Requested-With,X-CSRF-Token")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	})

	// Session中间件：签名密钥持久化在数据库中，首次启动自动生成
	store := cookie.NewStore(loadSessionSecret(db))
	store.Options(sessions.Options{
		Path:     "/",
		MaxAge:   86400 * 7, // 7天
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	r.Use(sessions.Sessions("openidcs_session", store))

	app.Router = r
	app.registerRoutes()

	return app
}

// registerRoutes 注册所有路由
func (app *App) registerRoutes() {
	r := app.Router

	// 认证中间件工厂
	authMW := auth.AuthMiddleware(
		func(username string) (map[string]interface{}, error) {
			return app.DB.GetUserByUsername(username)
		},
		app.SystemSvc.GetToken,
	)
	adminMW := auth.RequireAdmin()

	// ============================================================
	// 公开路由（无需认证）
	// ============================================================
	r.GET("/api/csrf-token", auth.CSRFTokenHandler())
	r.POST("/api/login", app.handleLogin)
	r.POST("/api/register", app.handleRegister)
	r.GET("/verify_email", app.handleVerifyEmail)
	r.GET("/verify-email-change", app.handleVerifyEmailChange)
	r.POST("/api/forgot-password", app.handleForgotPassword)
	r.POST("/api/system/forgot-password", app.handleForgotPassword)
	r.POST("/api/system/reset-password", app.handleResetPassword)
	r.POST("/api/client/upload", app.handleVMUpload)
	r.POST("/api/client/cmd_result", app.handleVMCmdResult)    // 命令结果回传（无需认证）
	r.GET("/api/client/templogin", app.handleVMTempLogin)      // 临时凭据登录（无需认证）
	r.POST("/api/client/templogin", app.handleVMTempLogin)     // 临时凭据登录（无需认证）
	r.GET("/api/client/keylogin", app.handleVMAccessKeyLogin)  // 访问密钥登录跳转（无需认证）
	r.POST("/api/client/keylogin", app.handleVMAccessKeyLogin) // 访问密钥登录跳转（无需认证）
	r.GET("/api/i18n/languages", app.handleI18nLanguages)
	r.GET("/api/i18n/translations/:lang_code", app.handleI18nTranslations)
	r.GET("/api/public/turnstile-config", app.handleTurnstileConfig)

	// ============================================================
	// 需要认证的路由（启用 CSRF 防护）
	// ============================================================
	// CSRF 豁免路径：只读操作（GET）已自动豁免
	csrfMW := auth.CSRFMiddleware(
		"/api/login",
		"/api/register",
		"/api/client/upload",
		"/api/client/cmd_result",
		"/api/client/templogin",
		"/api/client/keylogin",
	)
	api := r.Group("/api", authMW, csrfMW, app.HostVMAccessMiddleware())
	{
		// 认证相关
		api.POST("/logout", app.handleLogout)
		api.POST("/users/change-password", app.handleChangePassword)
		api.POST("/users/change-email", app.handleChangeEmail)

		// 系统管理
		api.GET("/system/engine", app.handleSystemEngine)
		api.POST("/system/saving", app.handleSystemSave)
		api.POST("/system/save", app.handleSystemSave)
		api.POST("/system/loader", app.handleSystemLoad)
		api.POST("/system/load", app.handleSystemLoad)
		api.GET("/system/statis", app.handleSystemStatis)
		api.GET("/system/stats", app.handleSystemStats)
		// 系统 Bearer Token 属于全局管理凭据：普通用户可读等同于提权，
		// 因此读取、设置、重置全部要求管理员身份。
		api.GET("/token/current", adminMW, app.handleTokenCurrent)
		api.POST("/token/set", adminMW, app.handleTokenSet)
		api.POST("/token/reset", adminMW, app.handleTokenReset)
		api.GET("/system/logger/detail", app.handleLoggerDetail)
		api.GET("/system/logger/stats", app.handleLoggerStats)
		api.POST("/system/logger/clear", app.handleLoggerClear)
		api.GET("/system/tasker", app.handleTasker)
		api.GET("/system/ipv4", app.handleSystemIPv4)
		api.POST("/system/recalculate-quotas", app.handleRecalculateQuotas)

		// 系统设置（管理员）
		api.GET("/system/settings", adminMW, app.handleGetSettings)
		api.POST("/system/settings", adminMW, app.handleUpdateSettings)
		api.POST("/system/test-email", adminMW, app.handleTestEmail)

		// 主机管理
		api.GET("/server/detail", app.handleHostList)
		api.GET("/server/detail/:hs_name", adminMW, app.handleHostDetail)
		api.POST("/server/create", adminMW, app.handleHostCreate)
		api.PUT("/server/update/:hs_name", adminMW, app.handleHostUpdate)
		api.DELETE("/server/delete/:hs_name", adminMW, app.handleHostDelete)
		api.POST("/server/powers/:hs_name", adminMW, app.handleHostPowers)
		api.GET("/server/status/:hs_name", app.handleHostStatus)
		api.GET("/server/plan/:hs_name", adminMW, app.handleHostPlanGet)
		api.POST("/server/plan/:hs_name", adminMW, app.handleHostPlanSet)
		api.DELETE("/server/plan/:hs_name/:plan_name", adminMW, app.handleHostPlanDelete)
		api.GET("/server/areas", app.handleHostAreas)
		api.GET("/server/plans/:hs_name", app.handleHostPlans)
		api.GET("/server/ports/:hs_name", app.handleHostPorts)
		api.POST("/server/backup/scan/:hs_name", app.handleHostBackupScan)

		// 虚拟机管理
		api.GET("/client/detail/:hs_name", app.handleVMList)
		api.GET("/client/detail/:hs_name/:vm_uuid", app.handleVMDetail)
		api.POST("/client/create/:hs_name", app.handleVMCreate)
		api.PUT("/client/update/:hs_name/:vm_uuid", app.handleVMUpdate)
		api.DELETE("/client/delete/:hs_name/:vm_uuid", app.handleVMDelete)
		api.POST("/client/powers/:hs_name/:vm_uuid", app.handleVMPowers)
		api.POST("/client/reinstall/:hs_name/:vm_uuid", app.handleVMReinstall)
		api.GET("/client/remote/:hs_name/:vm_uuid", app.handleVMRemote)
		api.GET("/client/screenshot/:hs_name/:vm_uuid", app.handleVMScreenshot)
		api.POST("/client/password/:hs_name/:vm_uuid", app.handleVMPassword)
		api.GET("/client/status/:hs_name/:vm_uuid", app.handleVMStatus)
		api.POST("/client/scaner/:hs_name", app.handleVMScan)
		// 清理不匹配 filter_name 前缀的历史纳管记录（仅记录，不动平台虚拟机）
		api.POST("/client/scaner/:hs_name/cleanup", app.handleVMCleanup)
		// 合并 Proxmox 命名变体（"-"/"_"）造成的重复记录
		api.POST("/client/scaner/:hs_name/dedupe", app.handleVMDedupe)

		// 命令执行
		api.POST("/client/cmd_send/:hs_name/:vm_uuid", app.handleVMCmdSend)
		api.GET("/client/cmd_status/:hs_name/:vm_uuid", app.handleVMCmdStatus)

		// 临时凭据
		api.GET("/client/temptoken/:hs_name/:vm_uuid", app.handleVMTempToken)

		// 访问密钥（16 位长期凭据，登录后权限与临时凭据一致）
		api.GET("/client/access-key/:hs_name/:vm_uuid", app.handleVMGetAccessKey)
		api.POST("/client/access-key/:hs_name/:vm_uuid", app.handleVMGenerateAccessKey)
		api.DELETE("/client/access-key/:hs_name/:vm_uuid", app.handleVMDeleteAccessKey)

		// 兼容路由
		api.POST("/client/reset/:hs_name/:vm_uuid", app.handleVMPassword)       // 旧版重置密码（复用handleVMPassword）
		api.GET("/client/owners/detail/:hs_name/:vm_uuid", app.handleOwnerList) // owners别名

		// 同步推送
		api.POST("/sync/push/:hs_name", app.handleSyncPush)

		// NAT端口转发
		api.GET("/client/natget/:hs_name/:vm_uuid", app.handleNATGet)
		api.POST("/client/natadd/:hs_name/:vm_uuid", app.handleNATAdd)
		api.DELETE("/client/natdel/:hs_name/:vm_uuid/:rule_index", app.handleNATDelete)

		// IP地址管理（旧风格）
		api.GET("/client/ipaddr/detail/:hs_name/:vm_uuid", app.handleIPList)
		api.POST("/client/ipaddr/create/:hs_name/:vm_uuid", app.handleIPCreate)
		api.DELETE("/client/ipaddr/delete/:hs_name/:vm_uuid/:ip_index", app.handleIPDelete)

		// IP地址管理（RESTful风格）
		api.GET("/hosts/:hs_name/vms/:vm_uuid/ip_addresses", app.handleIPList)
		api.POST("/hosts/:hs_name/vms/:vm_uuid/ip_addresses", app.handleIPCreate)
		api.PUT("/hosts/:hs_name/vms/:vm_uuid/ip_addresses/:nic_name", app.handleIPUpdate)

		// 网络静态绑定（爱快 DHCP静态地址 + ARP，对应 Python 版 NetiKuai）
		api.POST("/client/ipaddr/bind/:hs_name/:vm_uuid", app.handleIPBind)
		api.DELETE("/client/ipaddr/bind/:hs_name/:vm_uuid", app.handleIPUnbind)
		api.DELETE("/hosts/:hs_name/vms/:vm_uuid/ip_addresses/:nic_name", app.handleIPDeleteByName)

		// 反向代理（用户级）
		api.GET("/client/proxys/list", app.handleProxyUserList)
		api.GET("/client/proxys/detail/:hs_name/:vm_uuid", app.handleProxyDetail)
		api.POST("/client/proxys/create/:hs_name/:vm_uuid", app.handleProxyCreate)
		// 前端 apis.ts updateWebProxy 调用的是 client 前缀路径，
		// Python 版只注册了 admin 前缀，导致编辑代理时报 404。
		api.PUT("/client/proxys/update/:hs_name/:vm_uuid/:proxy_index", app.handleProxyUpdate)
		api.DELETE("/client/proxys/delete/:hs_name/:vm_uuid/:proxy_index", app.handleProxyDelete)

		// 反向代理（管理员级）
		api.GET("/admin/proxys/list", adminMW, app.handleProxyAdminList)
		api.GET("/admin/proxys/list/:hs_name", adminMW, app.handleProxyAdminListByHost)
		api.GET("/admin/proxys/detail/:hs_name/:vm_uuid", adminMW, app.handleProxyAdminDetail)
		api.POST("/admin/proxys/create/:hs_name/:vm_uuid", adminMW, app.handleProxyAdminCreate)
		api.PUT("/admin/proxys/update/:hs_name/:vm_uuid/:proxy_index", adminMW, app.handleProxyAdminUpdate)
		api.DELETE("/admin/proxys/delete/:hs_name/:vm_uuid/:proxy_index", adminMW, app.handleProxyAdminDelete)

		// 存储管理 - 数据盘
		api.GET("/client/hdd/detail/:hs_name/:vm_uuid", app.handleHDDList)
		api.POST("/client/hdd/mount/:hs_name/:vm_uuid", app.handleHDDMount)
		api.POST("/client/hdd/unmount/:hs_name/:vm_uuid", app.handleHDDUnmount)
		api.POST("/client/hdd/transfer/:hs_name/:vm_uuid", app.handleHDDTransfer)
		api.DELETE("/client/hdd/delete/:hs_name/:vm_uuid", app.handleHDDDelete)

		// 存储管理 - ISO
		api.GET("/client/isos/detail/:hs_name/:vm_uuid", app.handleISOList)
		api.POST("/client/iso/mount/:hs_name/:vm_uuid", app.handleISOMount)
		api.DELETE("/client/iso/unmount/:hs_name/:vm_uuid/:iso_name", app.handleISOUnmount)

		// 存储管理 - USB
		api.POST("/client/usb/mount/:hs_name/:vm_uuid", app.handleUSBMount)
		api.DELETE("/client/usb/delete/:hs_name/:vm_uuid/:usb_key", app.handleUSBDelete)

		// 备份管理
		api.GET("/client/backup/detail/:hs_name/:vm_uuid", app.handleBackupList)
		api.POST("/client/backup/create/:hs_name/:vm_uuid", app.handleBackupCreate)
		api.POST("/client/backup/restore/:hs_name/:vm_uuid", app.handleBackupRestore)
		api.DELETE("/client/backup/delete/:hs_name/:vm_uuid", app.handleBackupDelete)

		// 设备直通
		api.GET("/client/os-images/:hs_name", app.handleOSImages)
		api.GET("/client/gpu-list/:hs_name", app.handleGPUList)
		api.GET("/client/pci-list/:hs_name", app.handlePCIList)
		api.POST("/client/pci/setup/:hs_name/:vm_uuid", app.handlePCISetup)
		api.GET("/client/usb-list/:hs_name", app.handleUSBList)
		api.POST("/client/usb/setup/:hs_name/:vm_uuid", app.handleUSBSetup)

		// EFI启动项
		api.GET("/client/efi-list/:hs_name/:vm_uuid", app.handleEFIList)
		api.POST("/client/efi/setup/:hs_name/:vm_uuid", app.handleEFISetup)

		// 虚拟机所有者
		api.GET("/client/owners/:hs_name/:vm_uuid", app.handleOwnerList)
		api.POST("/client/owners/:hs_name/:vm_uuid", app.handleOwnerAdd)
		api.DELETE("/client/owners/:hs_name/:vm_uuid", app.handleOwnerDelete)
		api.PUT("/client/owners/:hs_name/:vm_uuid/permission", app.handleOwnerPermission)
		api.POST("/client/owners/:hs_name/:vm_uuid/transfer", app.handleOwnerTransfer)

		// 用户管理
		api.GET("/users/current", app.handleUserCurrent)
		api.GET("/users", adminMW, app.handleUserList)
		api.POST("/users", adminMW, app.handleUserCreate)
		api.GET("/users/:user_id", adminMW, app.handleUserDetail)
		api.PUT("/users/:user_id", adminMW, app.handleUserUpdate)
		api.DELETE("/users/:user_id", adminMW, app.handleUserDelete)

		// 异步任务管理 - 旧路径（兼容别名）
		api.GET("/tasks", app.handleTaskList)
		api.GET("/tasks/stats", app.handleTaskStats)
		api.GET("/tasks/:task_id", app.handleTaskDetail)
		api.POST("/tasks/:task_id/cancel", app.handleTaskCancel)
		api.POST("/tasks/:task_id/stop", app.handleTaskCancel)

		// 异步任务管理 - 新路径（主路径）
		api.GET("/system/async_task/list", app.handleTaskList)
		api.GET("/system/async_task/stats", app.handleTaskStats)
		api.GET("/system/async_task/:task_id", app.handleTaskDetail)
		api.POST("/system/async_task/:task_id/stop", app.handleTaskCancel)
		api.POST("/system/async_task/:task_id/retry", app.handleTaskRetry)

		// 外部资源对账与删除墓碑（运维接口，仅管理员）
		api.GET("/system/outbox/list", adminMW, app.handleOutboxList)
		api.POST("/system/outbox/:outbox_id/retry", adminMW, app.handleOutboxRetry)
		api.POST("/system/outbox/:outbox_id/dismiss", adminMW, app.handleOutboxDismiss)
		api.GET("/system/tombstone/list", adminMW, app.handleTombstoneList)
		api.POST("/system/tombstone/sweep", adminMW, app.handleTombstoneSweep)
	}

	// 静态文件服务（前端SPA）
	r.StaticFS("/static", http.Dir("./static"))
	r.StaticFS("/assets", http.Dir("./static/assets"))
	r.StaticFS("/ttf", http.Dir("./static/ttf"))
	r.StaticFile("/index.html", "./static/index.html")
	r.NoRoute(func(c *gin.Context) {
		// SPA路由：非API请求返回index.html
		if len(c.Request.URL.Path) >= 4 && c.Request.URL.Path[:4] == "/api" {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "msg": "接口不存在"})
			return
		}
		c.File("./static/index.html")
	})
}
