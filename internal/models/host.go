package models

import (
	"encoding/json"
	"time"
)

// HSConfig 主机配置结构体
type HSConfig struct {
	ServerName   string            `json:"server_name" db:"server_name"`
	ServerType   string            `json:"server_type" db:"server_type"`
	ServerAddr   string            `json:"server_addr" db:"server_addr"`
	ServerUser   string            `json:"server_user" db:"server_user"`
	ServerPass   string            `json:"server_pass" db:"server_pass"`
	FilterName   string            `json:"filter_name" db:"filter_name"`
	ImagesPath   string            `json:"images_path" db:"images_path"`
	SystemPath   string            `json:"system_path" db:"system_path"`
	BackupPath   string            `json:"backup_path" db:"backup_path"`
	ExternPath   string            `json:"extern_path" db:"extern_path"`
	LaunchPath   string            `json:"launch_path" db:"launch_path"`
	NetworkNAT   string            `json:"network_nat" db:"network_nat"`
	NetworkPub   string            `json:"network_pub" db:"network_pub"`
	IKuaiAddr    string            `json:"i_kuai_addr" db:"i_kuai_addr"`
	IKuaiUser    string            `json:"i_kuai_user" db:"i_kuai_user"`
	IKuaiPass    string            `json:"i_kuai_pass" db:"i_kuai_pass"`
	PortsStart   int               `json:"ports_start" db:"ports_start"`
	PortsClose   int               `json:"ports_close" db:"ports_close"`
	RemotePort   int               `json:"remote_port" db:"remote_port"`
	SystemMaps   map[string]string `json:"system_maps" db:"system_maps"`
	PublicAddr   []string          `json:"public_addr" db:"public_addr"`
	ExtendData   map[string]string `json:"extend_data" db:"extend_data"`
	ServerDNSS   []string          `json:"server_dnss" db:"server_dnss"`
	LimitsNums   int               `json:"limits_nums" db:"limits_nums"`
	IPAddrMaps   map[string]string `json:"ipaddr_maps" db:"ipaddr_maps"`
	IPAddrDNSS   []string          `json:"ipaddr_dnss" db:"ipaddr_dnss"`
	CreatedAt    *time.Time        `json:"created_at,omitempty" db:"created_at"`
	UpdatedAt    *time.Time        `json:"updated_at,omitempty" db:"updated_at"`
}

// NCConfig 网卡配置结构体
type NCConfig struct {
	NicType  string   `json:"nic_type"`
	IP4Addr  string   `json:"ip4_addr"`
	IP6Addr  string   `json:"ip6_addr"`
	NicGate  string   `json:"nic_gate"`
	NicMask  string   `json:"nic_mask"`
	DNSAddr  []string `json:"dns_addr"`
	MACAddr  string   `json:"mac_addr"`
}

// WebProxy 代理配置结构体
type WebProxy struct {
	LanPort  int    `json:"lan_port"`
	LanAddr  string `json:"lan_addr"`
	WebAddr  string `json:"web_addr"`
	WebTips  string `json:"web_tips"`
	IsHTTPS  bool   `json:"is_https"`
}

// PortData 端口数据结构体
type PortData struct {
	LanPort  int    `json:"lan_port"`
	WanPort  int    `json:"wan_port"`
	LanAddr  string `json:"lan_addr"`
	NatTips  string `json:"nat_tips"`
}

// VMConfig 虚拟机配置结构体
type VMConfig struct {
	VMUUID     string               `json:"vm_uuid"`
	VMName     string               `json:"vm_name"`
	OSName     string               `json:"os_name"`
	OSPass     string               `json:"os_pass"`
	CPUNum     int                  `json:"cpu_num"`
	RAMNum     int                  `json:"ram_num"`
	HDDNum     int                  `json:"hdd_num"`
	NicAll     map[string]NCConfig  `json:"nic_all"`
	NatAll     []PortData           `json:"nat_all"`
	WebAll     []WebProxy           `json:"web_all"`
	VCPort     int                  `json:"vc_port"`
	VCPass     string               `json:"vc_pass"`
	VCEnabled  bool                 `json:"vc_enabled"`
	AutoStart  bool                 `json:"auto_start"`
	BackupMode string               `json:"backup_mode"`
	CreatedAt  *time.Time           `json:"created_at,omitempty"`
	UpdatedAt  *time.Time           `json:"updated_at,omitempty"`
}

// VMPowers 虚拟机电源操作枚举
type VMPowers string

const (
	VMPowerStart     VMPowers = "start"
	VMPowerStop      VMPowers = "stop"
	VMPowerHardStop  VMPowers = "hard_stop"
	VMPowerReset     VMPowers = "reset"
	VMPowerHardReset VMPowers = "hard_reset"
	VMPowerPause     VMPowers = "pause"
	VMPowerResume    VMPowers = "resume"
	VMPowerShutdown  VMPowers = "shutdown"
)

// HWStatus 硬件状态结构体
type HWStatus struct {
	CPUUsage    float64 `json:"cpu_usage"`
	MemUsage    int64   `json:"mem_usage"`
	DiskUsage   float64 `json:"disk_usage"`
	NetworkIn   int64   `json:"network_in"`
	NetworkOut  int64   `json:"network_out"`
	FluUsage    float64 `json:"flu_usage"`
	Timestamp   time.Time `json:"timestamp"`
	Status      string  `json:"status"`
	Uptime      int64   `json:"uptime"`
	Processes   int     `json:"processes"`
	LoadAvg     float64 `json:"load_avg"`
	Temperature float64 `json:"temperature"`
}

// HSStatus 主机状态结构体
type HSStatus struct {
	HostStatus *HWStatus `json:"host_status"`
	Version    string    `json:"version"`
	Uptime     int64     `json:"uptime"`
	Platform   string    `json:"platform"`
	Arch       string    `json:"arch"`
	CPUCores   int       `json:"cpu_cores"`
	CPUFreq    float64   `json:"cpu_freq"`
	TotalMem   int64     `json:"total_mem"`
	TotalDisk  int64     `json:"total_disk"`
	Networks   []string  `json:"networks"`
}

// ToJSON 将结构体转换为JSON字符串
func (h *HSConfig) ToJSON() string {
	data, _ := json.Marshal(h)
	return string(data)
}

// ToJSON 将结构体转换为JSON字符串
func (v *VMConfig) ToJSON() string {
	data, _ := json.Marshal(v)
	return string(data)
}

// ToJSON 将结构体转换为JSON字符串
func (h *HWStatus) ToJSON() string {
	data, _ := json.Marshal(h)
	return string(data)
}

// ToJSON 将结构体转换为JSON字符串
func (h *HSStatus) ToJSON() string {
	data, _ := json.Marshal(h)
	return string(data)
}

// GenerateMAC 生成MAC地址
func (n *NCConfig) GenerateMAC() string {
	// 简单的MAC地址生成逻辑，实际应该更复杂
	return "00:50:56:XX:XX:XX" // 占位符，实际需要实现
}

// SendMAC 发送MAC地址（生成）
func (n *NCConfig) SendMAC() string {
	if n.MACAddr == "" {
		n.MACAddr = n.GenerateMAC()
	}
	return n.MACAddr
}