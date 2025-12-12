package api

import (
	"html/template"
	"github.com/gin-gonic/gin"
	"github.com/sirupsen/logrus"
)

// Router 路由器结构体
type Router struct {
	restManager *RestManager
	logger      *logrus.Logger
}

// NewRouter 创建路由器
func NewRouter(restManager *RestManager, logger *logrus.Logger) *Router {
	return &Router{
		restManager: restManager,
		logger:      logger,
	}
}

// SetupRoutes 设置路由
func (r *Router) SetupRoutes() *gin.Engine {
	// 设置Gin模式
	gin.SetMode(gin.ReleaseMode)
	
	// 创建Gin引擎
	engine := gin.New()
	
	// 设置HTML模板
	// 加载所有模板文件，包括partials
	templateFiles := []string{
		"./web/templates/*.html",
		"./web/templates/partials/*.html",
	}
	
	var templates *template.Template
	for _, pattern := range templateFiles {
		var err error
		if templates == nil {
			templates, err = template.ParseGlob(pattern)
		} else {
			templates, err = templates.ParseGlob(pattern)
		}
		if err != nil {
			r.logger.Fatalf("加载模板失败 %s: %v", pattern, err)
		}
	}
	
	engine.SetHTMLTemplate(templates)
	
	// 添加中间件
	engine.Use(gin.Logger())
	engine.Use(gin.Recovery())
	engine.Use(r.corsMiddleware())
	
	// 静态文件服务
	engine.Static("/static", "./web/static")
	
	// 健康检查
	engine.GET("/health", r.healthCheck)
	engine.GET("/", r.root)
	
	// 页面路由
	r.setupPageRoutes(engine)
	
	// API路由 - 与Python版本保持一致
	// 系统管理API - /api/system/*
	systemAPI := engine.Group("/api/system")
	systemAPI.Use(r.authAPIMiddleware())
	{
		systemAPI.GET("/engine", r.restManager.getEngineTypes)
		systemAPI.POST("/saving", r.restManager.saveSystem)
		systemAPI.POST("/save", r.restManager.saveSystem) // 别名
		systemAPI.POST("/loader", r.restManager.loadSystem)
		systemAPI.POST("/load", r.restManager.loadSystem) // 别名
		systemAPI.GET("/statis", r.restManager.getSystemStats)
		systemAPI.GET("/stats", r.restManager.getStats) // 别名
		systemAPI.GET("/logger/detail", r.restManager.getLogs)
		systemAPI.GET("/tasker", r.restManager.getTasks)
	}
	
	// Token管理API - /api/token/*
	tokenAPI := engine.Group("/api/token")
	tokenAPI.Use(r.authAPIMiddleware())
	{
		tokenAPI.GET("/current", r.restManager.getCurrentToken)
		tokenAPI.POST("/set", r.restManager.setToken)
		tokenAPI.POST("/reset", r.restManager.resetToken)
	}
	
	// 主机管理API - /api/server/*
	serverAPI := engine.Group("/api/server")
	serverAPI.Use(r.authAPIMiddleware())
	{
		serverAPI.GET("/detail", r.restManager.getHosts)
		serverAPI.GET("/detail/:hs_name", r.restManager.getHost)
		serverAPI.POST("/create", r.restManager.createHost)
		serverAPI.PUT("/update/:hs_name", r.restManager.updateHost)
		serverAPI.DELETE("/delete/:hs_name", r.restManager.deleteHost)
		serverAPI.POST("/powers/:hs_name", r.restManager.powerHost)
		serverAPI.GET("/status/:hs_name", r.restManager.getHostStatus)
	}
	
	// 虚拟机管理API - /api/client/*
	clientAPI := engine.Group("/api/client")
	clientAPI.Use(r.authAPIMiddleware())
	{
		clientAPI.GET("/detail/:hs_name", r.restManager.getVMsByHost)
		clientAPI.GET("/detail/:hs_name/:vm_uuid", r.restManager.getVM)
		clientAPI.POST("/create/:hs_name", r.restManager.createVM)
		clientAPI.PUT("/update/:hs_name/:vm_uuid", r.restManager.updateVM)
		clientAPI.DELETE("/delete/:hs_name/:vm_uuid", r.restManager.deleteVM)
		clientAPI.POST("/powers/:hs_name/:vm_uuid", r.restManager.vmPower)
		clientAPI.GET("/remote/:hs_name/:vm_uuid", r.restManager.getVMConsole)
		clientAPI.POST("/password/:hs_name/:vm_uuid", r.restManager.vmPassword)
		clientAPI.GET("/status/:hs_name/:vm_uuid", r.restManager.getVMStatus)
		clientAPI.POST("/scaner/:hs_name", r.restManager.scanVMs)
		
		// NAT端口转发
		clientAPI.GET("/natget/:hs_name/:vm_uuid", r.restManager.getVMNATRules)
		clientAPI.POST("/natadd/:hs_name/:vm_uuid", r.restManager.addVMNATRule)
		clientAPI.DELETE("/natdel/:hs_name/:vm_uuid/:rule_index", r.restManager.deleteVMNATRule)
		
		// IP地址管理
		clientAPI.GET("/ipaddr/detail/:hs_name/:vm_uuid", r.restManager.getVMIPAddresses)
		clientAPI.POST("/ipaddr/create/:hs_name/:vm_uuid", r.restManager.addVMIPAddress)
		clientAPI.DELETE("/ipaddr/delete/:hs_name/:vm_uuid/:ip_index", r.restManager.deleteVMIPAddress)
		
		// 反向代理管理
		clientAPI.GET("/proxys/detail/:hs_name/:vm_uuid", r.restManager.getVMProxyConfigs)
		clientAPI.POST("/proxys/create/:hs_name/:vm_uuid", r.restManager.addVMProxyConfig)
		clientAPI.DELETE("/proxys/delete/:hs_name/:vm_uuid/:proxy_index", r.restManager.deleteVMProxyConfig)
	}
	
	// 虚拟机状态上报（无需认证）
	engine.POST("/api/client/upload", r.restManager.uploadVMStatus)
	
	// RESTful风格的IP地址管理API（兼容）
	hostsAPI := engine.Group("/api/hosts")
	hostsAPI.Use(r.authAPIMiddleware())
	{
		hostsAPI.GET("/:hs_name/vms/:vm_uuid/ip_addresses", r.restManager.getVMIPAddresses)
		hostsAPI.POST("/:hs_name/vms/:vm_uuid/ip_addresses", r.restManager.addVMIPAddress)
		hostsAPI.PUT("/:hs_name/vms/:vm_uuid/ip_addresses/:nic_name", r.restManager.updateVMIPAddress)
		hostsAPI.DELETE("/:hs_name/vms/:vm_uuid/ip_addresses/:nic_name", r.restManager.deleteVMIPAddressByName)
	}
	
	return engine
}

// corsMiddleware CORS中间件
func (r *Router) corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Authorization")
		c.Header("Access-Control-Allow-Credentials", "true")
		
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		
		c.Next()
	}
}

// healthCheck 健康检查
func (r *Router) healthCheck(c *gin.Context) {
	c.JSON(200, gin.H{
		"status":  "ok",
		"message": "OpenIDCS 客户端运行正常",
	})
}

// root 根路由 - 重定向到仪表盘
func (r *Router) root(c *gin.Context) {
	// 检查是否已登录
	token, err := c.Cookie("auth_token")
	if err != nil || token == "" {
		// 未登录，重定向到登录页
		c.Redirect(302, "/login")
		return
	}
	// 已登录，重定向到仪表盘
	c.Redirect(302, "/admin")
}

// setupPageRoutes 设置页面路由
func (r *Router) setupPageRoutes(engine *gin.Engine) {
	// 登录页面
	engine.GET("/login", func(c *gin.Context) {
		c.HTML(200, "login.html", gin.H{
			"Title": "登录 - OpenIDCS",
			"Username": "Guest",
		})
	})
	
	// 处理登录请求
	engine.POST("/login", func(c *gin.Context) {
		var req struct {
			Token string `json:"token"`
		}
		
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{
				"code": 400,
				"msg":  "请求参数错误",
			})
			return
		}
		
		// 验证token是否与系统bearer token一致（与Python版本一致）
		// Python: if token and token == hs_manage.bearer:
		if req.Token != "" && req.Token == r.restManager.hostManager.GetBearer() {
			// 设置认证cookie
			c.SetCookie("auth_token", req.Token, 3600*24, "/", "", false, true)
			c.JSON(200, gin.H{
				"code": 200,
				"msg":  "登录成功",
				"data": gin.H{
					"redirect": "/admin",
				},
			})
		} else {
			c.JSON(401, gin.H{
				"code": 401,
				"msg":  "Token错误",
			})
		}
	})
	
	// 仪表盘页面
	engine.GET("/admin", r.authMiddleware(), func(c *gin.Context) {
		c.HTML(200, "dashboard.html", gin.H{
			"Title": "仪表盘 - OpenIDCS",
			"Username": "Admin",
		})
	})
	
	// 主机管理页面
	engine.GET("/hosts", r.authMiddleware(), func(c *gin.Context) {
		c.HTML(200, "hosts.html", gin.H{
			"Title": "主机管理 - OpenIDCS",
			"Username": "Admin",
		})
	})
	
	// 日志管理页面
	engine.GET("/debug", r.authMiddleware(), func(c *gin.Context) {
		c.HTML(200, "logs.html", gin.H{
			"Title": "日志管理 - OpenIDCS",
			"Username": "Admin",
		})
	})
	
	// 任务管理页面
	engine.GET("/tasks", r.authMiddleware(), func(c *gin.Context) {
		c.HTML(200, "tasks.html", gin.H{
			"Title": "任务管理 - OpenIDCS",
			"Username": "Admin",
		})
	})
	
	// 虚拟机管理页面（与Python版本一致的路由）
	engine.GET("/hosts/:hs_name/vms", r.authMiddleware(), func(c *gin.Context) {
		hsName := c.Param("hs_name")
		c.HTML(200, "vms.html", gin.H{
			"Title": "虚拟机管理 - OpenIDCS - " + hsName,
			"Username": "Admin",
			"hs_name": hsName,
		})
	})
	
	// 虚拟机详情页面（与Python版本一致的路由）
	engine.GET("/hosts/:hs_name/vms/:vm_uuid", r.authMiddleware(), func(c *gin.Context) {
		hsName := c.Param("hs_name")
		vmUUID := c.Param("vm_uuid")
		c.HTML(200, "vm_detail.html", gin.H{
			"Title": "OpenIDCS - " + vmUUID,
			"Username": "Admin",
			"hs_name": hsName,
			"vm_uuid": vmUUID,
		})
	})
	
	// 系统设置页面
	engine.GET("/settings", r.authMiddleware(), func(c *gin.Context) {
		c.HTML(200, "settings.html", gin.H{
			"Title": "系统设置 - OpenIDCS",
			"Username": "Admin",
		})
	})
	
	// 退出登录（与Python版本一致的路由名称）
	engine.GET("/exitd", func(c *gin.Context) {
		// 清除认证信息
		c.SetCookie("auth_token", "", -1, "/", "", false, true)
		c.Redirect(302, "/login")
	})
}

// authMiddleware 认证中间件（用于页面）
func (r *Router) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 简单的认证检查，实际项目中应该验证token
		token, err := c.Cookie("auth_token")
		if err != nil || token == "" {
			c.Redirect(302, "/login")
			c.Abort()
			return
		}
		c.Next()
	}
}

// authAPIMiddleware API认证中间件（用于API路由）
func (r *Router) authAPIMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 检查Bearer Token（与Python版本一致）
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" && len(authHeader) > 7 && authHeader[:7] == "Bearer " {
			token := authHeader[7:]
			// 验证token是否与系统bearer token一致
			if token != "" && token == r.restManager.hostManager.GetBearer() {
				c.Next()
				return
			}
		}
		
		// 检查Session登录（兼容Web界面调用API）
		token, err := c.Cookie("auth_token")
		if err == nil && token != "" && token == r.restManager.hostManager.GetBearer() {
			c.Next()
			return
		}
		
		// 未授权
		c.JSON(401, gin.H{
			"code": 401,
			"msg":  "未授权访问",
			"data": nil,
		})
		c.Abort()
	}
}

// RegisterAllRoutes 注册所有路由
func (r *Router) RegisterAllRoutes(engine *gin.Engine) {
	// 系统管理路由
	system := engine.Group("/api/v1/system")
	{
		system.GET("/token/current", r.restManager.getCurrentToken)
		system.POST("/token/set", r.restManager.setToken)
		system.POST("/token/reset", r.restManager.resetToken)
		system.GET("/engine", r.restManager.getEngineTypes)
		system.POST("/saving", r.restManager.saveSystem)
		system.POST("/loader", r.restManager.loadSystem)
		system.GET("/statis", r.restManager.getSystemStats)
		system.GET("/logger/detail", r.restManager.getLogs)
	}
	
	// 主机管理路由
	hosts := engine.Group("/api/v1/hosts")
	{
		hosts.GET("/", r.restManager.getHosts)
		hosts.GET("/detail", r.restManager.getHostDetails)
		hosts.GET("/names", r.restManager.getHostNames)
		hosts.GET("/:hs_name", r.restManager.getHost)
		hosts.POST("/", r.restManager.createHost)
		hosts.PUT("/:hs_name", r.restManager.updateHost)
		hosts.DELETE("/:hs_name", r.restManager.deleteHost)
		hosts.POST("/:hs_name/power", r.restManager.powerHost)
		hosts.GET("/:hs_name/status", r.restManager.getHostStatus)
		hosts.GET("/:hs_name/status/list", r.restManager.getHostStatusList)
	}
	
	// 虚拟机管理路由
	vms := engine.Group("/api/v1/vms")
	{
		vms.GET("/", r.restManager.getVMs)
		vms.GET("/:hs_name", r.restManager.getVMsByHost)
		vms.GET("/:hs_name/:vm_uuid", r.restManager.getVM)
		vms.POST("/:hs_name", r.restManager.createVM)
		vms.PUT("/:hs_name/:vm_uuid", r.restManager.updateVM)
		vms.DELETE("/:hs_name/:vm_uuid", r.restManager.deleteVM)
		vms.POST("/:hs_name/:vm_uuid/power", r.restManager.vmPower)
		vms.POST("/:hs_name/:vm_uuid/password", r.restManager.vmPassword)
		vms.GET("/:hs_name/:vm_uuid/status", r.restManager.getVMStatus)
		vms.GET("/:hs_name/:vm_uuid/status/list", r.restManager.getVMStatusList)
		vms.GET("/:hs_name/:vm_uuid/console", r.restManager.getVMConsole)
		vms.POST("/:hs_name/scan", r.restManager.scanVMs)
		vms.POST("/upload/status", r.restManager.uploadVMStatus)
	}
}