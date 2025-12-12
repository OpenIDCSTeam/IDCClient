package network

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// NetworkManager 网络管理器
type NetworkManager struct {
	allocatedPorts map[int]bool
	allocatedIPs   map[string]bool
	portMutex      sync.RWMutex
	ipMutex        sync.RWMutex
	logger         *logrus.Logger
	portRange      PortRange
}

// PortRange 端口范围
type PortRange struct {
	Start int
	End   int
}

// NewNetworkManager 创建网络管理器
func NewNetworkManager(portStart, portEnd int, logger *logrus.Logger) *NetworkManager {
	return &NetworkManager{
		allocatedPorts: make(map[int]bool),
		allocatedIPs:   make(map[string]bool),
		portMutex:      sync.RWMutex{},
		ipMutex:        sync.RWMutex{},
		logger:         logger,
		portRange: PortRange{
			Start: portStart,
			End:   portEnd,
		},
	}
}

// AllocatePort 分配端口
func (n *NetworkManager) AllocatePort() (int, error) {
	n.portMutex.Lock()
	defer n.portMutex.Unlock()

	// 遍历端口范围寻找可用端口
	for port := n.portRange.Start; port <= n.portRange.End; port++ {
		if !n.allocatedPorts[port] && !n.isPortInUse(port) {
			n.allocatedPorts[port] = true
			n.logger.Infof("分配端口: %d", port)
			return port, nil
		}
	}

	return 0, fmt.Errorf("端口范围内没有可用端口")
}

// ReleasePort 释放端口
func (n *NetworkManager) ReleasePort(port int) {
	n.portMutex.Lock()
	defer n.portMutex.Unlock()

	if n.allocatedPorts[port] {
		delete(n.allocatedPorts, port)
		n.logger.Infof("释放端口: %d", port)
	}
}

// isPortInUse 检查端口是否被占用
func (n *NetworkManager) isPortInUse(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf(":%d", port), 100*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// AllocatePortInRange 在指定范围内分配端口
func (n *NetworkManager) AllocatePortInRange(start, end int) (int, error) {
	n.portMutex.Lock()
	defer n.portMutex.Unlock()

	// 验证端口范围
	if start < n.portRange.Start || end > n.portRange.End || start > end {
		return 0, fmt.Errorf("无效的端口范围")
	}

	for port := start; port <= end; port++ {
		if !n.allocatedPorts[port] && !n.isPortInUse(port) {
			n.allocatedPorts[port] = true
			n.logger.Infof("在范围[%d-%d]中分配端口: %d", start, end, port)
			return port, nil
		}
	}

	return 0, fmt.Errorf("指定端口范围内没有可用端口")
}

// CheckPortAvailable 检查端口是否可用
func (n *NetworkManager) CheckPortAvailable(port int) bool {
	if port < n.portRange.Start || port > n.portRange.End {
		return false
	}

	n.portMutex.RLock()
	allocated := n.allocatedPorts[port]
	n.portMutex.RUnlock()

	if allocated {
		return false
	}

	return !n.isPortInUse(port)
}

// GetAllocatedPorts 获取已分配的端口列表
func (n *NetworkManager) GetAllocatedPorts() []int {
	n.portMutex.RLock()
	defer n.portMutex.RUnlock()

	ports := make([]int, 0, len(n.allocatedPorts))
	for port := range n.allocatedPorts {
		ports = append(ports, port)
	}
	return ports
}

// AllocateIP 分配IP地址
func (n *NetworkManager) AllocateIP(networkCIDR string) (string, error) {
	n.ipMutex.Lock()
	defer n.ipMutex.Unlock()

	_, ipnet, err := net.ParseCIDR(networkCIDR)
	if err != nil {
		return "", fmt.Errorf("无效的网络CIDR: %w", err)
	}

	// 遍历网络中的IP地址
	for ip := ipnet.IP; ipnet.Contains(ip); n.incrementIP(ip) {
		// 跳过网络地址和广播地址
		if n.isNetworkAddress(ip, ipnet) || n.isBroadcastAddress(ip, ipnet) {
			continue
		}

		ipStr := ip.String()
		if !n.allocatedIPs[ipStr] {
			n.allocatedIPs[ipStr] = true
			n.logger.Infof("分配IP地址: %s", ipStr)
			return ipStr, nil
		}
	}

	return "", fmt.Errorf("网络中没有可用IP地址")
}

// ReleaseIP 释放IP地址
func (n *NetworkManager) ReleaseIP(ip string) {
	n.ipMutex.Lock()
	defer n.ipMutex.Unlock()

	if n.allocatedIPs[ip] {
		delete(n.allocatedIPs, ip)
		n.logger.Infof("释放IP地址: %s", ip)
	}
}

// CheckIPAvailable 检查IP是否可用
func (n *NetworkManager) CheckIPAvailable(ip string) bool {
	n.ipMutex.RLock()
	defer n.ipMutex.RUnlock()

	return !n.allocatedIPs[ip]
}

// GetAllocatedIPs 获取已分配的IP地址列表
func (n *NetworkManager) GetAllocatedIPs() []string {
	n.ipMutex.RLock()
	defer n.ipMutex.RUnlock()

	ips := make([]string, 0, len(n.allocatedIPs))
	for ip := range n.allocatedIPs {
		ips = append(ips, ip)
	}
	return ips
}

// incrementIP 递增IP地址
func (n *NetworkManager) incrementIP(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

// isNetworkAddress 检查是否为网络地址
func (n *NetworkManager) isNetworkAddress(ip net.IP, ipnet *net.IPNet) bool {
	return ip.Equal(ipnet.IP)
}

// isBroadcastAddress 检查是否为广播地址
func (n *NetworkManager) isBroadcastAddress(ip net.IP, ipnet *net.IPNet) bool {
	broadcast := make(net.IP, len(ipnet.IP))
	copy(broadcast, ipnet.IP)

	for i := 0; i < len(broadcast); i++ {
		broadcast[i] |= ^ipnet.Mask[i]
	}

	return ip.Equal(broadcast)
}

// NetworkConfig 网络配置
type NetworkConfig struct {
	NatNetwork string
	PubNetwork string
	DNSServers []string
	IPMaps     map[string]string
	EnableIPv6 bool
}

// ValidateNetworkConfig 验证网络配置
func (n *NetworkManager) ValidateNetworkConfig(config *NetworkConfig) error {
	// 验证NAT网络
	if config.NatNetwork != "" {
		_, _, err := net.ParseCIDR(config.NatNetwork)
		if err != nil {
			return fmt.Errorf("无效的NAT网络配置: %w", err)
		}
	}

	// 验证公网网络
	if config.PubNetwork != "" {
		_, _, err := net.ParseCIDR(config.PubNetwork)
		if err != nil {
			return fmt.Errorf("无效的公网网络配置: %w", err)
		}
	}

	// 验证DNS服务器
	for _, dns := range config.DNSServers {
		if net.ParseIP(dns) == nil {
			return fmt.Errorf("无效的DNS服务器地址: %s", dns)
		}
	}

	// 验证IP映射
	for ip, mac := range config.IPMaps {
		if net.ParseIP(ip) == nil {
			return fmt.Errorf("无效的IP地址: %s", ip)
		}
		if !n.isValidMAC(mac) {
			return fmt.Errorf("无效的MAC地址: %s", mac)
		}
	}

	return nil
}

// isValidMAC 验证MAC地址格式
func (n *NetworkManager) isValidMAC(mac string) bool {
	// 简单的MAC地址验证
	if len(mac) != 17 {
		return false
	}

	parts := strings.Split(mac, ":")
	if len(parts) != 6 {
		return false
	}

	for _, part := range parts {
		if len(part) != 2 {
			return false
		}
		for _, ch := range part {
			if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
				return false
			}
		}
	}

	return true
}

// GenerateMAC 生成MAC地址
func (n *NetworkManager) GenerateMAC() string {
	// 简单的MAC地址生成逻辑
	return "00:50:56:XX:XX:XX" // 占位符，实际需要实现
}

// ProxyManager 代理管理器
type ProxyManager struct {
	proxies map[int]*ProxyConfig
	mutex   sync.RWMutex
	logger  *logrus.Logger
}

// ProxyConfig 代理配置
type ProxyConfig struct {
	LanPort int
	LanAddr string
	WebAddr string
	WebTips string
	IsHTTPS bool
	Active  bool
	Created time.Time
}

// NewProxyManager 创建代理管理器
func NewProxyManager(logger *logrus.Logger) *ProxyManager {
	return &ProxyManager{
		proxies: make(map[int]*ProxyConfig),
		mutex:   sync.RWMutex{},
		logger:  logger,
	}
}

// AddProxy 添加代理
func (p *ProxyManager) AddProxy(config *ProxyConfig) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if _, exists := p.proxies[config.LanPort]; exists {
		return fmt.Errorf("端口 %d 已被使用", config.LanPort)
	}

	config.Active = true
	config.Created = time.Now()
	p.proxies[config.LanPort] = config

	p.logger.Infof("添加代理: %d -> %s", config.LanPort, config.WebAddr)
	return nil
}

// RemoveProxy 移除代理
func (p *ProxyManager) RemoveProxy(port int) error {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if proxy, exists := p.proxies[port]; exists {
		proxy.Active = false
		delete(p.proxies, port)
		p.logger.Infof("移除代理: %d", port)
		return nil
	}

	return fmt.Errorf("代理不存在: %d", port)
}

// GetProxy 获取代理
func (p *ProxyManager) GetProxy(port int) (*ProxyConfig, error) {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	if proxy, exists := p.proxies[port]; exists {
		return proxy, nil
	}

	return nil, fmt.Errorf("代理不存在: %d", port)
}

// GetAllProxies 获取所有代理
func (p *ProxyManager) GetAllProxies() map[int]*ProxyConfig {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	// 返回副本
	result := make(map[int]*ProxyConfig)
	for port, proxy := range p.proxies {
		proxyCopy := *proxy
		result[port] = &proxyCopy
	}
	return result
}

// GetActiveProxies 获取活跃代理
func (p *ProxyManager) GetActiveProxies() map[int]*ProxyConfig {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	result := make(map[int]*ProxyConfig)
	for port, proxy := range p.proxies {
		if proxy.Active {
			proxyCopy := *proxy
			result[port] = &proxyCopy
		}
	}
	return result
}

// CheckHealth 检查代理健康状态
func (p *ProxyManager) CheckHealth(port int) bool {
	proxy, err := p.GetProxy(port)
	if err != nil {
		return false
	}

	// 简单的健康检查
	client := &http.Client{Timeout: 5 * time.Second}
	scheme := "http"
	if proxy.IsHTTPS {
		scheme = "https"
	}

	url := fmt.Sprintf("%s://%s", scheme, proxy.WebAddr)
	resp, err := client.Get(url)
	if err != nil {
		p.logger.Warnf("代理健康检查失败 %d: %v", port, err)
		return false
	}
	defer resp.Body.Close()

	return resp.StatusCode < 500
}

// CheckAllHealth 检查所有代理健康状态
func (p *ProxyManager) CheckAllHealth() map[int]bool {
	p.mutex.RLock()
	proxies := make(map[int]*ProxyConfig)
	for port, proxy := range p.proxies {
		if proxy.Active {
			proxyCopy := *proxy
			proxies[port] = &proxyCopy
		}
	}
	p.mutex.RUnlock()

	result := make(map[int]bool)
	for port := range proxies {
		result[port] = p.CheckHealth(port)
	}

	return result
}

// GetStats 获取代理统计信息
func (p *ProxyManager) GetStats() map[string]interface{} {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	activeCount := 0
	inactiveCount := 0

	for _, proxy := range p.proxies {
		if proxy.Active {
			activeCount++
		} else {
			inactiveCount++
		}
	}

	return map[string]interface{}{
		"total_count":    len(p.proxies),
		"active_count":   activeCount,
		"inactive_count": inactiveCount,
		"ports":          p.getAllPorts(),
	}
}

func (p *ProxyManager) getAllPorts() []int {
	ports := make([]int, 0, len(p.proxies))
	for port := range p.proxies {
		ports = append(ports, port)
	}
	return ports
}
