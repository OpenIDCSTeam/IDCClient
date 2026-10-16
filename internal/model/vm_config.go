// Package model 定义所有数据模型结构体
package model

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

// VMPowers 虚拟机电源状态枚举
type VMPowers string

const (
	VMPowerUnknown VMPowers = "UNKNOWN" // 未知状态
	VMPowerStarted VMPowers = "STARTED" // 运行中
	VMPowerStopped VMPowers = "STOPPED" // 已关机
	VMPowerSuspend VMPowers = "SUSPEND" // 已暂停
	VMPowerOnOpen  VMPowers = "ON_OPEN" // 启动中
	VMPowerOnStop  VMPowers = "ON_STOP" // 关机中
	VMPowerOnSave  VMPowers = "ON_SAVE" // 暂停中
	VMPowerOnWake  VMPowers = "ON_WAKE" // 唤醒中
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
//
// 字段与 Python 版 MainObject/Config/NCConfig.py 的 __save__ 输出完全一致，
// 前端 HostManage.tsx / VMConfig 按这些 key 读写。
type NCConfig struct {
	NicName string `json:"nic_name"` // 网卡名称
	// NicType 网卡类型：nat / pub，决定使用哪张网桥与哪个IP池
	// 对应 Python 版 NCConfig.nic_type（PVE 的 net_conf 用它查 hs_config.network_<type>）
	NicType string   `json:"nic_type"`
	MacAddr string   `json:"mac_addr"` // MAC地址
	IP4Addr string   `json:"ip4_addr"` // IPv4地址
	IP6Addr string   `json:"ip6_addr"` // IPv6地址
	NicGate string   `json:"nic_gate"` // 网关地址
	NicMask string   `json:"nic_mask"` // 子网掩码，默认 255.255.255.0
	DNSAddr []string `json:"dns_addr"` // DNS服务器列表
}

// EffectiveNicType 返回生效的网卡类型，缺省为 nat（与 Python 版一致）
func (n NCConfig) EffectiveNicType() string {
	if n.NicType == "" {
		return "nat"
	}
	return n.NicType
}

// EffectiveMask 返回生效的子网掩码，缺省 255.255.255.0（与 Python 版默认值一致）
func (n NCConfig) EffectiveMask() string {
	if n.NicMask == "" {
		return "255.255.255.0"
	}
	return n.NicMask
}

// SendMAC 按 IPv4/IPv6 推导 MAC 地址（与 Python 版 NCConfig.send_mac 完全一致）。
//
// 关键：MAC 由 IP 确定性推导，因此路由器上的 DHCP 静态绑定（IP↔MAC）
// 才能与虚拟机上真实生效的网卡地址一致；若两者不一致，DHCP 会分配失败。
func (n *NCConfig) SendMAC() string {
	if ip := strings.TrimSpace(n.IP4Addr); ip != "" {
		parts := strings.Split(ip, ".")
		if len(parts) == 4 {
			octets := make([]string, 0, 4)
			ok := true
			for _, p := range parts {
				v, err := strconv.Atoi(strings.TrimSpace(p))
				if err != nil || v < 0 || v > 255 {
					ok = false
					break
				}
				octets = append(octets, fmt.Sprintf("%02x", v))
			}
			if ok {
				suffix := strings.Join(octets, ":")
				switch {
				case strings.HasPrefix(ip, "192"):
					return "00:1c:" + suffix
				case strings.HasPrefix(ip, "172"):
					return "cc:d9:" + suffix
				case strings.HasPrefix(ip, "10"):
					return "10:f6:" + suffix
				case strings.HasPrefix(ip, "100"):
					return "00:1e:" + suffix
				default:
					return "00:00:" + suffix
				}
			}
		}
	}

	// 回退到 IPv6：取后 48 位（12 个十六进制字符）
	if ip6 := strings.TrimSpace(n.IP6Addr); ip6 != "" {
		clean := strings.ToLower(strings.ReplaceAll(ip6, ":", ""))
		if len(clean) >= 12 {
			hex := clean[len(clean)-12:]
			parts := make([]string, 0, 6)
			for i := 0; i < 12; i += 2 {
				parts = append(parts, hex[i:i+2])
			}
			return strings.Join(parts, ":")
		}
	}

	// 无 IP：生成随机 MAC（前缀 00:1C）
	buf := make([]byte, 4)
	if _, err := crand.Read(buf); err != nil {
		seed := time.Now().UnixNano()
		for i := range buf {
			buf[i] = byte(seed >> ((i % 8) * 8))
		}
	}
	return fmt.Sprintf("00:1C:%02x:%02x:%02x:%02x", buf[0], buf[1], buf[2], buf[3])
}

// EnsureMAC 在 MAC 为空时按 IP 推导并写回（对应 Python __load__ 的行为）
func (n *NCConfig) EnsureMAC() {
	if strings.TrimSpace(n.MacAddr) == "" {
		n.MacAddr = n.SendMAC()
	}
}

// SDConfig 数据盘配置
type SDConfig struct {
	HddName string `json:"hdd_name"` // 磁盘名称
	HddSize int    `json:"hdd_size"` // 磁盘大小(MB)
	HddFlag int    `json:"hdd_flag"` // 挂载状态(0=未挂载,1=已挂载)
	// HddType 磁盘介质类型：0=HDD，1=SSD。
	// 前端「添加数据盘 / 挂载数据盘」表单会下发该字段并在列表中显示介质类型
	// （DockDetail.tsx: info.hdd_type === 1 ? 'SSD' : 'HDD'）；
	// 模型缺失时字段被静默丢弃，表现为用户选了 SSD 仍然显示 HDD。
	HddType int    `json:"hdd_type"`
	HddPath string `json:"hdd_path"` // 磁盘路径 / 平台卷标识(如 local-lvm:vm-100-disk-1)
	// HddScsi 平台设备位（如 Proxmox 的 scsi1、QEMU 的 vdb）。
	// 与 Python 版 SDConfig.hdd_scsi 对应，删除/移交磁盘时用于精确定位设备，
	// 避免误删系统盘或其它数据盘。
	HddScsi string `json:"hdd_scsi"`
	// HddFile 磁盘文件名（Python 版 SDConfig.hdd_file）。
	// 与 Python 后端互推（/api/sync/push）时保留该字段，避免往返丢失。
	HddFile string `json:"hdd_file,omitempty"`
}

// UnmarshalJSON 解析时把 Python 的 hdd_file 兜底到 HddPath，
// 使各驱动读取 HddPath 时也能拿到 Python 侧写入的盘路径。
func (s *SDConfig) UnmarshalJSON(data []byte) error {
	type alias SDConfig
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*s = SDConfig(a)
	if s.HddPath == "" && s.HddFile != "" {
		s.HddPath = s.HddFile
	}
	return nil
}

// MarshalJSON 序列化时回填 hdd_file，保证 Python 侧载入后不丢盘路径。
func (s SDConfig) MarshalJSON() ([]byte, error) {
	type alias SDConfig
	a := alias(s)
	if a.HddFile == "" {
		a.HddFile = a.HddPath
	}
	return json.Marshal(a)
}

// IMConfig ISO镜像配置
type IMConfig struct {
	IsoName string `json:"iso_name"` // 挂载名称(key)
	IsoFile string `json:"iso_file"` // 镜像文件名
	IsoPath string `json:"iso_path"` // 镜像路径
	// IsoHint 镜像说明（Python 版 IMConfig.iso_hint），同步往返时保留。
	IsoHint string `json:"iso_hint,omitempty"`
}

// UnmarshalJSON 解析时把 iso_file / iso_hint 兜底到 IsoPath。
func (i *IMConfig) UnmarshalJSON(data []byte) error {
	type alias IMConfig
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*i = IMConfig(a)
	if i.IsoPath == "" {
		if i.IsoFile != "" {
			i.IsoPath = i.IsoFile
		} else if i.IsoHint != "" {
			i.IsoPath = i.IsoHint
		}
	}
	return nil
}

// MarshalJSON 序列化时回填 iso_file，保证 Python 侧能取到镜像路径。
func (i IMConfig) MarshalJSON() ([]byte, error) {
	type alias IMConfig
	a := alias(i)
	if a.IsoFile == "" {
		a.IsoFile = a.IsoPath
	}
	return json.Marshal(a)
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
	PwrEdits  bool `json:"pwr_edits"` // 允许编辑电源
	PwdEdits  bool `json:"pwd_edits"` // 允许编辑密码
	SysEdits  bool `json:"sys_edits"` // 允许编辑系统
	NicEdits  bool `json:"nic_edits"` // 允许编辑网卡
	IsoEdits  bool `json:"iso_edits"` // 允许编辑光盘
	HddEdits  bool `json:"hdd_edits"` // 允许编辑硬盘
	NetEdits  bool `json:"net_edits"` // 允许编辑网络
	WebEdits  bool `json:"web_edits"` // 允许编辑网页
	VncEdits  bool `json:"vnc_edits"` // 允许控制桌面
	PciEdits  bool `json:"pci_edits"` // 允许编辑PCIe
	UsbEdits  bool `json:"usb_edits"` // 允许编辑USBs
	VmBackup  bool `json:"vm_backup"` // 允许备份还原
	EfiEdits  bool `json:"efi_edits"` // 允许管理启动
	VmModify  bool `json:"vm_modify"` // 允许修改配置
	VmDelete  bool `json:"vm_delete"` // 允许删除实例
	Firewalls bool `json:"firewalls"` // 允许编辑防火墙
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
//
// 该数值与 Python 后端 get_temp_token 中的 TEMP_USER_MASK(35287) 完全一致：
// 35287 = 全量 65535 - 8(NIC_EDITS) - 32(HDD_EDITS) - 512(PCI_EDITS)
//   - 1024(USB_EDITS) - 4096(EFI_EDITS) - 8192(VM_MODIFY) - 16384(VM_DELETE)
//
// 即保留 编辑电源/密码/系统/光盘/网络/网页/控制桌面/备份还原/编辑网关。
//
// 访问密钥与临时凭据登录后，前端 user_permissions 直接读取该掩码，
// 因此这两个入口的权限天然一致。
func VirtualUserMask() UserMask {
	return UserMaskFromBits(35287)
}

// UserMaskFromBits 根据 16 位权限掩码还原 UserMask
// 位序与 Python 版 UserMask 一致：PwrEdits=1, PwdEdits=2, SysEdits=4, ... Firewalls=32768
func UserMaskFromBits(bits int) UserMask {
	return UserMask{
		PwrEdits:  bits&1 != 0,
		PwdEdits:  bits&2 != 0,
		SysEdits:  bits&4 != 0,
		NicEdits:  bits&8 != 0,
		IsoEdits:  bits&16 != 0,
		HddEdits:  bits&32 != 0,
		NetEdits:  bits&64 != 0,
		WebEdits:  bits&128 != 0,
		VncEdits:  bits&256 != 0,
		PciEdits:  bits&512 != 0,
		UsbEdits:  bits&1024 != 0,
		VmBackup:  bits&2048 != 0,
		EfiEdits:  bits&4096 != 0,
		VmModify:  bits&8192 != 0,
		VmDelete:  bits&16384 != 0,
		Firewalls: bits&32768 != 0,
	}
}

// ToBits 将 UserMask 转为 16 位掩码（与 Python 版 UserMask 数值一致）
func (m UserMask) ToBits() int {
	bits := 0
	if m.PwrEdits {
		bits |= 1
	}
	if m.PwdEdits {
		bits |= 2
	}
	if m.SysEdits {
		bits |= 4
	}
	if m.NicEdits {
		bits |= 8
	}
	if m.IsoEdits {
		bits |= 16
	}
	if m.HddEdits {
		bits |= 32
	}
	if m.NetEdits {
		bits |= 64
	}
	if m.WebEdits {
		bits |= 128
	}
	if m.VncEdits {
		bits |= 256
	}
	if m.PciEdits {
		bits |= 512
	}
	if m.UsbEdits {
		bits |= 1024
	}
	if m.VmBackup {
		bits |= 2048
	}
	if m.EfiEdits {
		bits |= 4096
	}
	if m.VmModify {
		bits |= 8192
	}
	if m.VmDelete {
		bits |= 16384
	}
	if m.Firewalls {
		bits |= 32768
	}
	return bits
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
	// VmDeleted 扫描时平台侧已不存在（软删除）。
	// 与 Python 版 _mark_missing_vms 一致：只标记不物理删除，
	// 保留 own_all / nat_all / web_all 等业务字段以便平台侧恢复后复用。
	VmDeleted bool   `json:"vm_deleted"`
	OsName    string `json:"os_name"` // 操作系统名
	OsPass    string `json:"os_pass"` // 系统密码
	OsUser    string `json:"os_user"` // 系统用户名（默认 root / Administrator）
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
	NicAll  map[string]NCConfig `json:"nic_all"` // 网卡配置
	HddAll  map[string]SDConfig `json:"hdd_all"` // 数据盘配置
	IsoAll  map[string]IMConfig `json:"iso_all"` // ISO配置
	PciAll  map[string]VFConfig `json:"pci_all"` // PCI设备
	UsbAll  map[string]USBInfos `json:"usb_all"` // USB设备
	EfiAll  []BootOpts          `json:"efi_all"` // 启动项
	NatAll  []PortData          `json:"nat_all"` // NAT端口
	WebAll  []WebProxy          `json:"web_all"` // 反向代理
	Backups []VMBackup          `json:"backups"` // 备份列表
	OwnAll  map[string]UserMask `json:"own_all"` // 所有者权限
	// 命令执行相关（新增）
	VmCmd       map[string]interface{} `json:"vm_cmd,omitempty"`        // 待执行命令
	VmCmdResult map[string]interface{} `json:"vm_cmd_result,omitempty"` // 命令执行结果
	// 驱动扩展数据（不持久化，运行时缓存）
	ExtData map[string]interface{} `json:"-"` // 驱动扩展数据
}

// NewVMConfig 创建默认虚拟机配置
func NewVMConfig() *VMConfig {
	return &VMConfig{
		VmFlag:  VMPowerUnknown,
		VcPort:  randomVNCPort(),
		CpuNum:  2,
		MemNum:  2048,
		HddNum:  8192,
		HddIop:  1000,
		BakNum:  1,
		IsoNum:  1,
		DatNum:  1,
		SpeedU:  100,
		SpeedD:  100,
		NatNum:  100,
		WebNum:  100,
		FluNum:  102400,
		FluRst:  []int{31, 10, 10},
		NicAll:  make(map[string]NCConfig),
		HddAll:  make(map[string]SDConfig),
		IsoAll:  make(map[string]IMConfig),
		PciAll:  make(map[string]VFConfig),
		UsbAll:  make(map[string]USBInfos),
		EfiAll:  []BootOpts{},
		NatAll:  []PortData{},
		WebAll:  []WebProxy{},
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
