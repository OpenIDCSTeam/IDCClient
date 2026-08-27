// Package model 主机配置数据模型
package model

import "encoding/json"

// OSConfig 操作系统镜像/光盘镜像条目
type OSConfig struct {
	SysName string `json:"sys_name"` // 镜像-系统显示名称
	SysFile string `json:"sys_file"` // 镜像-文件存储名称
	SysSize string `json:"sys_size"` // 允许最低磁盘值(GB)
	SysType string `json:"sys_type"` // WinNT/Linux/macOS
	SysFlag bool   `json:"sys_flag"` // 是否启用此镜像
}

// HSConfig 主机配置
type HSConfig struct {
	// 基本信息
	ServerName string `json:"server_name" db:"server_name"` // 服务器名称
	ServerType string `json:"server_type" db:"server_type"` // 服务器类型
	ServerAddr string `json:"server_addr" db:"server_addr"` // 服务器地址
	ServerUser string `json:"server_user" db:"server_user"` // 服务器用户
	ServerPass string `json:"server_pass" db:"server_pass"` // 服务器密码
	ServerPort int    `json:"server_port" db:"server_port"` // 服务器端口
	ServerArea string `json:"server_area" db:"server_area"` // 服务器区域
	FilterName string `json:"filter_name" db:"filter_name"` // 过滤器名称
	EnableHost bool   `json:"enable_host" db:"enable_host"` // 是否启用
	// 存储信息
	ImagesPath string `json:"images_path" db:"images_path"` // 系统镜像路径
	DvdromPath string `json:"dvdrom_path" db:"dvdrom_path"` // 光盘镜像路径
	SystemPath string `json:"system_path" db:"system_path"` // 虚拟机系统路径
	BackupPath string `json:"backup_path" db:"backup_path"` // 备份路径
	ExternPath string `json:"extern_path" db:"extern_path"` // 数据盘路径
	LaunchPath string `json:"launch_path" db:"launch_path"` // 二进制路径
	// 本机网络
	NetworkNat string `json:"network_nat" db:"network_nat"` // NAT网络NIC
	NetworkPub string `json:"network_pub" db:"network_pub"` // 公网NIC
	PortsStart int    `json:"ports_start" db:"ports_start"` // TCP端口起始
	PortsClose int    `json:"ports_close" db:"ports_close"` // TCP端口结束
	RemotePort int    `json:"remote_port" db:"remote_port"` // VNC服务端口
	LimitsNums int    `json:"limits_nums" db:"limits_nums"` // 最大VM数量
	// 系统映射
	SystemMaps []OSConfig `json:"system_maps"` // 系统镜像列表
	ImagesMaps []OSConfig `json:"images_maps"` // 光盘镜像列表
	// 区域网络
	IKuaiAddr  string              `json:"i_kuai_addr"` // 爱快OS地址
	IKuaiUser  string              `json:"i_kuai_user"` // 爱快OS用户
	IKuaiPass  string              `json:"i_kuai_pass"` // 爱快OS密码
	IPAddrDDNS []string            `json:"ipaddr_ddns"` // DNS服务器列表
	IPAddrMaps map[string]map[string]interface{} `json:"ipaddr_maps"` // IP地址映射
	PublicAddr []string            `json:"public_addr"` // 公共IP列表
	ExtendData map[string]interface{} `json:"extend_data"` // 扩展数据
	// 套餐划分
	ServerPlan map[string]*VMConfig `json:"server_plan"` // 套餐配置
	// 售价配置
	NCpuPrice float64 `json:"n_cpu_price" db:"n_cpu_price"` // 处理器核心单价
	NMemPrice float64 `json:"n_mem_price" db:"n_mem_price"` // 虚拟机内存单价
	NHddPrice float64 `json:"n_hdd_price" db:"n_hdd_price"` // 虚拟机硬盘单价
	NNetPrice float64 `json:"n_net_price" db:"n_net_price"` // 虚拟机带宽单价
}

// NewHSConfig 创建默认主机配置
func NewHSConfig() *HSConfig {
	return &HSConfig{
		ServerPort: 22,
		IPAddrDDNS: []string{"119.29.29.29", "223.5.5.5"},
		SystemMaps: []OSConfig{},
		ImagesMaps: []OSConfig{},
		IPAddrMaps: make(map[string]map[string]interface{}),
		PublicAddr: []string{},
		ExtendData: make(map[string]interface{}),
		ServerPlan: make(map[string]*VMConfig),
	}
}

// HSConfigFromJSON 从JSON字节解析HSConfig
func HSConfigFromJSON(data []byte) (*HSConfig, error) {
	cfg := NewHSConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	if cfg.IPAddrDDNS == nil {
		cfg.IPAddrDDNS = []string{"119.29.29.29", "223.5.5.5"}
	}
	if cfg.SystemMaps == nil {
		cfg.SystemMaps = []OSConfig{}
	}
	if cfg.ImagesMaps == nil {
		cfg.ImagesMaps = []OSConfig{}
	}
	if cfg.IPAddrMaps == nil {
		cfg.IPAddrMaps = make(map[string]map[string]interface{})
	}
	if cfg.PublicAddr == nil {
		cfg.PublicAddr = []string{}
	}
	if cfg.ExtendData == nil {
		cfg.ExtendData = make(map[string]interface{})
	}
	if cfg.ServerPlan == nil {
		cfg.ServerPlan = make(map[string]*VMConfig)
	}
	return cfg, nil
}

// HWStatus 硬件状态
type HWStatus struct {
	CpuModel  string  `json:"cpu_model"`  // CPU型号
	CpuTotal  int     `json:"cpu_total"`  // CPU核心数
	CpuUsage  int     `json:"cpu_usage"`  // CPU使用率(%)
	MemTotal  int     `json:"mem_total"`  // 内存总量(MB)
	MemUsage  int     `json:"mem_usage"`  // 内存使用(MB)
	HddTotal  int     `json:"hdd_total"`  // 磁盘总量(MB)
	HddUsage  int     `json:"hdd_usage"`  // 磁盘使用(MB)
	FluUsage  int     `json:"flu_usage"`  // 流量使用(MB)
	NetworkU  int     `json:"network_u"`  // 上行带宽(Mbps)
	NetworkD  int     `json:"network_d"`  // 下行带宽(Mbps)
	VmState   string  `json:"vm_state"`   // 虚拟机状态
	OnUpdate  int64   `json:"on_update"`  // 更新时间戳
	GpuUsage  float64 `json:"gpu_usage"`  // GPU使用率(%)
	GpuMemory int     `json:"gpu_memory"` // GPU显存(MB)
}

// ZMessage 统一消息结构
type ZMessage struct {
	Success bool        `json:"success"`  // 是否成功
	Action  string      `json:"action"`   // 操作名称
	Message string      `json:"message"`  // 消息内容
	Results interface{} `json:"results"`  // 附加数据
}

// WebUser 用户数据模型
type WebUser struct {
	ID            int    `json:"id"            db:"id"`
	Username      string `json:"username"      db:"username"`
	Password      string `json:"password"      db:"password"`
	Email         string `json:"email"         db:"email"`
	IsAdmin       bool   `json:"is_admin"      db:"is_admin"`
	IsActive      bool   `json:"is_active"     db:"is_active"`
	EmailVerified bool   `json:"email_verified" db:"email_verified"`
	VerifyToken   string `json:"verify_token"  db:"verify_token"`
	ResetToken    string `json:"reset_token"   db:"reset_token"`
	// 权限设置
	CanCreateVM bool `json:"can_create_vm" db:"can_create_vm"`
	CanDeleteVM bool `json:"can_delete_vm" db:"can_delete_vm"`
	CanModifyVM bool `json:"can_modify_vm" db:"can_modify_vm"`
	// 资源配额
	QuotaCPU           int `json:"quota_cpu"            db:"quota_cpu"`
	QuotaRAM           int `json:"quota_ram"            db:"quota_ram"`
	QuotaSSD           int `json:"quota_ssd"            db:"quota_ssd"`
	QuotaGPU           int `json:"quota_gpu"            db:"quota_gpu"`
	QuotaNatPorts      int `json:"quota_nat_ports"      db:"quota_nat_ports"`
	QuotaWebProxy      int `json:"quota_web_proxy"      db:"quota_web_proxy"`
	QuotaNatIPs        int `json:"quota_nat_ips"        db:"quota_nat_ips"`
	QuotaPubIPs        int `json:"quota_pub_ips"        db:"quota_pub_ips"`
	QuotaBandwidthUp   int `json:"quota_bandwidth_up"   db:"quota_bandwidth_up"`
	QuotaBandwidthDown int `json:"quota_bandwidth_down" db:"quota_bandwidth_down"`
	QuotaTraffic       int `json:"quota_traffic"        db:"quota_traffic"`
	// 已使用资源
	UsedCPU           int `json:"used_cpu"            db:"used_cpu"`
	UsedRAM           int `json:"used_ram"            db:"used_ram"`
	UsedSSD           int `json:"used_ssd"            db:"used_ssd"`
	UsedGPU           int `json:"used_gpu"            db:"used_gpu"`
	UsedNatPorts      int `json:"used_nat_ports"      db:"used_nat_ports"`
	UsedWebProxy      int `json:"used_web_proxy"      db:"used_web_proxy"`
	UsedNatIPs        int `json:"used_nat_ips"        db:"used_nat_ips"`
	UsedPubIPs        int `json:"used_pub_ips"        db:"used_pub_ips"`
	UsedBandwidthUp   int `json:"used_bandwidth_up"   db:"used_bandwidth_up"`
	UsedBandwidthDown int `json:"used_bandwidth_down" db:"used_bandwidth_down"`
	UsedTraffic       int `json:"used_traffic"        db:"used_traffic"`
	// 分配的主机列表
	AssignedHosts []string `json:"assigned_hosts"` // JSON数组存储
	// 时间戳
	CreatedAt string `json:"created_at" db:"created_at"`
	UpdatedAt string `json:"updated_at" db:"updated_at"`
	LastLogin string `json:"last_login" db:"last_login"`
}

// HSTasker 任务记录
type HSTasker struct {
	TaskID   string      `json:"task_id"`   // 任务ID
	HsName   string      `json:"hs_name"`   // 主机名称
	TaskType string      `json:"task_type"` // 任务类型
	TaskData interface{} `json:"task_data"` // 任务数据
	Status   string      `json:"status"`    // 任务状态
	CreatedAt string     `json:"created_at"` // 创建时间
}

// LogEntry 日志条目
type LogEntry struct {
	ID        int    `json:"id"         db:"id"`
	HsName    string `json:"hs_name"    db:"hs_name"`
	LogData   string `json:"log_data"   db:"log_data"`
	LogLevel  string `json:"log_level"  db:"log_level"`
	CreatedAt string `json:"created_at" db:"created_at"`
}
