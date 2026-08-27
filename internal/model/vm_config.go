// Package model 定义所有数据模型结构体
package model

import (
	"encoding/json"
	"math/rand"
	"strconv"
	"time"
)

// VMPowers 虚拟机电源状态枚举
type VMPowers string

const (
	VMPowerUnknown VMPowers = "UNKNOWN"  // 未知状态
	VMPowerStarted VMPowers = "STARTED"  // 运行中
	VMPowerStopped VMPowers = "STOPPED"  // 已关机
	VMPowerSuspend VMPowers = "SUSPEND"  // 已暂停
	VMPowerOnOpen  VMPowers = "ON_OPEN"  // 启动中
	VMPowerOnStop  VMPowers = "ON_STOP"  // 关机中
	VMPowerOnSave  VMPowers = "ON_SAVE"  // 暂停中
	VMPowerOnWake  VMPowers = "ON_WAKE"  // 唤醒中
	// 电源操作指令
	VMPowerSStart VMPowers = "S_START" // 软启动
	VMPowerSClose VMPowers = "S_CLOSE" // 软关机
	VMPowerSReset VMPowers = "S_RESET" // 软重启
	VMPowerHClose VMPowers = "H_CLOSE" // 强制关机
	VMPowerHReset VMPowers = "H_RESET" // 强制重启
	VMPowerAPause VMPowers = "A_PAUSE" // 暂停
	VMPowerAWaked VMPowers = "A_WAKED" // 恢复
)

// NCConfig 网卡配置
type NCConfig struct {
	NicName string   `json:"nic_name"` // 网卡名称
	MacAddr string   `json:"mac_addr"` // MAC地址
	IP4Addr string   `json:"ip4_addr"` // IPv4地址
	IP6Addr string   `json:"ip6_addr"` // IPv6地址
	DNSAddr []string `json:"dns_addr"` // DNS服务器列表
}

// SDConfig 数据盘配置
type SDConfig struct {
	HddName string `json:"hdd_name"` // 磁盘名称
	HddSize int    `json:"hdd_size"` // 磁盘大小(MB)
	HddFlag int    `json:"hdd_flag"` // 挂载状态(0=未挂载,1=已挂载)
	HddPath string `json:"hdd_path"` // 磁盘路径
}

// IMConfig ISO镜像配置
type IMConfig struct {
	IsoName string `json:"iso_name"` // 挂载名称(key)
	IsoFile string `json:"iso_file"` // 镜像文件名
	IsoPath string `json:"iso_path"` // 镜像路径
}

// VFConfig PCI设备配置
type VFConfig struct {
	PciKey  string `json:"pci_key"`  // 设备Key
	GpuUUID string `json:"gpu_uuid"` // GPU UUID
	GpuMdev string `json:"gpu_mdev"` // mdev类型
	GpuHint string `json:"gpu_hint"` // 设备描述
}

// USBInfos USB设备信息
type USBInfos struct {
	UsbKey  string `json:"usb_key"`  // 设备Key
	VidUUID string `json:"vid_uuid"` // VID
	PidUUID string `json:"pid_uuid"` // PID
	UsbHint string `json:"usb_hint"` // 设备描述
}

// PortData NAT端口转发配置
type PortData struct {
	WanPort int    `json:"wan_port"` // 外网端口
	LanPort int    `json:"lan_port"` // 内网端口
	LanAddr string `json:"lan_addr"` // 内网地址
	NatTips string `json:"nat_tips"` // 端口说明
}

// WebProxy 反向代理配置
type WebProxy struct {
	LanPort int    `json:"lan_port"` // 内网端口
	LanAddr string `json:"lan_addr"` // 内网地址
	WebAddr string `json:"web_addr"` // 域名地址
	WebTips string `json:"web_tips"` // 代理说明
	IsHTTPS bool   `json:"is_https"` // 是否HTTPS
}

// VMBackup 虚拟机备份记录
type VMBackup struct {
	BackupName string    `json:"backup_name"` // 备份文件名
	BackupTime time.Time `json:"backup_time"` // 备份时间
	BackupTips string    `json:"backup_tips"` // 备份说明
}

// BootOpts EFI启动项
type BootOpts struct {
	EfiType bool   `json:"efi_type"` // true=ISO, false=HDD
	EfiName string `json:"efi_name"` // 启动项名称
}

// UserMask 用户权限掩码（16位）
type UserMask struct {
	PwrEdits  bool `json:"pwr_edits"`  // 允许编辑电源
	PwdEdits  bool `json:"pwd_edits"`  // 允许编辑密码
	SysEdits  bool `json:"sys_edits"`  // 允许编辑系统
	NicEdits  bool `json:"nic_edits"`  // 允许编辑网卡
	IsoEdits  bool `json:"iso_edits"`  // 允许编辑光盘
	HddEdits  bool `json:"hdd_edits"`  // 允许编辑硬盘
	NetEdits  bool `json:"net_edits"`  // 允许编辑网络
	WebEdits  bool `json:"web_edits"`  // 允许编辑网页
	VncEdits  bool `json:"vnc_edits"`  // 允许控制桌面
	PciEdits  bool `json:"pci_edits"`  // 允许编辑PCIe
	UsbEdits  bool `json:"usb_edits"`  // 允许编辑USBs
	VmBackup  bool `json:"vm_backup"`  // 允许备份还原
	EfiEdits  bool `json:"efi_edits"`  // 允许管理启动
	VmModify  bool `json:"vm_modify"`  // 允许修改配置
	VmDelete  bool `json:"vm_delete"`  // 允许删除实例
	Firewalls bool `json:"firewalls"`  // 允许编辑防火墙
}

// FullUserMask 返回全权限掩码
func FullUserMask() UserMask {
	return UserMask{
		PwrEdits: true, PwdEdits: true, SysEdits: true, NicEdits: true,
		IsoEdits: true, HddEdits: true, NetEdits: true, WebEdits: true,
		VncEdits: true, PciEdits: true, UsbEdits: true, VmBackup: true,
		EfiEdits: true, VmModify: true, VmDelete: true, Firewalls: true,
	}
}

// VirtualUserMask 返回虚拟用户权限掩码（35287）
// 对应Python后端的UserMask(35287)，排除部分敏感权限
func VirtualUserMask() UserMask {
	return UserMask{
		PwrEdits: true,                     // 1
		PwdEdits: true,                     // 2
		SysEdits: true,                     // 4
		NicEdits: false,                    // 8 (排除：禁止编辑网卡)
		IsoEdits: true,                     // 16
		HddEdits: false,                    // 32 (排除：禁止编辑硬盘)
		NetEdits: true,                     // 64
		WebEdits: false,                    // 128 (排除：禁止编辑网页代理)
		VncEdits: true,                     // 256
		PciEdits: false,                    // 512 (排除：禁止PCI直通)
		UsbEdits: false,                    // 1024 (排除：禁止USB直通)
		VmBackup: true,                     // 2048
		EfiEdits: false,                    // 4096 (排除：禁止管理启动)
		VmModify: false,                    // 8192 (排除：禁止修改配置)
		VmDelete: false,                    // 16384 (排除：禁止删除实例)
		Firewalls: false,                   // 32768 (排除：禁止编辑防火墙)
	}
}

// HasPermission 检查是否有指定权限
func (m UserMask) HasPermission(action string) bool {
	switch action {
	case "pwr_edits":
		return m.PwrEdits
	case "pwd_edits":
		return m.PwdEdits
	case "sys_edits":
		return m.SysEdits
	case "nic_edits":
		return m.NicEdits
	case "iso_edits":
		return m.IsoEdits
	case "hdd_edits":
		return m.HddEdits
	case "net_edits":
		return m.NetEdits
	case "web_edits":
		return m.WebEdits
	case "vnc_edits":
		return m.VncEdits
	case "pci_edits":
		return m.PciEdits
	case "usb_edits":
		return m.UsbEdits
	case "vm_backup":
		return m.VmBackup
	case "efi_edits":
		return m.EfiEdits
	case "vm_modify":
		return m.VmModify
	case "vm_delete":
		return m.VmDelete
	case "firewalls":
		return m.Firewalls
	}
	return false
}

// VMConfig 虚拟机配置
type VMConfig struct {
	// 机器配置
	VmUUID string   `json:"vm_uuid"` // 虚拟机名-UUID
	VmFlag VMPowers `json:"vm_flag"` // 电源状态
	OsName string   `json:"os_name"` // 操作系统名
	OsPass string   `json:"os_pass"` // 系统密码
	// 远程连接
	VcPort string `json:"vc_port"` // VNC端口
	VcPass string `json:"vc_pass"` // VNC密码
	// 资源配置
	CpuNum int `json:"cpu_num"` // CPU核心数
	CpuPer int `json:"cpu_per"` // CPU使用比例
	GpuMem int `json:"gpu_mem"` // 显存(MB)
	MemNum int `json:"mem_num"` // 内存(MB)
	HddNum int `json:"hdd_num"` // 硬盘(MB)
	HddIop int `json:"hdd_iop"` // 硬盘IOPS
	// 配额配置
	BakNum int `json:"bak_num"` // 最大备份数
	IsoNum int `json:"iso_num"` // 最大光盘数
	PciNum int `json:"pci_num"` // 最大PCIe数
	UsbNum int `json:"usb_num"` // 最大USB数
	DatNum int `json:"dat_num"` // 最大数据盘数
	DatAll int `json:"dat_all"` // 数据盘总容量
	// 售价配置
	CpuPrc float64 `json:"cpu_prc"` // 处理器核心单位价格
	MemPrc float64 `json:"mem_prc"` // 虚拟机内存单位价格
	HddPrc float64 `json:"hdd_prc"` // 虚拟机硬盘单位价格
	NetPrc float64 `json:"net_prc"` // 虚拟机双向带宽价格
	// 网络配置
	SpeedU int   `json:"speed_u"` // 上行带宽(Mbps)
	SpeedD int   `json:"speed_d"` // 下行带宽(Mbps)
	NatNum int   `json:"nat_num"` // 分配端口数
	WebNum int   `json:"web_num"` // 分配代理数
	FluNum int   `json:"flu_num"` // 分配流量(MB)
	FluRst []int `json:"flu_rst"` // 流量重置配置[天数,超限MB,上次重置时间戳]
	// 附加配置
	NicAll  map[string]NCConfig  `json:"nic_all"`  // 网卡配置
	HddAll  map[string]SDConfig  `json:"hdd_all"`  // 数据盘配置
	IsoAll  map[string]IMConfig  `json:"iso_all"`  // ISO配置
	PciAll  map[string]VFConfig  `json:"pci_all"`  // PCI设备
	UsbAll  map[string]USBInfos  `json:"usb_all"`  // USB设备
	EfiAll  []BootOpts           `json:"efi_all"`  // 启动项
	NatAll  []PortData           `json:"nat_all"`  // NAT端口
	WebAll  []WebProxy           `json:"web_all"`  // 反向代理
	Backups []VMBackup           `json:"backups"`  // 备份列表
	OwnAll  map[string]UserMask  `json:"own_all"`  // 所有者权限
	// 命令执行相关（新增）
	VmCmd       map[string]interface{} `json:"vm_cmd,omitempty"`        // 待执行命令
	VmCmdResult map[string]interface{} `json:"vm_cmd_result,omitempty"` // 命令执行结果
	// 驱动扩展数据（不持久化，运行时缓存）
	ExtData map[string]interface{} `json:"-"` // 驱动扩展数据
}

// NewVMConfig 创建默认虚拟机配置
func NewVMConfig() *VMConfig {
	return &VMConfig{
		VmFlag: VMPowerUnknown,
		VcPort: randomVNCPort(),
		CpuNum: 2,
		MemNum: 2048,
		HddNum: 8192,
		HddIop: 1000,
		BakNum: 1,
		IsoNum: 1,
		DatNum: 1,
		SpeedU: 100,
		SpeedD: 100,
		NatNum: 100,
		WebNum: 100,
		FluNum: 102400,
		FluRst: []int{31, 10, 10},
		NicAll: make(map[string]NCConfig),
		HddAll: make(map[string]SDConfig),
		IsoAll: make(map[string]IMConfig),
		PciAll: make(map[string]VFConfig),
		UsbAll: make(map[string]USBInfos),
		EfiAll: []BootOpts{},
		NatAll: []PortData{},
		WebAll: []WebProxy{},
		Backups: []VMBackup{},
		OwnAll: map[string]UserMask{
			"admin": FullUserMask(),
		},
		ExtData: make(map[string]interface{}),
	}
}

// randomVNCPort 生成随机VNC端口 5900-6999
func randomVNCPort() string {
	return strconv.Itoa(5900 + rand.Intn(1100))
}

func init() {
	rand.Seed(time.Now().UnixNano())
}

// VMConfigFromJSON 从JSON字节解析VMConfig
func VMConfigFromJSON(data []byte) (*VMConfig, error) {
	cfg := NewVMConfig()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	// 确保map不为nil
	if cfg.NicAll == nil {
		cfg.NicAll = make(map[string]NCConfig)
	}
	if cfg.HddAll == nil {
		cfg.HddAll = make(map[string]SDConfig)
	}
	if cfg.IsoAll == nil {
		cfg.IsoAll = make(map[string]IMConfig)
	}
	if cfg.PciAll == nil {
		cfg.PciAll = make(map[string]VFConfig)
	}
	if cfg.UsbAll == nil {
		cfg.UsbAll = make(map[string]USBInfos)
	}
	if cfg.OwnAll == nil {
		cfg.OwnAll = map[string]UserMask{"admin": FullUserMask()}
	}
	if cfg.EfiAll == nil {
		cfg.EfiAll = []BootOpts{}
	}
	if cfg.NatAll == nil {
		cfg.NatAll = []PortData{}
	}
	if cfg.WebAll == nil {
		cfg.WebAll = []WebProxy{}
	}
	if cfg.Backups == nil {
		cfg.Backups = []VMBackup{}
	}
	if cfg.FluRst == nil {
		cfg.FluRst = []int{31, 10, 10}
	}
	if cfg.ExtData == nil {
		cfg.ExtData = make(map[string]interface{})
	}
	return cfg, nil
}
