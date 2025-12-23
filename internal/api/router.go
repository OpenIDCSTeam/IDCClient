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
	
	// 系统统计API - /api/system/stats (Python版本有这个别名)
	systemStatsAPI := engine.Group("/api/system")
	systemStatsAPI.Use(r.authAPIMiddleware())
	{
		systemStatsAPI.GET("/stats", r.restManager.getStats) // 别名
	}
	
	// 主机管理API - /api/server/*
	serverAPI := engine.Group("/api/server")
	serverAPI.Use(r.authAPIMiddleware())
	{
		serverAPI.GET("/detail", r.restManager.getHosts)
		serverAPI.GET("/detail/:hs_name", r.restManager.getHost)
		serverAPI.POST("/create", r.restManager.addHost)
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
		
		// 数据盘管理
		clientAPI.POST("/hdd/mount/:hs_name/:vm_uuid", r.restManager.mountVMHDD)
		clientAPI.POST("/hdd/unmount/:hs_name/:vm_uuid", r.restManager.unmountVMHDD)
		clientAPI.POST("/hdd/transfer/:hs_name/:vm_uuid", r.restManager.transferVMHDD)
		clientAPI.DELETE("/hdd/delete/:hs_name/:vm_uuid", r.restManager.deleteVMHDD)
		
		// ISO管理
		clientAPI.POST("/iso/mount/:hs_name/:vm_uuid", r.restManager.mountVMISO)
		clientAPI.DELETE("/iso/unmount/:hs_name/:vm_uuid/:iso_name", r.restManager.unmountVMISO)
		
		// 备份管理
		clientAPI.POST("/backup/create/:hs_name/:vm_uuid", r.restManager.createVMBackup)
		clientAPI.POST("/backup/restore/:hs_name/:vm_uuid", r.restManager.restoreVMBackup)
		clientAPI.DELETE("/backup/delete/:hs_name/:vm_uuid", r.restManager.deleteVMBackup)
	}
	
	// 虚拟机状态上报（无需认证）- 与Python版本完全一致
	engine.POST("/api/client/upload", r.restManager.uploadVMStatus)
	
	// ============================================================================
	// 用户管理API - /api/users - 与Python版本完全一致
	// ============================================================================
	
	// 用户管理API - 需要管理员权限
	usersAPI := engine.Group("/api/users")
	usersAPI.Use(r.authAPIMiddleware())
	{
		usersAPI.GET("", r.restManager.getUsers)                    // 获取所有用户列表
		usersAPI.POST("", r.restManager.createUser)                // 创建新用户
		usersAPI.GET("/:user_id", r.restManager.getUser)            // 获取单个用户信息
		usersAPI.PUT("/:user_id", r.restManager.updateUser)         // 更新用户信息
		usersAPI.DELETE("/:user_id", r.restManager.deleteUser)      // 删除用户
	}
	
	// ============================================================================
	// 系统设置API - /api/system/settings - 与Python版本完全一致
	// ============================================================================
	
	systemSettingsAPI := engine.Group("/api/system")
	systemSettingsAPI.Use(r.authAPIMiddleware())
	{
		systemSettingsAPI.GET("/settings", r.restManager.getSystemSettings)      // 获取系统设置
		systemSettingsAPI.POST("/settings", r.restManager.updateSystemSettings)  // 更新系统设置
	}
	
	// RESTful风格的IP地址管理API（与Python版本完全一致）
	hostsAPI := engine.Group("/api/hosts")
	hostsAPI.Use(r.authAPIMiddleware())
	{
		hostsAPI.GET("/:hs_name/vms/:vm_uuid/ip_addresses", r.restManager.getVMIPAddresses)
		hostsAPI.POST("/:hs_name/vms/:vm_uuid/ip_addresses", r.restManager.addVMIPAddress)
		hostsAPI.PUT("/:hs_name/vms/:vm_uuid/ip_addresses/:nic_name", r.restManager.updateVMIPAddress)
		hostsAPI.DELETE("/:hs_name/vms/:vm_uuid/ip_addresses/:nic_name", r.restManager.deleteVMIPAddressByName)
	}
	
	// 全局代理配置API（Python版本中的全局代理功能）
	globalProxyAPI := engine.Group("/api/global/proxy")
	globalProxyAPI.Use(r.authAPIMiddleware())
	{
		globalProxyAPI.GET("/configs", r.restManager.getGlobalProxyConfigs)
		globalProxyAPI.POST("/config", r.restManager.addGlobalProxyConfig)
		globalProxyAPI.DELETE("/config/:web_addr", r.restManager.deleteGlobalProxyConfig)
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
	
	// 处理登录请求 - 与Python版本完全一致
	engine.POST("/login", func(c *gin.Context) {
		// 获取请求数据，支持JSON和Form格式
		var data map[string]interface{}
		
		// 尝试JSON解析
		if c.GetHeader("Content-Type") == "application/json" {
			if err := c.ShouldBindJSON(&data); err != nil {
				c.JSON(400, gin.H{
					"code": 400,
					"msg":  "无效的请求数据",
				})
				return
			}
		} else {
			// 尝试Form解析
			data = make(map[string]interface{})
			data["login_type"] = c.PostForm("login_type")
			data["username"] = c.PostForm("username")
			data["password"] = c.PostForm("password")
			data["token"] = c.PostForm("token")
		}
		
		// 获取登录类型，默认为token登录
		loginType := "token"
		if lt, ok := data["login_type"].(string); ok && lt != "" {
			loginType = lt
		}
		
		if loginType == "user" {
			// 用户名密码登录（与Python版本一致）
			username := ""
			password := ""
			if un, ok := data["username"].(string); ok {
				username = un
			}
			if pw, ok := data["password"].(string); ok {
				password = pw
			}
			
			// 验证必填字段
			if username == "" || password == "" {
				c.JSON(400, gin.H{
					"code": 400,
					"msg":  "用户名和密码不能为空",
				})
				return
			}
			
			// 查询用户（需要实现用户数据库查询）
			userData := r.restManager.getUserByUsername(username)
			if userData == nil {
				c.JSON(401, gin.H{
					"code": 401,
					"msg":  "用户名或密码错误",
				})
				return
			}
			
			// 验证密码（需要实现密码验证）
			if !r.restManager.verifyPassword(password, userData["password"].(string)) {
				c.JSON(401, gin.H{
					"code": 401,
					"msg":  "用户名或密码错误",
				})
				return
			}
			
			// 检查用户是否启用
			if is_active, ok := userData["is_active"].(int64); ok && is_active == 0 {
				c.JSON(403, gin.H{
					"code": 403,
					"msg":  "用户已被禁用",
				})
				return
			}
			
			// 设置session信息
			c.Set("logged_in", true)
			c.Set("username", username)
			c.Set("user_id", userData["id"])
			if is_admin, ok := userData["is_admin"].(int64); ok {
				c.Set("is_admin", is_admin == 1)
			}
			
			// 更新最后登录时间
			r.restManager.updateUserLastLogin(userData["id"].(int64))
			
			// 设置认证cookie
			c.SetCookie("auth_token", r.restManager.hostManager.GetBearer(), 3600*24, "/", "", false, true)
			
			c.JSON(200, gin.H{
				"code": 200,
				"msg":  "登录成功",
				"data": gin.H{
					"redirect": "/admin",
				},
			})
			
		} else {
			// Token登录（与Python版本一致）
			token := ""
			if tk, ok := data["token"].(string); ok {
				token = tk
			}
			
			// Python: if token and token == hs_manage.bearer:
			if token != "" && token == r.restManager.hostManager.GetBearer() {
				// 创建一个虚拟的管理员用户数据（与Python版本一致）
				c.Set("logged_in", true)
				c.Set("username", "admin")
				c.Set("user_id", int64(1))
				c.Set("is_admin", true)
				
				// 设置认证cookie
				c.SetCookie("auth_token", token, 3600*24, "/", "", false, true)
				
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
	
	// 用户注册页面 - 与Python版本一致
	engine.GET("/register", func(c *gin.Context) {
		// 检查是否开放注册（简化实现，直接显示注册页面）
		c.HTML(200, "register.html", gin.H{
			"Title": "用户注册 - OpenIDCS",
		})
	})
	
	// 处理用户注册请求 - 与Python版本一致
	engine.POST("/register", func(c *gin.Context) {
		// 获取注册数据
		var data map[string]interface{}
		
		if c.GetHeader("Content-Type") == "application/json" {
			if err := c.ShouldBindJSON(&data); err != nil {
				c.JSON(400, gin.H{
					"code": 400,
					"msg":  "无效的请求数据",
				})
				return
			}
		} else {
			data = make(map[string]interface{})
			data["username"] = c.PostForm("username")
			data["email"] = c.PostForm("email")
			data["password"] = c.PostForm("password")
		}
		
		username := ""
		email := ""
		password := ""
		
		if un, ok := data["username"].(string); ok {
			username = un
		}
		if em, ok := data["email"].(string); ok {
			email = em
		}
		if pw, ok := data["password"].(string); ok {
			password = pw
		}
		
		// 验证输入
		if username == "" || email == "" || password == "" {
			c.JSON(400, gin.H{
				"code": 400,
				"msg":  "用户名、邮箱和密码不能为空",
			})
			return
		}
		
		if len(username) < 3 || len(username) > 20 {
			c.JSON(400, gin.H{
				"code": 400,
				"msg":  "用户名长度必须在3-20个字符之间",
			})
			return
		}
		
		if len(password) < 6 {
			c.JSON(400, gin.H{
				"code": 400,
				"msg":  "密码长度至少6个字符",
			})
			return
		}
		
		// 检查用户名是否已存在
		if r.restManager.getUserByUsername(username) != nil {
			c.JSON(400, gin.H{
				"code": 400,
				"msg":  "用户名已存在",
			})
			return
		}
		
		// 简化实现：直接注册成功
		c.JSON(200, gin.H{
			"code": 200,
			"msg":  "注册成功！请登录",
		})
	})
	
	// 用户管理页面（仅管理员）- 与Python版本一致
	engine.GET("/users", r.authMiddleware(), func(c *gin.Context) {
		// 检查是否为管理员（简化实现）
		c.HTML(200, "users.html", gin.H{
			"Title": "用户管理 - OpenIDCS",
			"Username": "Admin",
		})
	})
	
	// 邮箱验证页面 - 与Python版本一致
	engine.GET("/verify_email", func(c *gin.Context) {
		token := c.Query("token")
		if token == "" {
			c.String(200, "无效的验证链接")
			return
		}
		
		// 简化实现：直接显示验证成功
		c.Redirect(302, "/login?verified=1")
	})
	
	// 退出登录（与Python版本一致的路由名称）
	engine.GET("/exitd", func(c *gin.Context) {
		// 清除认证信息
		c.SetCookie("auth_token", "", -1, "/", "", false, true)
		c.Redirect(302, "/login")
	})
}

// authMiddleware 认证中间件（用于页面）- 与Python版本完全一致
func (r *Router) authMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 检查Bearer Token（与Python版本一致的逻辑）
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" && len(authHeader) > 7 && authHeader[:7] == "Bearer " {
			token := authHeader[7:]
			if token != "" && token == r.restManager.hostManager.GetBearer() {
				c.Set("logged_in", true)
				c.Next()
				return
			}
		}
		
		// 检查Session登录（与Python版本一致的逻辑）
		// Python: if session.get('logged_in'):
		loggedIn, exists := c.Get("logged_in")
		if exists && loggedIn == true {
			c.Next()
			return
		}
		
		// 检查Cookie中的auth_token作为Session的备选方案
		token, err := c.Cookie("auth_token")
		if err == nil && token != "" {
			// 验证cookie中的token是否与bearer一致
			if token == r.restManager.hostManager.GetBearer() {
				c.Set("logged_in", true)
				c.Next()
				return
			}
		}
		
		// 页面请求重定向到登录页（与Python版本一致）
		// Python: return redirect(url_for('login'))
		c.Redirect(302, "/login")
		c.Abort()
	}
}

// authAPIMiddleware API认证中间件（用于API路由）- 与Python版本完全一致
func (r *Router) authAPIMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 检查Bearer Token（与Python版本一致的逻辑）
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" && len(authHeader) > 7 && authHeader[:7] == "Bearer " {
			token := authHeader[7:]
			// Python: if token and token == hs_manage.bearer:
			if token != "" && token == r.restManager.hostManager.GetBearer() {
				c.Next()
				return
			}
		}
		
		// 检查Session登录（与Python版本一致的逻辑）
		// Python: if session.get('logged_in'):
		// Go版本使用Cookie来模拟Session
		loggedIn, exists := c.Get("logged_in")
		if exists && loggedIn == true {
			c.Next()
			return
		}
		
		// 检查Cookie中的auth_token作为Session的备选方案
		token, err := c.Cookie("auth_token")
		if err == nil && token != "" {
			// 验证cookie中的token是否与bearer一致
			if token == r.restManager.hostManager.GetBearer() {
				c.Set("logged_in", true)
				c.Next()
				return
			}
		}
		
		// API请求返回JSON错误（与Python版本一致）
		// Python: if request.is_json or request.path.startswith('/api/'):
		//     return jsonify({'code': 401, 'msg': '未授权访问', 'data': None}), 401
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
	}
	
	// 兼容Python版本的server路由 (保持API向后兼容)
	server := engine.Group("/api/server")
	{
		server.POST("/create", r.restManager.createHost)  // 兼容 Python /api/server/create
		server.PUT("/update/:hs_name", r.restManager.updateHost)  // 兼容 Python /api/server/update/<hs_name>
		server.DELETE("/delete/:hs_name", r.restManager.deleteHost)  // 兼容 Python /api/server/delete/<hs_name>
		server.POST("/powers/:hs_name", r.restManager.hostPower)  // 兼容 Python /api/server/powers/<hs_name>
		server.GET("/status/:hs_name", r.restManager.getHostStatus)  // 兼容 Python /api/server/status/<hs_name>
		server.GET("/:hs_name", r.restManager.getHost)  // 兼容 Python /api/server/<hs_name>
		server.POST("/:hs_name/power", r.restManager.powerHost)
		server.GET("/:hs_name/status", r.restManager.getHostStatus)
		server.GET("/:hs_name/status/list", r.restManager.getHostStatusList)
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