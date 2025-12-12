package monitoring

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/database"
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/host"
	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/models"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

// Monitor 监控器
type Monitor struct {
	hostManager   *host.HostManager
	db            *database.Database
	logger        *logrus.Logger
	config        MonitorConfig
	statusCache   map[string]*SystemStatus
	mutex         sync.RWMutex
	stopCh        chan struct{}
	wg            sync.WaitGroup
}

// MonitorConfig 监控配置
type MonitorConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Interval       time.Duration `yaml:"interval"`
	DataRetention  time.Duration `yaml:"data_retention"`
	EnableAlerts   bool          `yaml:"enable_alerts"`
	AlertThreshold AlertThreshold `yaml:"alert_threshold"`
	MetricsPath    string        `yaml:"metrics_path"`
	ReportsPath    string        `yaml:"reports_path"`
}

// AlertThreshold 告警阈值
type AlertThreshold struct {
	CPUUsage    float64 `yaml:"cpu_usage"`
	MemoryUsage float64 `yaml:"memory_usage"`
	DiskUsage   float64 `yaml:"disk_usage"`
	LoadAvg     float64 `yaml:"load_avg"`
}

// SystemStatus 系统状态
type SystemStatus struct {
	Timestamp     time.Time            `json:"timestamp"`
	HostCount     int                  `json:"host_count"`
	VMCount       int                  `json:"vm_count"`
	RunningVMs    int                  `json:"running_vms"`
	StoppedVMs    int                  `json:"stopped_vms"`
	TotalCPU      int64                `json:"total_cpu"`
	TotalMemory   int64                `json:"total_memory"`
	TotalDisk     int64                `json:"total_disk"`
	UsedPorts     int                  `json:"used_ports"`
	NetworkIn     int64                `json:"network_in"`
	NetworkOut    int64                `json:"network_out"`
	HostStatuses  map[string]*HostStatus `json:"host_statuses"`
	Alerts        []Alert              `json:"alerts"`
}

// HostStatus 主机状态
type HostStatus struct {
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Addr       string            `json:"addr"`
	Status     string            `json:"status"`
	Uptime     int64             `json:"uptime"`
	CPUUsage   float64           `json:"cpu_usage"`
	MemoryUsage int64            `json:"memory_usage"`
	DiskUsage  float64           `json:"disk_usage"`
	NetworkIn  int64             `json:"network_in"`
	NetworkOut int64             `json:"network_out"`
	LoadAvg    float64           `json:"load_avg"`
	Temp       float64           `json:"temperature"`
	VMs        map[string]*VMStatus `json:"vms"`
}

// VMStatus 虚拟机状态
type VMStatus struct {
	UUID       string  `json:"uuid"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	CPUUsage   float64 `json:"cpu_usage"`
	MemoryUsage int64  `json:"memory_usage"`
	DiskUsage  float64 `json:"disk_usage"`
	NetworkIn  int64   `json:"network_in"`
	NetworkOut int64   `json:"network_out"`
	FluUsage   float64 `json:"flu_usage"`
	Uptime     int64   `json:"uptime"`
}

// Alert 告警
type Alert struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Level     string    `json:"level"`
	Target    string    `json:"target"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
	Acked     bool      `json:"acked"`
}

// NewMonitor 创建监控器
func NewMonitor(hostManager *host.HostManager, db *database.Database, logger *logrus.Logger) *Monitor {
	return &Monitor{
		hostManager: hostManager,
		db:          db,
		logger:      logger,
		config: MonitorConfig{
			Enabled:      true,
			Interval:     30 * time.Second,
			DataRetention: 7 * 24 * time.Hour,
			EnableAlerts: true,
			MetricsPath:  "./data/metrics",
			ReportsPath:  "./data/reports",
			AlertThreshold: AlertThreshold{
				CPUUsage:    80.0,
				MemoryUsage: 80.0,
				DiskUsage:   80.0,
				LoadAvg:     2.0,
			},
		},
		statusCache: make(map[string]*SystemStatus),
		stopCh:     make(chan struct{}),
	}
}

// Start 启动监控
func (m *Monitor) Start() error {
	if !m.config.Enabled {
		m.logger.Info("监控已禁用")
		return nil
	}

	// 创建必要的目录
	if err := m.createDirectories(); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}

	// 启动监控协程
	m.wg.Add(1)
	go m.monitorLoop()

	m.logger.Info("监控器已启动")
	return nil
}

// Stop 停止监控
func (m *Monitor) Stop() {
	close(m.stopCh)
	m.wg.Wait()
	m.logger.Info("监控器已停止")
}

// monitorLoop 监控循环
func (m *Monitor) monitorLoop() {
	defer m.wg.Done()

	ticker := time.NewTicker(m.config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.collectMetrics()
		case <-m.stopCh:
			return
		}
	}
}

// collectMetrics 收集指标
func (m *Monitor) collectMetrics() {
	start := time.Now()

	// 收集系统状态
	status := m.collectSystemStatus()

	// 缓存状态
	m.mutex.Lock()
	m.statusCache["current"] = status
	m.mutex.Unlock()

	// 保存指标到文件
	if err := m.saveMetrics(status); err != nil {
		m.logger.Errorf("保存指标失败: %v", err)
	}

	// 检查告警
	if m.config.EnableAlerts {
		m.checkAlerts(status)
	}

	// 清理过期数据
	m.cleanupOldData()

	m.logger.Debugf("指标收集完成，耗时: %v", time.Since(start))
}

// collectSystemStatus 收集系统状态
func (m *Monitor) collectSystemStatus() *SystemStatus {
	status := &SystemStatus{
		Timestamp:    time.Now(),
		HostStatuses: make(map[string]*HostStatus),
		Alerts:       make([]Alert, 0),
	}

	hosts := m.hostManager.GetAllHosts()
	status.HostCount = len(hosts)

	totalCPU := int64(0)
	totalMemory := int64(0)
	totalDisk := int64(0)
	totalNetworkIn := int64(0)
	totalNetworkOut := int64(0)

	for hsName, server := range hosts {
		hostStatus := m.collectHostStatus(hsName, server)
		status.HostStatuses[hsName] = hostStatus

		totalCPU += int64(hostStatus.CPUUsage * 100) // 转换为整数
		totalMemory += hostStatus.MemoryUsage
		totalDisk += hostStatus.DiskUsage
		totalNetworkIn += hostStatus.NetworkIn
		totalNetworkOut += hostStatus.NetworkOut

		status.VMCount += len(hostStatus.VMs)

		for _, vm := range hostStatus.VMs {
			if vm.Status == "running" {
				status.RunningVMs++
			} else {
				status.StoppedVMs++
			}
		}
	}

	status.TotalCPU = totalCPU
	status.TotalMemory = totalMemory
	status.TotalDisk = totalDisk
	status.NetworkIn = totalNetworkIn
	status.NetworkOut = totalNetworkOut

	return status
}

// collectHostStatus 收集主机状态
func (m *Monitor) collectHostStatus(hsName string, server host.BaseServer) *HostStatus {
	config := server.GetConfig()
	hsStatus := server.GetHostStatus()

	hostStatus := &HostStatus{
		Name:   hsName,
		Type:   config.ServerType,
		Addr:   config.ServerAddr,
		Status: "running", // 默认状态
		VMs:    make(map[string]*VMStatus),
	}

	if hsStatus != nil && hsStatus.HostStatus != nil {
		hwStatus := hsStatus.HostStatus
		hostStatus.Uptime = hsStatus.Uptime
		hostStatus.CPUUsage = hwStatus.CPUUsage
		hostStatus.MemoryUsage = hwStatus.MemUsage
		hostStatus.DiskUsage = int64(hwStatus.DiskUsage)
		hostStatus.NetworkIn = hwStatus.NetworkIn
		hostStatus.NetworkOut = hwStatus.NetworkOut
		hostStatus.LoadAvg = hwStatus.LoadAvg
		hostStatus.Temp = hwStatus.Temperature
		hostStatus.Status = hwStatus.Status
	}

	// 收集虚拟机状态
	vmConfigs := server.GetAllVMs()
	for vmUUID, vmConfig := range vmConfigs {
		vmStatus := server.VMStatus(vmUUID)
		vmInfo := &VMStatus{
			UUID: vmUUID,
			Name: vmConfig.VMName,
		}

		if len(vmStatus) > 0 {
			latestStatus := vmStatus[len(vmStatus)-1]
			vmInfo.Status = latestStatus.Status
			vmInfo.CPUUsage = latestStatus.CPUUsage
			vmInfo.MemoryUsage = latestStatus.MemUsage
			vmInfo.DiskUsage = int64(latestStatus.DiskUsage)
			vmInfo.NetworkIn = latestStatus.NetworkIn
			vmInfo.NetworkOut = latestStatus.NetworkOut
			vmInfo.FluUsage = latestStatus.FluUsage
		}

		hostStatus.VMs[vmUUID] = vmInfo
	}

	return hostStatus
}

// checkAlerts 检查告警
func (m *Monitor) checkAlerts(status *SystemStatus) {
	alerts := make([]Alert, 0)

	// 检查主机告警
	for hsName, hostStatus := range status.HostStatuses {
		if hostStatus.CPUUsage > m.config.AlertThreshold.CPUUsage {
			alerts = append(alerts, Alert{
				ID:        generateAlertID(),
				Type:      "cpu",
				Level:     "warning",
				Target:    hsName,
				Message:   fmt.Sprintf("主机 %s CPU使用率过高: %.1f%%", hsName, hostStatus.CPUUsage),
				Timestamp: time.Now(),
			})
		}

		if float64(hostStatus.MemoryUsage) > float64(m.config.AlertThreshold.MemoryUsage) {
			alerts = append(alerts, Alert{
				ID:        generateAlertID(),
				Type:      "memory",
				Level:     "warning",
				Target:    hsName,
				Message:   fmt.Sprintf("主机 %s 内存使用率过高: %.1f%%", hsName, float64(hostStatus.MemoryUsage)),
				Timestamp: time.Now(),
			})
		}

		if hostStatus.LoadAvg > m.config.AlertThreshold.LoadAvg {
			alerts = append(alerts, Alert{
				ID:        generateAlertID(),
				Type:      "load",
				Level:     "warning",
				Target:    hsName,
				Message:   fmt.Sprintf("主机 %s 负载过高: %.2f", hsName, hostStatus.LoadAvg),
				Timestamp: time.Now(),
			})
		}

		// 检查虚拟机告警
		for vmUUID, vmStatus := range hostStatus.VMs {
			if vmStatus.CPUUsage > m.config.AlertThreshold.CPUUsage {
				alerts = append(alerts, Alert{
					ID:        generateAlertID(),
					Type:      "vm_cpu",
					Level:     "warning",
					Target:    fmt.Sprintf("%s/%s", hsName, vmUUID),
					Message:   fmt.Sprintf("虚拟机 %s CPU使用率过高: %.1f%%", vmStatus.Name, vmStatus.CPUUsage),
					Timestamp: time.Now(),
				})
			}
		}
	}

	status.Alerts = alerts

	// 记录告警日志
	for _, alert := range alerts {
		m.logger.Warnf("告警: %s", alert.Message)
	}
}

// saveMetrics 保存指标
func (m *Monitor) saveMetrics(status *SystemStatus) error {
	filename := fmt.Sprintf("metrics_%s.json", status.Timestamp.Format("20060102_150405"))
	filepath := filepath.Join(m.config.MetricsPath, filename)

	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化指标失败: %w", err)
	}

	return os.WriteFile(filepath, data, 0644)
}

// cleanupOldData 清理过期数据
func (m *Monitor) cleanupOldData() {
	cutoff := time.Now().Add(-m.config.DataRetention)

	// 清理指标文件
	if err := filepath.Walk(m.config.MetricsPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() && info.ModTime().Before(cutoff) {
			return os.Remove(path)
		}

		return nil
	}); err != nil {
		m.logger.Errorf("清理指标文件失败: %v", err)
	}

	// 清理数据库中的旧数据
	if err := m.db.Cleanup(int(m.config.DataRetention.Hours() / 24)); err != nil {
		m.logger.Errorf("清理数据库失败: %v", err)
	}
}

// createDirectories 创建必要的目录
func (m *Monitor) createDirectories() error {
	dirs := []string{m.config.MetricsPath, m.config.ReportsPath}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("创建目录 %s 失败: %w", dir, err)
		}
	}

	return nil
}

// GetCurrentStatus 获取当前状态
func (m *Monitor) GetCurrentStatus() *SystemStatus {
	m.mutex.RLock()
	defer m.mutex.RUnlock()

	if status, exists := m.statusCache["current"]; exists {
		return status
	}

	return &SystemStatus{Timestamp: time.Now()}
}

// GetStatusHistory 获取状态历史
func (m *Monitor) GetStatusHistory(limit int) []*SystemStatus {
	history := make([]*SystemStatus, 0)

	// 从文件中读取历史数据
	files, err := os.ReadDir(m.config.MetricsPath)
	if err != nil {
		m.logger.Errorf("读取指标目录失败: %v", err)
		return history
	}

	// 按时间排序
	for i := len(files) - 1; i >= 0 && len(history) < limit; i-- {
		file := files[i]
		if file.IsDir() {
			continue
		}

		filepath := filepath.Join(m.config.MetricsPath, file.Name())
		data, err := os.ReadFile(filepath)
		if err != nil {
			continue
		}

		var status SystemStatus
		if err := json.Unmarshal(data, &status); err != nil {
			continue
		}

		history = append(history, &status)
	}

	return history
}

// GenerateReport 生成报告
func (m *Monitor) GenerateReport() (string, error) {
	status := m.GetCurrentStatus()

	report := map[string]interface{}{
		"generated_at": time.Now(),
		"system_info": map[string]interface{}{
			"os":           runtime.GOOS,
			"arch":         runtime.GOARCH,
			"go_version":   runtime.Version(),
			"uptime":       time.Since(time.Now().Add(-time.Hour)), // 占位符
		},
		"current_status": status,
		"alerts":         status.Alerts,
	}

	// 生成报告文件
	filename := fmt.Sprintf("report_%s.yaml", time.Now().Format("20060102_150405"))
	filepath := filepath.Join(m.config.ReportsPath, filename)

	data, err := yaml.Marshal(report)
	if err != nil {
		return "", fmt.Errorf("序列化报告失败: %w", err)
	}

	if err := os.WriteFile(filepath, data, 0644); err != nil {
		return "", fmt.Errorf("保存报告失败: %w", err)
	}

	return filepath, nil
}

// generateAlertID 生成告警ID
func generateAlertID() string {
	return fmt.Sprintf("alert_%d", time.Now().UnixNano())
}

// LoadConfig 加载配置
func (m *Monitor) LoadConfig(configPath string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			m.logger.Info("监控配置文件不存在，使用默认配置")
			return nil
		}
		return fmt.Errorf("读取配置文件失败: %w", err)
	}

	if err := yaml.Unmarshal(data, &m.config); err != nil {
		return fmt.Errorf("解析配置文件失败: %w", err)
	}

	m.logger.Info("监控配置加载成功")
	return nil
}

// SaveConfig 保存配置
func (m *Monitor) SaveConfig(configPath string) error {
	data, err := yaml.Marshal(m.config)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("保存配置失败: %w", err)
	}

	m.logger.Info("监控配置保存成功")
	return nil
}