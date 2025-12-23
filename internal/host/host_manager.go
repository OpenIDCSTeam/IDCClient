package host

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/database"
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/models"
	"github.com/sirupsen/logrus"
)

// HostManager 主机管理器 - 与Python版本的HostManage完全一致
type HostManager struct {
	engines       map[string]BaseServer
	logger        []*models.ZMessage
	bearer        string
	db            *database.Database
	hostConfigDAO *database.HostConfigDAO
	vmConfigDAO   *database.VMConfigDAO
	statusDAO     *database.StatusDAO
	logDAO        *database.LogDAO
	proxys        interface{} // 全局HTTP代理管理器，对应Python版本的HttpManage
	webAll        []*models.WebProxy // 全局代理配置列表，对应Python版本的web_all
	mutex         sync.RWMutex
	loggerField   *logrus.Logger
}

// NewHostManager 创建主机管理器
func NewHostManager(db *database.Database, logger *logrus.Logger) *HostManager {
	return &HostManager{
		engines:       make(map[string]BaseServer),
		logger:        make([]*models.ZMessage, 0),
		db:            db,
		hostConfigDAO: database.NewHostConfigDAO(db),
		vmConfigDAO:   database.NewVMConfigDAO(db),
		statusDAO:     database.NewStatusDAO(db),
		logDAO:        database.NewLogDAO(db),
		proxys:        nil, // 初始化为nil，在LoadAll中创建
		webAll:        make([]*models.WebProxy, 0), // 初始化全局代理配置列表
		mutex:         sync.RWMutex{},
		loggerField:   logger,
	}
}

// SetConfig 设置全局配置
func (h *HostManager) SetConfig() error {
	globalConfigs, err := h.hostConfigDAO.GetAllGlobalConfigs()
	if err != nil {
		return fmt.Errorf("获取全局配置失败: %w", err)
	}

	h.bearer = globalConfigs["bearer"]
	if h.bearer == "" {
		h.bearer = h.GenerateToken()
		err := h.hostConfigDAO.SetGlobalConfig("bearer", h.bearer)
		if err != nil {
			return fmt.Errorf("保存Token失败: %w", err)
		}
		h.loggerField.Infof("自动生成新Token: %s", h.bearer)
	}

	// 加载全局日志
	h.loadGlobalLogs()
	return nil
}

// GenerateToken 生成随机Token
func (h *HostManager) GenerateToken() string {
	bytes := make([]byte, 8)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// SetPassword 设置/重置访问Token
func (h *HostManager) SetPassword(token string) string {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if token != "" {
		h.bearer = token
	} else {
		h.bearer = h.GenerateToken()
	}

	err := h.hostConfigDAO.SetGlobalConfig("bearer", h.bearer)
	if err != nil {
		h.loggerField.Errorf("保存Token失败: %v", err)
	}

	return h.bearer
}

// ValidateToken 验证Token
func (h *HostManager) ValidateToken(token string) bool {
	h.mutex.RLock()
	defer h.mutex.RUnlock()
	return token != "" && token == h.bearer
}

// GetBearer 获取当前Token
func (h *HostManager) GetBearer() string {
	h.mutex.RLock()
	defer h.mutex.RUnlock()
	return h.bearer
}

// GetVMConfigDAO 获取虚拟机配置DAO
func (h *HostManager) GetVMConfigDAO() *database.VMConfigDAO {
	return h.vmConfigDAO
}

// GetHost 获取主机
func (h *HostManager) GetHost(hsName string) BaseServer {
	h.mutex.RLock()
	defer h.mutex.RUnlock()
	return h.engines[hsName]
}

// AddHost 添加主机
func (h *HostManager) AddHost(hsName, hsType string, hsConfig *models.HSConfig) *models.ZMessage {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	if _, exists := h.engines[hsName]; exists {
		return models.NewZMessage(false, "AddHost", "主机已存在")
	}

	// 检查引擎类型是否支持
	if !h.IsEngineSupported(hsType) {
		return models.NewZMessage(false, "AddHost", "不支持的主机类型")
	}

	// 设置服务器名称
	hsConfig.ServerName = hsName

	// 创建主机实例
	server, err := h.createServer(hsType, hsConfig)
	if err != nil {
		return models.NewZMessage(false, "AddHost", fmt.Sprintf("创建主机实例失败: %v", err))
	}

	// 初始化主机
	result := server.Create()
	if !result.Success {
		return result
	}

	// 加载主机数据
	result = server.Load()
	if !result.Success {
		return result
	}

	// 添加到管理器
	h.engines[hsName] = server
	h.loggerField.Infof("主机 %s 已添加到内存缓存，当前主机数量: %d", hsName, len(h.engines))

	// 保存配置到数据库
	err = h.hostConfigDAO.SetHostConfig(hsName, hsConfig)
	if err != nil {
		// 回滚
		delete(h.engines, hsName)
		h.loggerField.Errorf("保存主机配置到数据库失败: %v", err)
		return models.NewZMessage(false, "AddHost", fmt.Sprintf("保存配置失败: %v", err))
	}

	h.loggerField.Infof("主机 %s 配置已保存到数据库", hsName)
	return models.NewZMessage(true, "AddHost", "主机添加成功")
}

// DeleteHost 删除主机
func (h *HostManager) DeleteHost(hsName string) bool {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	server, exists := h.engines[hsName]
	if !exists {
		return false
	}

	// 卸载主机
	server.Unload()

	// 删除主机
	server.Delete()

	// 从数据库删除配置
	err := h.hostConfigDAO.DeleteHostConfig(hsName)
	if err != nil {
		h.loggerField.Errorf("删除主机配置失败: %v", err)
	}

	// 从管理器中移除
	delete(h.engines, hsName)

	return true
}

// UpdateHost 更新主机配置
func (h *HostManager) UpdateHost(hsName string, hsConfig *models.HSConfig) *models.ZMessage {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	server, exists := h.engines[hsName]
	if !exists {
		return models.NewZMessage(false, "UpdateHost", "主机不存在")
	}

	// 记录更新前的配置
	oldConfig := server.GetConfig()
	h.loggerField.Infof("更新主机 %s: 旧配置 network_pub=%s, 新配置 network_pub=%s", 
		hsName, oldConfig.NetworkPub, hsConfig.NetworkPub)

	// 保存原有虚拟机配置
	oldVMSaving := server.GetAllVMs()

	// 设置服务器名称
	hsConfig.ServerName = hsName

	// 重新创建主机实例
	newServer, err := h.createServer(hsConfig.ServerType, hsConfig)
	if err != nil {
		return models.NewZMessage(false, "UpdateHost", fmt.Sprintf("创建主机实例失败: %v", err))
	}

	// 恢复虚拟机配置
	for vmUUID, vmConfig := range oldVMSaving {
		// 这里需要深拷贝，实际实现时可以使用序列化反序列化
		newServer.(*AbstractBaseServer).vmSaving[vmUUID] = vmConfig
	}

	// 卸载旧主机
	server.Unload()

	// 加载新主机
	result := newServer.Load()
	if !result.Success {
		return result
	}

	// 更新管理器
	h.engines[hsName] = newServer
	h.loggerField.Infof("主机 %s 内存缓存已更新，新配置 network_pub=%s", hsName, newServer.GetConfig().NetworkPub)

	// 保存配置到数据库
	err = h.hostConfigDAO.SetHostConfig(hsName, hsConfig)
	if err != nil {
		// 回滚：恢复旧的主机实例
		h.engines[hsName] = server
		h.loggerField.Errorf("保存主机配置到数据库失败: %v", err)
		return models.NewZMessage(false, "UpdateHost", fmt.Sprintf("保存配置失败: %v", err))
	}
	h.loggerField.Infof("主机 %s 配置已保存到数据库", hsName)

	return models.NewZMessage(true, "UpdateHost", "主机更新成功")
}

// PowerHost 主机电源控制
func (h *HostManager) PowerHost(hsName string, enable bool) *models.ZMessage {
	server := h.GetHost(hsName)
	if server == nil {
		return models.NewZMessage(false, "PowerHost", "主机不存在")
	}

	var result *models.ZMessage
	if enable {
		result = server.Load()
	} else {
		result = server.Unload()
	}

	if result.Success {
		// 保存状态
		err := server.SaveData()
		if err != nil {
			h.loggerField.Errorf("保存主机状态失败: %v", err)
		}
	}

	return result
}

// LoadAll 加载所有配置
func (h *HostManager) LoadAll() error {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	// 加载全局配置
	err := h.SetConfig()
	if err != nil {
		return fmt.Errorf("加载全局配置失败: %w", err)
	}

	// 加载全局日志
	h.loadGlobalLogs()

	// 启动Http实例（与Python版本一致）
	// h.proxys = HttpManage()
	// h.proxys.start_web()

	// 加载全局代理配置（与Python版本一致）
	h.webAll = []*models.WebProxy{}
	// webProxyList := h.hostConfigDAO.GetWebProxy()
	// for _, proxyData := range webProxyList {
	//     webProxy := &models.WebProxy{
	//         LanPort:  proxyData["lan_port"].(int),
	//         LanAddr:  proxyData["lan_addr"].(string),
	//         WebAddr:  proxyData["web_addr"].(string),
	//         WebTips:  proxyData["web_tips"].(string),
	//         IsHTTPS:  proxyData["is_https"].(bool),
	//     }
	//     h.webAll = append(h.webAll, webProxy)
	//     
	//     // 添加到HttpManage
	//     if h.proxys != nil {
	//         h.proxys.proxy_add(
	//             (webProxy.LanPort, webProxy.LanAddr),
	//             webProxy.WebAddr,
	//             webProxy.IsHTTPS
	//         )
	//     }
	// }

	// 加载所有主机配置
	hostConfigs, err := h.hostConfigDAO.GetAllHostConfigs()
	if err != nil {
		return fmt.Errorf("加载主机配置失败: %w", err)
	}

	for _, hostConfig := range hostConfigs {
		hsName := hostConfig.ServerName

		// 创建主机实例
		server, err := h.createServer(hostConfig.ServerType, hostConfig)
		if err != nil {
			h.loggerField.Errorf("创建主机实例失败 %s: %v", hsName, err)
			continue
		}

		// 加载主机数据
		result := server.Load()
		if !result.Success {
			h.loggerField.Errorf("加载主机数据失败 %s: %s", hsName, result.Message)
			continue
		}

		// 添加到管理器
		h.engines[hsName] = server
		h.loggerField.Infof("主机 %s 加载成功", hsName)
	}

	h.loggerField.Infof("共加载 %d 个主机", len(h.engines))
	return nil
}

// SaveAll 保存所有配置
func (h *HostManager) SaveAll() error {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	errorCount := 0
	var lastError error

	// 保存全局日志
	if len(h.logger) > 0 {
		for _, log := range h.logger {
			err := h.logDAO.AddLog("", log) // 全局日志
			if err != nil {
				h.loggerField.Errorf("保存全局日志失败: %v", err)
				errorCount++
				lastError = err
			}
		}
	}

	// 保存每个主机的配置数据
	for hsName, server := range h.engines {
		err := server.SaveData()
		if err != nil {
			h.loggerField.Errorf("保存主机配置失败 %s: %v", hsName, err)
			errorCount++
			lastError = fmt.Errorf("主机 %s 保存失败: %w", hsName, err)
		}
	}

	// 关闭web服务器（与Python版本一致）
	// if h.proxys != nil {
	//     h.proxys.close_web()
	// }

	if errorCount > 0 {
		h.loggerField.Errorf("保存数据失败，共 %d 个错误", errorCount)
		return lastError
	} else {
		h.loggerField.Debug("所有数据保存成功")
		return nil
	}
}

// GetAllHosts 获取所有主机
func (h *HostManager) GetAllHosts() map[string]BaseServer {
	h.mutex.RLock()
	defer h.mutex.RUnlock()
	
	// 返回副本
	result := make(map[string]BaseServer)
	for k, v := range h.engines {
		result[k] = v
	}
	return result
}

// GetHostNames 获取所有主机名称
func (h *HostManager) GetHostNames() []string {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	names := make([]string, 0, len(h.engines))
	for hsName := range h.engines {
		names = append(names, hsName)
	}
	return names
}

// GetHostCount 获取主机数量
func (h *HostManager) GetHostCount() int {
	h.mutex.RLock()
	defer h.mutex.RUnlock()
	return len(h.engines)
}

// ScanVMs 扫描主机上的虚拟机
func (h *HostManager) ScanVMs(hsName, prefix string) *models.ZMessage {
	server := h.GetHost(hsName)
	if server == nil {
		return models.NewZMessage(false, "ScanVMs", "主机不存在")
	}

	result := server.VMScan(prefix)
	if result.Success {
		// 保存配置
		err := server.SaveData()
		if err != nil {
			h.loggerField.Errorf("保存扫描结果失败: %v", err)
		}
	}

	return result
}

// ExecuteCron 执行定时任务
func (h *HostManager) ExecuteCron() {
	h.mutex.RLock()
	engines := make(map[string]BaseServer)
	for k, v := range h.engines {
		engines[k] = v
	}
	h.mutex.RUnlock()

	for hsName, server := range engines {
		h.loggerField.Debugf("执行主机 %s 的定时任务", hsName)
		err := server.ExecuteCron()
		if err != nil {
			h.loggerField.Errorf("主机 %s 执行定时任务失败: %v", hsName, err)
		}
	}

	h.loggerField.Debug("执行定时任务完成")
}

// GetAllLogs 获取所有日志
func (h *HostManager) GetAllLogs(limit int) ([]*models.ZMessage, error) {
	return h.logDAO.GetAllLogs(limit)
}

// GetHostLogs 获取主机日志
func (h *HostManager) GetHostLogs(hsName string, limit int) ([]*models.ZMessage, error) {
	return h.logDAO.GetLogs(hsName, limit)
}

// AddLog 添加日志
func (h *HostManager) AddLog(log *models.ZMessage) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.logger = append(h.logger, log)
	
	// 限制日志数量
	if len(h.logger) > 1000 {
		h.logger = h.logger[500:] // 保留最新的500条
	}
}

// ExitAll 退出所有主机
func (h *HostManager) ExitAll() {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	for _, server := range h.engines {
		server.Unload()
	}
}

// loadGlobalLogs 加载全局日志
func (h *HostManager) loadGlobalLogs() {
	logs, err := h.logDAO.GetLogs("", 100) // 获取全局日志
	if err != nil {
		h.loggerField.Errorf("加载全局日志失败: %v", err)
		return
	}

	h.logger = logs
}

// createServer 创建服务器实例（工厂方法）
func (h *HostManager) createServer(serverType string, config *models.HSConfig) (BaseServer, error) {
	// 这里需要根据服务器类型创建不同的实现
	// 目前先返回AbstractBaseServer，实际项目中需要实现具体的服务器类型
	switch serverType {
	case "VMWareSetup":
		// 返回VMware实现
		return NewAbstractBaseServer(config, h.db, h.loggerField), nil
	case "HyperV":
		// 返回Hyper-V实现
		return NewAbstractBaseServer(config, h.db, h.loggerField), nil
	default:
		return NewAbstractBaseServer(config, h.db, h.loggerField), nil
	}
}

// IsEngineSupported 检查引擎是否支持
func (h *HostManager) IsEngineSupported(engineType string) bool {
	// 这里应该检查引擎配置，暂时返回true
	supportedEngines := []string{"VMWareSetup", "HyperV", "Proxmox", "VirtualBox"}
	for _, engine := range supportedEngines {
		if engine == engineType {
			return true
		}
	}
	return false
}

// GetEngineConfigs 获取支持的引擎配置
func (h *HostManager) GetEngineConfigs() map[string]*models.EngineConfig {
	configs := make(map[string]*models.EngineConfig)
	
	configs["VMWareSetup"] = &models.EngineConfig{
		Name:        "VMWareSetup",
		Description: "VMware Workstation",
		Enabled:     true,
		Platform:    []string{"Windows"},
		Arch:        []string{"x86_64"},
		Options:     map[string]string{},
		Message:     "VMware Workstation 虚拟化平台",
		Imported:    "host.NewAbstractBaseServer",
	}

	configs["HyperV"] = &models.EngineConfig{
		Name:        "HyperV",
		Description: "Microsoft Hyper-V",
		Enabled:     false,
		Platform:    []string{"Windows"},
		Arch:        []string{"x86_64"},
		Options:     map[string]string{},
		Message:     "计划中的 Hyper-V 支持",
		Imported:    "",
	}

	return configs
}

// GetSystemStats 获取系统统计信息
func (h *HostManager) GetSystemStats() *models.SystemStats {
	h.mutex.RLock()
	defer h.mutex.RUnlock()

	stats := &models.SystemStats{
		HostCount:      len(h.engines),
		RunningVMCount: 0,
		StoppedVMCount: 0,
		ErrorVMCount:   0,
	}

	for _, server := range h.engines {
		vms := server.GetAllVMs()
		stats.VMCount += len(vms)
		
		// 统计虚拟机状态
		for _, vmConfig := range vms {
			// 根据vmConfig中的配置信息来判断虚拟机状态
			// 这里简单检查VCEnabled字段来判断虚拟机是否可用
			// TODO: 可以根据更多字段（如AutoStart、CreatedAt等）进行更精确的状态判断
			if vmConfig.VCEnabled {
				// 如果VNC启用，认为虚拟机正在运行
				stats.RunningVMCount++
			} else {
				// 否则认为虚拟机停止
				stats.StoppedVMCount++
			}
		}
	}

	return stats
}

// ============================================================================
// 全局代理管理方法 - 与Python版本的HostManage.py保持一致
// ============================================================================

// AddProxy 添加全局代理配置 - 与Python版本一致
func (h *HostManager) AddProxy(proxyData map[string]interface{}) *models.ZMessage {
	defer func() {
		if r := recover(); r != nil {
			h.loggerField.Errorf("添加全局代理失败: %v", r)
		}
	}()

	// 创建WebProxy对象
	webProxy := &models.WebProxy{
		LanPort:  int(proxyData["lan_port"].(float64)),
		LanAddr:  proxyData["lan_addr"].(string),
		WebAddr:  proxyData["web_addr"].(string),
		WebTips:  proxyData["web_tips"].(string),
		IsHTTPS:  proxyData["is_https"].(bool),
	}

	// 检查代理是否已存在
	for _, proxy := range h.webAll {
		if proxy.WebAddr == webProxy.WebAddr {
			return models.NewZMessage(false, "AddProxy", "代理域名 "+webProxy.WebAddr+" 已存在")
		}
	}

	// 调用HttpManage添加代理
	if h.proxys != nil {
		// TODO: 实现proxy_add方法
		// result := h.proxys.proxy_add(
		//     (webProxy.LanPort, webProxy.LanAddr),
		//     webProxy.WebAddr,
		//     webProxy.IsHTTPS
		// )
		// if !result {
		//     return models.NewZMessage(false, "AddProxy", "添加代理到HttpManage失败")
		// }
	} else {
		return models.NewZMessage(false, "AddProxy", "HttpManage未初始化")
	}

	// 添加到webAll列表
	h.mutex.Lock()
	h.webAll = append(h.webAll, webProxy)
	h.mutex.Unlock()

	// 保存到数据库
	// success := h.hostConfigDAO.AddWebProxy(proxyData)
	// if !success {
	//     // 如果保存失败，回滚操作
	//     h.mutex.Lock()
	//     h.webAll = h.webAll[:len(h.webAll)-1]
	//     h.mutex.Unlock()
	//     if h.proxys != nil {
	//         // h.proxys.proxy_del(webProxy.WebAddr)
	//     }
	//     return models.NewZMessage(false, "AddProxy", "保存代理配置到数据库失败")
	// }

	return models.NewZMessage(true, "AddProxy", "代理 "+webProxy.WebAddr+" 添加成功")
}

// DeleteProxy 删除全局代理配置 - 与Python版本一致
func (h *HostManager) DeleteProxy(webAddr string) *models.ZMessage {
	defer func() {
		if r := recover(); r != nil {
			h.loggerField.Errorf("删除全局代理失败: %v", r)
		}
	}()

	// 查找代理
	var webProxy *models.WebProxy = nil
	h.mutex.Lock()
	for i, proxy := range h.webAll {
		if proxy.WebAddr == webAddr {
			webProxy = proxy
			// 从webAll列表移除
			h.webAll = append(h.webAll[:i], h.webAll[i+1:]...)
			break
		}
	}
	h.mutex.Unlock()

	if webProxy == nil {
		return models.NewZMessage(false, "DeleteProxy", "代理域名 "+webAddr+" 不存在")
	}

	// 调用HttpManage删除代理
	if h.proxys != nil {
		// TODO: 实现proxy_del方法
		// result := h.proxys.proxy_del(webAddr)
		// if !result {
		//     return models.NewZMessage(false, "DeleteProxy", "从HttpManage删除代理失败")
		// }
	} else {
		return models.NewZMessage(false, "DeleteProxy", "HttpManage未初始化")
	}

	// 从数据库删除
	// success := h.hostConfigDAO.DeleteWebProxy(webAddr)
	// if !success {
	//     // 如果删除失败，回滚操作
	//     h.mutex.Lock()
	//     h.webAll = append(h.webAll, webProxy)
	//     h.mutex.Unlock()
	//     if h.proxys != nil {
	//         // h.proxys.proxy_add(
	//         //     (webProxy.LanPort, webProxy.LanAddr),
	//         //     webProxy.WebAddr,
	//         //     webProxy.IsHTTPS
	//         // )
	//     }
	//     return models.NewZMessage(false, "DeleteProxy", "从数据库删除代理配置失败")
	// }

	return models.NewZMessage(true, "DeleteProxy", "代理 "+webAddr+" 删除成功")
}

// GetAllProxies 获取所有全局代理配置 - 与Python版本一致
func (h *HostManager) GetAllProxies() []*models.WebProxy {
	h.mutex.RLock()
	defer h.mutex.RUnlock()
	
	// 返回副本
	result := make([]*models.WebProxy, len(h.webAll))
	copy(result, h.webAll)
	return result
}

// SetProxys 设置全局代理管理器 - 与Python版本一致
func (h *HostManager) SetProxys(proxys interface{}) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.proxys = proxys
}

// GetProxys 获取全局代理管理器 - 与Python版本一致
func (h *HostManager) GetProxys() interface{} {
	h.mutex.RLock()
	defer h.mutex.RUnlock()
	return h.proxys
}