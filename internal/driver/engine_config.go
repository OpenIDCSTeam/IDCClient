// Package driver 宿主机引擎（主机类型）元信息
// 与 Python 版 MainObject/Server/HSEngine.HEConfig 保持一致，
// 供 /api/system/engine 返回给前端渲染「服务器类型」下拉框、注意事项与可选配置项，
// 并供主机详情接口补充 ban_init / ban_edit / tab_lock 等表单约束。
package driver

import "runtime"

// EngineMeta 引擎（主机类型）元信息
// 字段与 Python HEConfig 条目一一对应（Descript -> Description 等）
type EngineMeta struct {
	Type        string            `json:"type"`
	Description string            `json:"description"`
	Enabled     bool              `json:"enabled"`
	IsRemote    bool              `json:"is_remote"`
	Platform    []string          `json:"platform"`
	CPUArch     []string          `json:"arch"`
	Optional    map[string]string `json:"options"`
	Messages    []string          `json:"messages"`
	BanInit     []string          `json:"ban_init"`
	BanEdit     []string          `json:"ban_edit"`
	TabLock     []string          `json:"tab_lock"`
}

// engineConfigs 引擎配置表（内容与 Python HSEngine.HEConfig 一致）
// 说明：QingzhouYun 与 Python 保持一致，以 Enabled=false 关闭对界面/接口的暴露
// （不出现在 /api/system/engine 的 engine_types 中）；
// 其驱动实现与 createDriver 分支完整保留，需要启用时把 Enabled 改为 true 即可。
var engineConfigs = []EngineMeta{
	{
		Type:        "VMWareSetup",
		Description: "VMWare Workstation",
		Enabled:     true,
		IsRemote:    false,
		Platform:    []string{"Windows"},
		CPUArch:     []string{"x86_64"},
		Optional:    map[string]string{},
		Messages: []string{
			"1、不支持PCI设备直通，但可分配虚拟显存",
			"2、支持USB设备直通",
		},
		BanInit: []string{"gpu_num"},
		BanEdit: []string{"gpu_num"},
		TabLock: []string{"pci"},
	},
	{
		Type:        "LxContainer",
		Description: "LinuxContainer Env",
		Enabled:     true,
		IsRemote:    true,
		Platform:    []string{"Linux"},
		CPUArch:     []string{"x86_64", "aarch64"},
		Optional:    map[string]string{},
		Messages: []string{
			"1、不支持分配GPU设备，不支持设置显存的大小",
			"2、不支持挂载ISO镜像、不支持挂载额外的硬盘",
			"3、不支持PCI和USB设备直通",
		},
		BanInit: []string{"gpu_num", "gpu_mem"},
		BanEdit: []string{"gpu_num", "gpu_mem", "flu_num", "hdd_num", "speed_u", "speed_d"},
		TabLock: []string{"hdd", "iso", "pci", "usb", "efi", "gpu"},
	},
	{
		Type:        "OCInterface",
		Description: "Docker Runtime Env",
		Enabled:     true,
		IsRemote:    true,
		Platform:    []string{"Linux", "MacOS"},
		CPUArch:     []string{"x86_64", "aarch64"},
		Optional:    map[string]string{},
		Messages: []string{
			"1、不支持分配GPU设备，不支持设置显存的大小",
			"2、不支持挂载ISO镜像、不支持挂载额外的硬盘",
			"3、不支持PCI和USB设备直通",
		},
		BanInit: []string{"gpu_num", "gpu_mem"},
		BanEdit: []string{"gpu_num", "gpu_mem", "flu_num", "hdd_num", "speed_u", "speed_d"},
		TabLock: []string{"hdd", "iso", "pci", "usb"},
	},
	{
		Type:        "vSphereESXi",
		Description: "vSphereESXi Server",
		Enabled:     true,
		IsRemote:    true,
		Platform:    []string{"Linux", "Windows", "MacOS"},
		CPUArch:     []string{"x86_64", "aarch64"},
		Optional:    map[string]string{},
		Messages:    []string{},
		BanInit:     []string{},
		BanEdit:     []string{},
		TabLock:     []string{},
	},
	{
		Type:        "HyperVSetup",
		Description: "Windows HyperV x64",
		Enabled:     true,
		IsRemote:    true,
		Platform:    []string{"Windows"},
		CPUArch:     []string{"x86_64"},
		Optional:    map[string]string{},
		Messages: []string{
			"1、无法单独限制上下行带宽，取二者最低值分配",
			"2、不支持USB直通，支持GPU PV和DDA设备直通",
		},
		BanInit: []string{},
		BanEdit: []string{},
		TabLock: []string{"usb"},
	},
	{
		Type:        "PromoxSetup",
		Description: "ProxmoxVE Platform",
		Enabled:     true,
		IsRemote:    true,
		Platform:    []string{"Linux", "Windows"},
		CPUArch:     []string{"x86_64", "aarch64"},
		Optional:    map[string]string{},
		Messages:    []string{},
		BanInit:     []string{},
		BanEdit:     []string{},
		TabLock:     []string{},
	},
	{
		Type:        "VirtualBoxs",
		Description: "Oracle VirtualBox",
		Enabled:     true,
		IsRemote:    false,
		Platform:    []string{"Linux", "Windows", "MacOS"},
		CPUArch:     []string{"x86_64", "aarch64"},
		Optional:    map[string]string{},
		Messages: []string{
			"1、通过VBoxManage命令行管理虚拟机",
			"2、支持USB设备直通（需要VirtualBox扩展包）",
			"3、远程桌面通过VRDE（RDP协议）实现",
		},
		BanInit: []string{"gpu_num"},
		BanEdit: []string{"gpu_num"},
		TabLock: []string{"pci"},
	},
	{
		Type:        "QEMUServer",
		Description: "QEMU/KVM Platform",
		Enabled:     true,
		IsRemote:    true,
		Platform:    []string{"Linux"},
		CPUArch:     []string{"x86_64", "aarch64"},
		Optional:    map[string]string{},
		Messages: []string{
			"1、通过virsh/libvirt管理QEMU/KVM虚拟机",
			"2、支持PCI/USB设备直通",
			"3、VNC远程桌面（自动分配端口）",
			"4、修改密码需要虚拟机安装qemu-guest-agent",
		},
		BanInit: []string{},
		BanEdit: []string{},
		TabLock: []string{},
	},
	{
		Type:        "SmolVM",
		Description: "SmolVM MicroVM Runtime",
		Enabled:     true,
		IsRemote:    true,
		Platform:    []string{"Linux"},
		CPUArch:     []string{"x86_64", "aarch64"},
		Optional: map[string]string{
			"smolvm_kernel":  "自定义内核 vmlinux 路径（留空则使用 images_path/vmlinux-smolvm）",
			"smolvm_hv":      "hypervisor 名称(firecracker/cloud-hypervisor/qemu-system-x86_64)",
			"smolvm_hv_path": "hypervisor 可执行文件路径（留空自动探测）",
		},
		Messages: []string{
			"1、依赖 KVM（/dev/kvm）与 Firecracker/Cloud-Hypervisor",
			"2、不支持 GPU/PCI/USB 直通，不支持 ISO 挂载",
			"3、使用 Docker 镜像作为 rootfs（docker export → ext4）",
			"4、SSH 端口转发通过 socat / iptables 实现",
		},
		BanInit: []string{"gpu_num", "gpu_mem"},
		BanEdit: []string{"gpu_num", "gpu_mem", "flu_num", "speed_u", "speed_d"},
		TabLock: []string{"hdd", "iso", "pci", "usb", "efi", "gpu"},
	},
	{
		Type:        "MemuAndroid",
		Description: "MEmu Game Emulator",
		Enabled:     true,
		IsRemote:    false,
		Platform:    []string{"Windows"},
		CPUArch:     []string{"x86_64"},
		Optional: map[string]string{
			"graphics_render_mode": "图形渲染模式(1:DirectX, 0:OpenGL)",
			"enable_su":            "是否以超级用户权限启动(1:是, 0:否)",
			"enable_audio":         "是否启用音频(1:是, 0:否)",
			"fps":                  "帧率(如30, 60)",
		},
		Messages: []string{
			"1、通过memuc命令行管理逍遥安卓模拟器",
			"2、不支持额外硬盘挂载，ISO挂载替换为APK安装",
			"3、不支持暂停/恢复操作",
			"4、VNC替换为ADB连接信息",
		},
		BanInit: []string{"gpu_num", "gpu_mem", "hdd_num", "speed_u", "speed_d"},
		BanEdit: []string{"gpu_num", "gpu_mem", "hdd_num", "speed_u", "speed_d"},
		TabLock: []string{"hdd", "pci", "usb"},
	},
	{
		Type:        "QingzhouYun",
		Description: "QingzhouYun Cloud",
		Enabled:     false, // 与 Python 一致：不对外暴露（驱动代码保留）
		IsRemote:    true,
		Platform:    []string{"Linux", "Windows", "MacOS"},
		CPUArch:     []string{"x86_64", "aarch64"},
		Optional:    map[string]string{},
		Messages: []string{
			"1、云平台不支持PCI/USB设备直通",
			"2、通过HTTP API远程管理虚拟机",
		},
		BanInit: []string{"gpu_num", "gpu_mem"},
		BanEdit: []string{"gpu_num", "gpu_mem"},
		TabLock: []string{"pci", "usb"},
	},
}

// CurrentPlatform 返回当前系统平台（与 Python platform_map 一致）
func CurrentPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "linux":
		return "Linux"
	case "darwin":
		return "MacOS"
	default:
		return runtime.GOOS
	}
}

// CurrentCPUArch 返回当前 CPU 架构（与 Python arch_map 一致）
func CurrentCPUArch() string {
	switch runtime.GOARCH {
	case "amd64", "386":
		return "x86_64"
	case "arm64":
		return "aarch64"
	default:
		return runtime.GOARCH
	}
}

// containsString 判断字符串切片是否包含指定值
func containsString(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// EngineTypes 返回当前平台可用的引擎类型
// 过滤规则与 Python get_engine_types 一致：
//   - isEnable 为 false 的跳过
//   - isRemote 为 false 时，要求当前平台与架构均在支持范围内
//
// 返回值为「引擎类型 -> 元信息对象」，前端以 Object.entries 遍历并使用 enabled/description/messages/options
func EngineTypes() map[string]EngineMeta {
	platform := CurrentPlatform()
	arch := CurrentCPUArch()

	result := make(map[string]EngineMeta, len(engineConfigs))
	for _, meta := range engineConfigs {
		if !meta.Enabled {
			continue
		}
		if !meta.IsRemote {
			if !containsString(meta.Platform, platform) || !containsString(meta.CPUArch, arch) {
				continue
			}
		}
		if meta.Optional == nil {
			meta.Optional = map[string]string{}
		}
		if meta.Messages == nil {
			meta.Messages = []string{}
		}
		if meta.BanInit == nil {
			meta.BanInit = []string{}
		}
		if meta.BanEdit == nil {
			meta.BanEdit = []string{}
		}
		if meta.TabLock == nil {
			meta.TabLock = []string{}
		}
		result[meta.Type] = meta
	}
	return result
}

// EngineMetaOf 返回指定引擎类型的元信息（不存在时返回空值对象）
func EngineMetaOf(engineType string) (EngineMeta, bool) {
	for _, meta := range engineConfigs {
		if meta.Type == engineType {
			return meta, true
		}
	}
	return EngineMeta{
		Type:     engineType,
		Optional: map[string]string{},
		Messages: []string{},
		BanInit:  []string{},
		BanEdit:  []string{},
		TabLock:  []string{},
	}, false
}
