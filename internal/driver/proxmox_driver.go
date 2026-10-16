// Package driver Proxmox VE 驱动
// 使用 github.com/luthermonson/go-proxmox 库
package driver

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	proxmox "github.com/luthermonson/go-proxmox"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// TLSVerifyEnabled 返回是否校验下游平台证书。
//
// 默认保持与 Python 版一致的「不校验」，因为 PVE / ESXi 普遍使用
// 自签名证书，强制校验会导致既有部署无法连接。
// 可通过环境变量 OPENIDCS_TLS_VERIFY=1 启用校验以获得完整的主机身份保证。
func TLSVerifyEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("OPENIDCS_TLS_VERIFY")))
	return v == "1" || v == "true" || v == "yes"
}

// platformHTTPClient 返回访问虚拟化平台 API 的 HTTP 客户端。
//
// 必须设置整体超时：原实现没有 Timeout，下游不响应时会让调用方
// 永久阻塞（持锁时还会拖住整台主机的所有操作）。
func platformHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: !TLSVerifyEnabled()},
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
}

// ProxmoxDriver Proxmox VE 驱动
type ProxmoxDriver struct {
	*BaseDriver
	client   *proxmox.Client
	clientMu sync.Mutex
}

// proxmoxAPIPath go-proxmox 客户端要求的 API 路径前缀。
//
// go-proxmox 的 NewClient(baseURL) 不会自动补 /api2/json：其 Req 对以
// "/" 开头的路径直接做 baseURL+path 拼接，因此 baseURL 必须自带
// /api2/json。若只传 "https://host:8006"，会去请求 https://host:8006/version，
// 被 Proxmox 的 pveproxy 以 `500 no such file '/version'` 拒绝
// （表现为「连接Proxmox失败: 500 no such file '/version'」）。
const proxmoxAPIPath = "/api2/json"

// proxmoxBaseURL 归一化出 go-proxmox 的 API 基地址 https://host:port/api2/json。
//
// 兼容以下常见填写方式：
//   - 192.168.1.50                        → https://192.168.1.50:8006/api2/json
//   - 192.168.1.50:8006
//   - https://192.168.1.50:8006/
//   - https://pve.example.com/pve/api2/json（反向代理前缀会被保留）
func (d *ProxmoxDriver) proxmoxBaseURL() string {
	raw := strings.TrimSpace(d.Config.ServerAddr)
	raw = strings.TrimPrefix(raw, "https://")
	raw = strings.TrimPrefix(raw, "http://")
	raw = strings.TrimRight(raw, "/")

	host, path := raw, ""
	if i := strings.Index(raw, "/"); i >= 0 {
		host, path = raw[:i], strings.TrimRight(raw[i:], "/")
	}
	if host == "" {
		host = "127.0.0.1"
	}
	// 未显式指定端口时：直连场景补默认 8006；
	// 若填写了路径（反向代理前缀），则沿用 https 默认端口
	if !hostHasPort(host) && path == "" {
		host += ":8006"
	}
	switch {
	case path == "" || path == "/":
		path = proxmoxAPIPath
	case !strings.HasSuffix(path, proxmoxAPIPath):
		path += proxmoxAPIPath
	}
	return "https://" + host + path
}

// proxmoxHostPort 返回 host:port（不含协议与 API 路径），供 VNC 等直连场景使用。
func (d *ProxmoxDriver) proxmoxHostPort() string {
	host := strings.TrimPrefix(d.proxmoxBaseURL(), "https://")
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	return host
}

// hostHasPort 判断 host[:port] 是否已带端口（兼容 IPv6 字面量 [::1]:8006）
func hostHasPort(host string) bool {
	if strings.HasPrefix(host, "[") {
		_, _, err := net.SplitHostPort(host)
		return err == nil
	}
	return strings.Contains(host, ":")
}

// NewProxmoxDriver 创建Proxmox驱动
func NewProxmoxDriver(cfg *model.HSConfig) *ProxmoxDriver {
	return &ProxmoxDriver{BaseDriver: NewBaseDriver(cfg)}
}

// ============================================================
// 连接管理
// ============================================================

// apiConn 获取Proxmox API连接（懒加载+复用）
//
// 原实现每次调用都在持锁状态下做一次网络探测，且使用
// context.Background()：PVE 不响应时会占住连接锁并无限阻塞，
// 进而拖住该主机后续所有操作、整轮状态采集以及进程退出。
// 现在改为：锁内只读取指针，慢路径在锁外建立连接并带超时。
func (d *ProxmoxDriver) apiConn() (*proxmox.Client, error) {
	d.clientMu.Lock()
	existing := d.client
	d.clientMu.Unlock()
	if existing != nil {
		return existing, nil
	}

	user := d.Config.ServerUser
	if user == "" {
		user = "root"
	}
	if !strings.Contains(user, "@") {
		user = user + "@pam"
	}

	client := proxmox.NewClient(
		d.proxmoxBaseURL(),
		proxmox.WithCredentials(&proxmox.Credentials{
			Username: user,
			Password: d.Config.ServerPass,
		}),
		proxmox.WithHTTPClient(platformHTTPClient()),
	)

	// 建立连接带超时，避免依赖不可用时阻塞调用方
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.Version(ctx); err != nil {
		return nil, fmt.Errorf("连接Proxmox失败: %w", err)
	}

	d.clientMu.Lock()
	// 并发首次访问时以先发布者为准，避免重复覆盖
	if d.client == nil {
		d.client = client
	}
	result := d.client
	d.clientMu.Unlock()
	return result, nil
}

// resetConn 在调用失败时丢弃连接，令下次访问重新建立。
func (d *ProxmoxDriver) resetConn() {
	d.clientMu.Lock()
	d.client = nil
	d.clientMu.Unlock()
}

// nodeName 获取PVE节点名
func (d *ProxmoxDriver) nodeName() string {
	if d.Config.LaunchPath != "" {
		return d.Config.LaunchPath
	}
	return "pve"
}

// getNode 获取节点对象
func (d *ProxmoxDriver) getNode() (*proxmox.Node, error) {
	client, err := d.apiConn()
	if err != nil {
		return nil, err
	}
	node, err := client.Node(context.Background(), d.nodeName())
	if err != nil {
		return nil, fmt.Errorf("获取节点失败: %w", err)
	}
	return node, nil
}

// ProxmoxSameVMName 判断两个虚拟机名是否为「同一台」。
//
// Proxmox 平台上的名称使用连字符，而早期版本的 Go 驱动在扫描时把 "-"
// 替换成 "_" 作为 vm_uuid，导致同一台虚拟机在库里出现 pve-xxx 与
// pve_xxx 两种键。这里把 "_" 与 "-" 视为等价来判重。
func ProxmoxSameVMName(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	return strings.ReplaceAll(a, "_", "-") == strings.ReplaceAll(b, "_", "-")
}

// findExistingVMKeyLocked 在锁内按命名变体查找已存在的 vm_uuid（未找到返回空串）。
func (d *ProxmoxDriver) findExistingVMKeyLocked(name string) string {
	if name == "" {
		return ""
	}
	if _, ok := d.VmSaving[name]; ok {
		return name
	}
	for key := range d.VmSaving {
		if ProxmoxSameVMName(key, name) {
			return key
		}
	}
	return ""
}

// getVMID 根据vm_uuid查找VMID（先查缓存，再查API）
func (d *ProxmoxDriver) getVMID(vmUUID string) (int64, error) {
	d.mu.RLock()
	cfg, ok := d.VmSaving[vmUUID]
	if !ok {
		// 兼容历史数据的命名变体（"_" 与 "-" 差异）
		if variant := d.findExistingVMKeyLocked(vmUUID); variant != "" {
			cfg, ok = d.VmSaving[variant]
		}
	}
	d.mu.RUnlock()

	if ok {
		if vmData, ok2 := cfg.ExtData["vmid"]; ok2 {
			if id, ok3 := vmData.(float64); ok3 && id > 0 {
				return int64(id), nil
			}
		}
	}

	// 从API查找
	node, err := d.getNode()
	if err != nil {
		return 0, err
	}

	vms, err := node.VirtualMachines(context.Background())
	if err != nil {
		return 0, fmt.Errorf("获取VM列表失败: %w", err)
	}

	// 平台上的名称优先与 vm_uuid 一致，其次兼容 "_"/"-" 互换的历史形式
	names := []string{vmUUID}
	for _, variant := range []string{
		strings.ReplaceAll(vmUUID, "_", "-"),
		strings.ReplaceAll(vmUUID, "-", "_"),
	} {
		if variant != vmUUID {
			names = append(names, variant)
		}
	}
	for _, vm := range vms {
		matched := false
		for _, n := range names {
			if vm.Name == n {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		// 缓存VMID
		d.mu.Lock()
		if cfg, ok := d.VmSaving[vmUUID]; ok {
			if cfg.ExtData == nil {
				cfg.ExtData = make(map[string]interface{})
			}
			cfg.ExtData["vmid"] = float64(vm.VMID)
		}
		d.mu.Unlock()
		return int64(vm.VMID), nil
	}
	return 0, fmt.Errorf("未找到虚拟机 %s 的VMID", vmUUID)
}

// getVM 获取VM对象
func (d *ProxmoxDriver) getVM(vmUUID string) (*proxmox.VirtualMachine, error) {
	node, err := d.getNode()
	if err != nil {
		return nil, err
	}
	vmid, err := d.getVMID(vmUUID)
	if err != nil {
		return nil, err
	}
	vm, err := node.VirtualMachine(context.Background(), int(vmid))
	if err != nil {
		return nil, fmt.Errorf("获取VM对象失败: %w", err)
	}
	return vm, nil
}

// newVMID 分配新的VMID
func (d *ProxmoxDriver) newVMID() (int, error) {
	node, err := d.getNode()
	if err != nil {
		return 100, nil
	}
	vms, err := node.VirtualMachines(context.Background())
	if err != nil {
		return 100, nil
	}
	used := make(map[int]bool)
	for _, vm := range vms {
		used[int(vm.VMID)] = true
	}
	for id := 100; id < 9999; id++ {
		if !used[id] {
			return id, nil
		}
	}
	return 100, nil
}

// ============================================================
// 电源状态
// ============================================================

// GetPower 获取虚拟机电源状态
func (d *ProxmoxDriver) GetPower(vmUUID string) string {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return "未知"
	}
	switch vm.Status {
	case "running":
		return "运行中"
	case "stopped":
		return "已关机"
	case "paused":
		return "已暂停"
	default:
		return "未知"
	}
}

// GetStatus 获取主机状态
func (d *ProxmoxDriver) GetStatus() (*model.HWStatus, error) {
	node, err := d.getNode()
	if err != nil {
		// 远程平台采集失败返回空状态，不能用本机资源替代
		return emptyHWStatus(), nil
	}

	if err := node.Status(context.Background()); err != nil {
		return emptyHWStatus(), nil
	}

	hw := &model.HWStatus{
		OnUpdate: time.Now().Unix(),
	}

	if node.CPU != 0 {
		hw.CpuUsage = int(node.CPU * 100)
	}
	if node.Memory.Total > 0 {
		hw.MemTotal = int(node.Memory.Total / 1024 / 1024)
		hw.MemUsage = int(node.Memory.Used / 1024 / 1024)
	}
	if node.RootFS.Total > 0 {
		hw.HddTotal = int(node.RootFS.Total / 1024 / 1024)
		hw.HddUsage = int(node.RootFS.Used / 1024 / 1024)
	}
	hw.CpuTotal = int(node.CPUInfo.CPUs)
	hw.CpuModel = node.CPUInfo.Model

	return hw, nil
}

// ============================================================
// 虚拟机生命周期
// ============================================================

// ScanVMs 扫描并同步虚拟机列表
//
// 与 Python 版 ProxmoxQemu.VMDetect 对齐：使用主机配置的 filter_name
// 作为名称前缀过滤，只纳管带该前缀的虚拟机，
// 避免把平台上他人或模板虚拟机一并纳入本主机管理。
func (d *ProxmoxDriver) ScanVMs() (*model.ZMessage, error) {
	node, err := d.getNode()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMDetect", Message: err.Error()}, nil
	}

	vms, err := node.VirtualMachines(context.Background())
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMDetect", Message: err.Error()}, nil
	}

	prefix := d.Config.FilterName
	scanned := 0
	added := 0

	d.mu.Lock()
	defer d.mu.Unlock()

	for _, vm := range vms {
		// 前缀过滤（未配置 filter_name 时不过滤）
		if prefix != "" && !strings.HasPrefix(vm.Name, prefix) {
			continue
		}
		scanned++

		// vm_uuid 统一使用 Proxmox 平台上的真实名称（与 Python 版一致），
		// 并对历史遗留的「下划线变体」键做复用，避免同一台虚拟机
		// 因命名形式不同产生第二条记录。
		vmUUID := d.findExistingVMKeyLocked(vm.Name)
		if vmUUID == "" {
			vmUUID = vm.Name
			cfg := model.NewVMConfig()
			cfg.VmUUID = vmUUID
			if cfg.ExtData == nil {
				cfg.ExtData = make(map[string]interface{})
			}
			cfg.ExtData["vmid"] = float64(vm.VMID)
			d.VmSaving[vmUUID] = cfg
			added++
		} else {
			if d.VmSaving[vmUUID].ExtData == nil {
				d.VmSaving[vmUUID].ExtData = make(map[string]interface{})
			}
			d.VmSaving[vmUUID].ExtData["vmid"] = float64(vm.VMID)
		}
	}

	return &model.ZMessage{
		Success: true, Action: "VMDetect",
		Message: fmt.Sprintf("扫描完成：匹配 %d 台，新增 %d 台", scanned, added),
	}, nil
}

// CreateVM 创建虚拟机
func (d *ProxmoxDriver) CreateVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	if cfg == nil {
		return &model.ZMessage{Success: false, Action: "VMCreate", Message: "虚拟机配置为空"}, nil
	}

	node, err := d.getNode()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate", Message: err.Error()}, nil
	}

	vmid, err := d.newVMID()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate", Message: err.Error()}, nil
	}

	ctx := context.Background()

	// 平台上的虚拟机名与内部 vm_uuid 保持一致（不再做 "_"→"-" 转换），
	// 否则创建后扫描会因键形式不同而多出一条重复记录。
	vmName := cfg.VmUUID

	memMB := cfg.MemNum
	if memMB <= 0 {
		memMB = 512
	}
	cpuNum := cfg.CpuNum
	if cpuNum <= 0 {
		cpuNum = 1
	}

	// 系统盘所在的存储（PVE 存储名，如 local / local-lvm）
	storage := "local-lvm"
	if strings.TrimSpace(d.Config.SystemPath) != "" {
		storage = d.Config.SystemPath
	}

	// 缓存 VMID，后续安装系统、开机都需要
	if cfg.ExtData == nil {
		cfg.ExtData = make(map[string]interface{})
	}
	cfg.ExtData["vmid"] = float64(vmid)

	// 填充默认启动项顺序（系统盘 -> 数据盘 -> 光盘）
	if len(cfg.EfiAll) == 0 {
		cfg.EfiAll = buildEfiList(cfg)
	}

	// 构建创建参数
	//
	// 关键项说明（对应 Python ProxmoxQemu.VMCreate）：
	//   bios=ovmf + efidisk0 ：UEFI 启动，Windows/现代 Linux 镜像必需
	//   cpu=host             ：透传宿主 CPU 特性，兼容性与性能最佳
	//   scsihw=virtio-scsi-single
	//   netN                 ：必须带 macaddr，否则路由器 DHCP 静态绑定对不上
	opts := []proxmox.VirtualMachineOption{
		{Name: "name", Value: vmName},
		{Name: "memory", Value: strconv.Itoa(memMB)},
		{Name: "cores", Value: strconv.Itoa(cpuNum)},
		{Name: "sockets", Value: "1"},
		{Name: "cpu", Value: "host"},
		{Name: "ostype", Value: pveOsType(cfg.OsName)},
		{Name: "bios", Value: "ovmf"},
		{Name: "scsihw", Value: "virtio-scsi-single"},
		{Name: "efidisk0", Value: fmt.Sprintf("%s:1,efitype=4m", storage)},
	}

	// 网卡配置（含 MAC）
	nets := d.netConf(cfg)
	for _, key := range sortedMapKeys(nets) {
		opts = append(opts, proxmox.VirtualMachineOption{Name: key, Value: nets[key]})
	}

	task, err := node.NewVirtualMachine(ctx, vmid, opts...)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate", Message: "创建虚拟机失败: " + err.Error()}, nil
	}
	if task != nil {
		if err := task.WaitFor(ctx, 120); err != nil {
			return &model.ZMessage{Success: false, Action: "VMCreate", Message: "等待创建任务超时: " + err.Error()}, nil
		}
	}

	// 重新获取 VM 对象（创建任务完成后才有完整配置）
	vm, err := node.VirtualMachine(ctx, vmid)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate", Message: "获取虚拟机对象失败: " + err.Error()}, nil
	}

	// PCI/GPU 直通（hostpciN）
	if len(cfg.PciAll) > 0 {
		gpuOpts := make([]proxmox.VirtualMachineOption, 0, len(cfg.PciAll))
		idx := 0
		for _, key := range sortedMapKeys(cfg.PciAll) {
			pci := cfg.PciAll[key]
			if strings.TrimSpace(pci.GpuUUID) == "" {
				continue
			}
			gpuOpts = append(gpuOpts, proxmox.VirtualMachineOption{
				Name:  fmt.Sprintf("hostpci%d", idx),
				Value: fmt.Sprintf("%s,pcie=1", pci.GpuUUID),
			})
			idx++
		}
		if len(gpuOpts) > 0 {
			if _, err := vm.Config(ctx, gpuOpts...); err != nil {
				// 直通失败不阻断创建，但必须暴露出来
				return &model.ZMessage{
					Success: false, Action: "VMCreate",
					Message: "虚拟机已创建，但 PCI 直通配置失败: " + err.Error(),
				}, nil
			}
		}
	}

	// 安装系统：把模板镜像导入/复制为系统盘并挂载
	// 缺少这一步虚拟机没有可启动的系统盘
	installResult, err := d.installSystem(cfg)
	if err != nil {
		return &model.ZMessage{
			Success: false, Action: "VMCreate",
			Message: "系统镜像安装失败: " + err.Error(),
		}, nil
	}
	if installResult != nil && !installResult.Success {
		return &model.ZMessage{
			Success: false, Action: "VMCreate",
			Message: "虚拟机已创建，但系统安装失败: " + installResult.Message,
		}, nil
	}

	// 配置 VNC 直通端口（创建时立即写入，无需重启）
	d.applyVNCPort(cfg, vm, vmid)

	// 启动虚拟机
	// 注意：此处 vm_saving 尚未写入，不能走 PowerControl（其内部会查表失败）
	if _, err := vm.Start(ctx); err != nil {
		return &model.ZMessage{
			Success: false, Action: "VMCreate",
			Message: "虚拟机创建成功但启动失败: " + err.Error(),
		}, nil
	}

	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMCreate", Message: "虚拟机创建成功"}, nil
}

// applyVNCPort 为虚拟机写入 VNC 直通参数（args=-vnc 0.0.0.0:<id>）
//
// PVE 默认的 VNC 由 pveproxy 代理且需要租约，直接指定 -vnc 参数
// 可以让 VNC 端口固定可预测，便于前端直连。
func (d *ProxmoxDriver) applyVNCPort(cfg *model.VMConfig, vm *proxmox.VirtualMachine, vmid int) {
	if cfg == nil || vm == nil {
		return
	}
	ctx := context.Background()

	vncPort := 5900 + vmid
	if p, err := strconv.Atoi(strings.TrimSpace(cfg.VcPort)); err == nil && p > 0 {
		vncPort = p
	}
	vncID := vncPort - 5900
	vncArg := fmt.Sprintf("-vnc 0.0.0.0:%d", vncID)

	currentArgs := ""
	if vm.VirtualMachineConfig != nil {
		currentArgs = strings.TrimSpace(vm.VirtualMachineConfig.Args)
	}

	newArgs := strings.TrimSpace(vncRe.ReplaceAllString(currentArgs, ""))
	if newArgs == "" {
		newArgs = vncArg
	} else {
		newArgs = newArgs + " " + vncArg
	}

	if _, err := vm.Config(ctx, proxmox.VirtualMachineOption{Name: "args", Value: newArgs}); err != nil {
		// VNC 写入失败不阻断创建流程
		return
	}
	cfg.VcPort = strconv.Itoa(vncPort)
}

var vncRe = regexp.MustCompile(`-vnc\s+\S+`)

// pveOsType 根据镜像名推断 PVE 的 ostype（对应 Python 的判断逻辑）
func pveOsType(osName string) string {
	lower := strings.ToLower(osName)
	if strings.Contains(lower, "win") {
		if strings.Contains(lower, "win11") || strings.Contains(lower, "win-11") {
			return "win11"
		}
		return "win10"
	}
	return "l26"
}

// sortedMapKeys 返回 map 的有序键（保证 netX / hostpciX 编号稳定）
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ReinstallVM 重装虚拟机系统
//
// 对应 Python ProxmoxQemu.VMSetups 的重装语义：
// 停止虚拟机 → 删除旧系统盘 → 重新导入/复制模板镜像 → 启动。
func (d *ProxmoxDriver) ReinstallVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	if cfg == nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall", Message: "虚拟机配置为空"}, nil
	}
	if strings.TrimSpace(cfg.OsName) == "" {
		return &model.ZMessage{Success: false, Action: "VMReinstall", Message: "未指定系统镜像"}, nil
	}

	vm, err := d.getVM(cfg.VmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall", Message: err.Error()}, nil
	}
	ctx := context.Background()

	// 1. 停止虚拟机
	if vm.IsRunning() {
		if _, err := vm.Stop(ctx); err != nil {
			return &model.ZMessage{
				Success: false, Action: "VMReinstall",
				Message: "停止虚拟机失败: " + err.Error(),
			}, nil
		}
		time.Sleep(3 * time.Second)
	}

	// 2. 卸载并删除旧系统盘（ide0）；旧盘不存在时忽略错误
	if _, err := vm.Config(ctx,
		proxmox.VirtualMachineOption{Name: "delete", Value: "ide0"}); err != nil {
		// 删除失败通常意味着本来就没有 ide0，继续重装即可
		_, _ = vm.Config(ctx, proxmox.VirtualMachineOption{Name: "ide0", Value: "none"})
	}

	// 3. 重新安装系统（导入/复制镜像并挂载）
	if len(cfg.EfiAll) == 0 {
		cfg.EfiAll = buildEfiList(cfg)
	}
	installResult, err := d.installSystem(cfg)
	if err != nil {
		return &model.ZMessage{
			Success: false, Action: "VMReinstall",
			Message: "系统重装失败: " + err.Error(),
		}, nil
	}
	if installResult != nil && !installResult.Success {
		return &model.ZMessage{
			Success: false, Action: "VMReinstall",
			Message: "系统重装失败: " + installResult.Message,
		}, nil
	}

	// 4. 重新启动
	if _, err := vm.Start(ctx); err != nil {
		return &model.ZMessage{
			Success: false, Action: "VMReinstall",
			Message: "系统已重装但启动失败: " + err.Error(),
		}, nil
	}

	return &model.ZMessage{Success: true, Action: "VMReinstall", Message: "系统重装成功"}, nil
}

// UpdateVM 更新虚拟机配置
func (d *ProxmoxDriver) UpdateVM(cfg *model.VMConfig, oldCfg *model.VMConfig) (*model.ZMessage, error) {
	vm, err := d.getVM(cfg.VmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMUpdate", Message: err.Error()}, nil
	}

	var opts []proxmox.VirtualMachineOption

	if cfg.CpuNum > 0 && (oldCfg == nil || cfg.CpuNum != oldCfg.CpuNum) {
		opts = append(opts, proxmox.VirtualMachineOption{Name: "cores", Value: strconv.Itoa(cfg.CpuNum)})
	}
	if cfg.MemNum > 0 && (oldCfg == nil || cfg.MemNum != oldCfg.MemNum) {
		opts = append(opts, proxmox.VirtualMachineOption{Name: "memory", Value: strconv.Itoa(cfg.MemNum)})
	}

	// 网卡同步：MAC/IP 变更后必须重新下发，否则平台侧网卡与
	// 路由器上的 DHCP 静态绑定会不一致，虚拟机拿不到预期地址。
	newNets := d.netConf(cfg)
	oldNets := map[string]string{}
	if oldCfg != nil {
		oldNets = d.netConf(oldCfg)
	}
	if !stringMapEqual(newNets, oldNets) {
		for _, key := range sortedMapKeys(newNets) {
			opts = append(opts, proxmox.VirtualMachineOption{Name: key, Value: newNets[key]})
		}
		// 网卡数量减少时删除多余的旧网卡
		for _, key := range sortedMapKeys(oldNets) {
			if _, ok := newNets[key]; !ok {
				opts = append(opts, proxmox.VirtualMachineOption{Name: "delete", Value: key})
			}
		}
	}

	if len(opts) > 0 {
		if _, err := vm.Config(context.Background(), opts...); err != nil {
			return &model.ZMessage{Success: false, Action: "VMUpdate", Message: err.Error()}, nil
		}
	}

	// 磁盘扩容（仅在容量变大时执行，PVE 不支持缩容）
	if oldCfg != nil && cfg.HddNum > oldCfg.HddNum && cfg.HddNum > 0 {
		sizeGB := cfg.HddNum / 1024
		if cfg.HddNum%1024 != 0 {
			sizeGB++
		}
		if sizeGB < 1 {
			sizeGB = 1
		}
		if _, err := vm.ResizeDisk(context.Background(), "ide0", fmt.Sprintf("%dG", sizeGB)); err != nil {
			return &model.ZMessage{
				Success: false, Action: "VMUpdate",
				Message: "系统盘扩容失败: " + err.Error(),
			}, nil
		}
	}

	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMUpdate", Message: "虚拟机配置已更新"}, nil
}

// DeleteVM 删除虚拟机
func (d *ProxmoxDriver) DeleteVM(vmUUID string, removeBackup bool) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMDelete", Message: err.Error()}, nil
	}

	// 先关机
	if vm.Status == "running" {
		task, err := vm.Stop(context.Background())
		if err == nil {
			_ = task.WaitFor(context.Background(), 30)
		}
	}

	task, err := vm.Delete(context.Background())
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMDelete", Message: err.Error()}, nil
	}

	if err := task.WaitFor(context.Background(), 60); err != nil {
		return &model.ZMessage{Success: false, Action: "VMDelete", Message: "等待删除任务超时: " + err.Error()}, nil
	}

	d.mu.Lock()
	delete(d.VmSaving, vmUUID)
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDelete", Message: "虚拟机已删除"}, nil
}

// PowerControl 电源控制
func (d *ProxmoxDriver) PowerControl(vmUUID string, action model.VMPowers) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMPowers", Message: err.Error()}, nil
	}

	var task *proxmox.Task
	var taskErr error

	switch action {
	case model.VMPowerSStart:
		task, taskErr = vm.Start(context.Background())
	case model.VMPowerSClose:
		task, taskErr = vm.Shutdown(context.Background())
	case model.VMPowerHClose:
		task, taskErr = vm.Stop(context.Background())
	case model.VMPowerSReset, model.VMPowerHReset:
		task, taskErr = vm.Reset(context.Background())
	case model.VMPowerAPause:
		task, taskErr = vm.Pause(context.Background())
	case model.VMPowerAWaked:
		task, taskErr = vm.Resume(context.Background())
	default:
		return &model.ZMessage{Success: false, Action: "VMPowers", Message: "不支持的电源操作"}, nil
	}

	if taskErr != nil {
		return &model.ZMessage{Success: false, Action: "VMPowers", Message: taskErr.Error()}, nil
	}

	if task != nil {
		if err := task.WaitFor(context.Background(), 30); err != nil {
			return &model.ZMessage{Success: false, Action: "VMPowers", Message: "等待电源操作超时"}, nil
		}
	}

	// 更新内存状态
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		if newState, ok2 := PowerActionMap[action]; ok2 {
			cfg.VmFlag = newState
		}
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMPowers", Message: "电源操作已执行"}, nil
}

// ============================================================
// VNC / 远程控制
// ============================================================

// GetVNCInfo 获取VNC连接信息
func (d *ProxmoxDriver) GetVNCInfo(vmUUID string, ipAddr string) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VCRemote", Message: err.Error()}, nil
	}

	vnc, err := vm.TermProxy(context.Background())
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VCRemote", Message: err.Error()}, nil
	}

	hostAddr := d.proxmoxHostPort()

	return &model.ZMessage{
		Success: true,
		Action:  "VCRemote",
		Message: "VNC信息获取成功",
		Results: map[string]interface{}{
			"type":     "proxmox",
			"host":     hostAddr,
			"ticket":   vnc.Ticket,
			"port":     vnc.Port,
			"password": vnc.Ticket,
		},
	}, nil
}

// GetScreenshot 获取虚拟机截图
func (d *ProxmoxDriver) GetScreenshot(vmUUID string) (string, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return "", fmt.Errorf("获取虚拟机失败: %w", err)
	}

	// 检查虚拟机是否运行
	if vm.Status != "running" {
		return "", fmt.Errorf("虚拟机未运行，无法截图")
	}

	// 使用 Proxmox 的 vncproxy 和截图 API
	// Proxmox 7.0+ 支持通过 spiceproxy 获取截图
	// 我们使用 HTTP API 直接获取截图
	client, err := d.apiConn()
	if err != nil {
		return "", fmt.Errorf("连接 Proxmox API 失败: %w", err)
	}

	// 构建截图 API 路径
	// GET /api2/json/nodes/{node}/qemu/{vmid}/agent/exec-status
	// 或使用 vncproxy 方式获取截图
	nodeName := d.nodeName()
	vmid, err := d.getVMID(vmUUID)
	if err != nil {
		return "", fmt.Errorf("获取 VM ID 失败: %w", err)
	}

	// 方案1: 尝试使用 QEMU Guest Agent 截图（需要虚拟机安装 qemu-guest-agent）
	screenshotPath := fmt.Sprintf("/nodes/%s/qemu/%d/agent/exec", nodeName, vmid)
	screenshotCmd := map[string]interface{}{
		"command": []string{
			"sh", "-c",
			"DISPLAY=:0 import -window root png:- | base64",
		},
	}

	ctx := context.Background()
	var result map[string]interface{}

	// 如果 Guest Agent 不可用，使用备用方案
	err = client.Post(ctx, screenshotPath, screenshotCmd, &result)
	if err != nil {
		// 方案2: 使用 Proxmox VNC Screenshot API (需要 Proxmox 7.2+)
		// 这个功能可能需要额外配置或不是所有版本都支持
		return "", fmt.Errorf("截图功能暂不可用: Proxmox 虚拟机需要安装 qemu-guest-agent 并配置 X11 显示，或升级到 Proxmox VE 7.2+ 使用内置截图 API")
	}

	// 等待命令执行完成并获取输出
	pid, ok := result["pid"].(float64)
	if !ok {
		return "", fmt.Errorf("执行截图命令失败")
	}

	// 轮询获取执行结果（最多等待10秒）
	for i := 0; i < 20; i++ {
		time.Sleep(500 * time.Millisecond)

		statusPath := fmt.Sprintf("/nodes/%s/qemu/%d/agent/exec-status?pid=%.0f", nodeName, vmid, pid)
		var statusResult map[string]interface{}
		err = client.Get(ctx, statusPath, &statusResult)
		if err != nil {
			continue
		}

		data, ok := statusResult["data"].(map[string]interface{})
		if !ok {
			continue
		}

		exited, ok := data["exited"].(bool)
		if !ok || !exited {
			continue
		}

		// 获取输出
		outData, ok := data["out-data"].(string)
		if ok && outData != "" {
			// out-data 已经是 base64 编码的 PNG
			return "data:image/png;base64," + outData, nil
		}
	}

	return "", fmt.Errorf("截图超时")
}

// ============================================================
// 备份管理
// ============================================================

// CreateBackup 创建备份
func (d *ProxmoxDriver) CreateBackup(vmUUID string, tips string) (*model.ZMessage, error) {
	node, err := d.getNode()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup", Message: err.Error()}, nil
	}

	vmid, err := d.getVMID(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup", Message: err.Error()}, nil
	}

	storage := "local"
	if d.Config.BackupPath != "" {
		storage = d.Config.BackupPath
	}

	task, err := node.Vzdump(context.Background(), &proxmox.VirtualMachineBackupOptions{
		VMID:     uint64(vmid),
		Storage:  storage,
		Compress: proxmox.VirtualMachineBackupCompressZstd,
	})
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup", Message: err.Error()}, nil
	}

	if err := task.WaitFor(context.Background(), 600); err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup", Message: "备份超时: " + err.Error()}, nil
	}

	// 更新备份列表
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		backupName := fmt.Sprintf("vzdump-qemu-%d-%s.vma.zst", vmid, time.Now().Format("2006_01_02-15_04_05"))
		cfg.Backups = append(cfg.Backups, model.VMBackup{
			BackupName: backupName,
			BackupTime: time.Now(),
			BackupTips: tips,
		})
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMBackup", Message: "备份创建成功"}, nil
}

// RestoreBackup 已迁移至 proxmox_ext.go（真实实现：POST /nodes/{node}/qemu 恢复任务）。

// DeleteBackup 删除备份
func (d *ProxmoxDriver) DeleteBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	storage := "local"
	if d.Config.BackupPath != "" {
		storage = d.Config.BackupPath
	}

	node, err := d.getNode()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "RMBackup", Message: err.Error()}, nil
	}
	bkpStorage, err := node.Storage(context.Background(), storage)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "RMBackup", Message: err.Error()}, nil
	}
	volID := fmt.Sprintf("%s:backup/%s", storage, backupName)
	delTask, err := bkpStorage.DeleteContent(context.Background(), volID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "RMBackup", Message: err.Error()}, nil
	}
	if delTask != nil {
		_ = delTask.WaitFor(context.Background(), 60)
	}

	// 从备份列表中移除
	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		newBackups := cfg.Backups[:0]
		for _, b := range cfg.Backups {
			if b.BackupName != backupName {
				newBackups = append(newBackups, b)
			}
		}
		cfg.Backups = newBackups
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "RMBackup", Message: "备份删除成功"}, nil
}

// ScanBackups 扫描备份文件
func (d *ProxmoxDriver) ScanBackups() (*model.ZMessage, error) {
	storage := "local"
	if d.Config.BackupPath != "" {
		storage = d.Config.BackupPath
	}

	node, err := d.getNode()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "LDBackup", Message: err.Error()}, nil
	}
	bkpStorage, err := node.Storage(context.Background(), storage)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "LDBackup", Message: err.Error()}, nil
	}
	contents, err := bkpStorage.GetContent(context.Background())
	if err != nil {
		return &model.ZMessage{Success: false, Action: "LDBackup", Message: err.Error()}, nil
	}

	// 按vmid分组备份（通过volid中包含backup判断）
	backupsByVM := make(map[int64][]*proxmox.StorageContent)
	for _, c := range contents {
		if strings.Contains(c.Volid, "backup") {
			backupsByVM[int64(c.VMID)] = append(backupsByVM[int64(c.VMID)], c)
		}
	}

	// 更新各VM的备份列表
	d.mu.Lock()
	for vmUUID, cfg := range d.VmSaving {
		vmid, _ := d.getVMIDNoLock(vmUUID)
		if vmid == 0 {
			continue
		}
		if bkps, ok := backupsByVM[vmid]; ok {
			cfg.Backups = make([]model.VMBackup, 0, len(bkps))
			for _, b := range bkps {
				cfg.Backups = append(cfg.Backups, model.VMBackup{
					BackupName: b.Volid,
					BackupTime: time.Unix(int64(b.Ctime), 0),
					BackupTips: b.Notes,
				})
			}
		}
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "LDBackup", Message: "备份扫描完成"}, nil
}

// getVMIDNoLock 不加锁获取VMID（内部使用，调用方已持有锁）
func (d *ProxmoxDriver) getVMIDNoLock(vmUUID string) (int64, error) {
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		if vmData, ok2 := cfg.ExtData["vmid"]; ok2 {
			if id, ok3 := vmData.(float64); ok3 && id > 0 {
				return int64(id), nil
			}
		}
	}
	return 0, fmt.Errorf("未找到VMID")
}

// ============================================================
// PCI / GPU 直通
// ============================================================

// GetGPUList / GetPCIList 已迁移至 proxmox_ext.go（SSH lspci -nn 真实枚举）。

// SetupPCI PCI设备直通操作
func (d *ProxmoxDriver) SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "PCISetup", Message: err.Error()}, nil
	}

	if add {
		// 添加PCI设备
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "hostpci0",
			Value: pciKey + ",pcie=1",
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "PCISetup", Message: err.Error()}, nil
		}
		// 更新内存配置
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			vmCfg.PciAll[pciKey] = *cfg
		}
		d.mu.Unlock()
	} else {
		// 移除PCI设备
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "delete",
			Value: "hostpci0",
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "PCISetup", Message: err.Error()}, nil
		}
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			delete(vmCfg.PciAll, pciKey)
		}
		d.mu.Unlock()
	}

	action := "添加"
	if !add {
		action = "移除"
	}
	return &model.ZMessage{Success: true, Action: "PCISetup", Message: "PCI设备" + action + "成功"}, nil
}

// ============================================================
// USB 直通
// ============================================================

// GetUSBList 已迁移至 proxmox_ext.go（SSH lsusb 真实枚举）。

// SetupUSB USB设备直通操作
func (d *ProxmoxDriver) SetupUSB(vmUUID string, info *model.USBInfos, usbKey string, add bool) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "USBSetup", Message: err.Error()}, nil
	}

	if add {
		usbVal := fmt.Sprintf("host=%s:%s", info.VidUUID, info.PidUUID)
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "usb0",
			Value: usbVal,
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "USBSetup", Message: err.Error()}, nil
		}
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			vmCfg.UsbAll[usbKey] = *info
		}
		d.mu.Unlock()
	} else {
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "delete",
			Value: "usb0",
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "USBSetup", Message: err.Error()}, nil
		}
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			delete(vmCfg.UsbAll, usbKey)
		}
		d.mu.Unlock()
	}

	action := "添加"
	if !add {
		action = "移除"
	}
	return &model.ZMessage{Success: true, Action: "USBSetup", Message: "USB设备" + action + "成功"}, nil
}

// ============================================================
// 磁盘管理
// ============================================================

// MountHDD 挂载/卸载数据盘
func (d *ProxmoxDriver) MountHDD(vmUUID string, hdd *model.SDConfig, mount bool) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDMount", Message: err.Error()}, nil
	}
	vmid, err := d.getVMID(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDMount", Message: err.Error()}, nil
	}

	storage := "local-lvm"
	if d.Config.ExternPath != "" {
		storage = d.Config.ExternPath
	}

	if mount {
		// 创建并挂载数据盘：先找一个空闲的 scsi 位（系统盘通常占 scsi0）
		sizeGB := hdd.HddSize / 1024
		if sizeGB <= 0 {
			sizeGB = 10
		}

		slot := hdd.HddScsi
		if slot == "" {
			cfgMap, _ := d.pveVMConfigMap(vmid)
			for i := 1; i <= 30; i++ {
				candidate := fmt.Sprintf("scsi%d", i)
				if _, used := cfgMap[candidate]; !used {
					slot = candidate
					break
				}
			}
			if slot == "" {
				slot = "scsi1"
			}
		}

		diskVal := fmt.Sprintf("%s:%d,format=qcow2", storage, sizeGB)
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  slot,
			Value: diskVal,
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "HDDMount", Message: err.Error()}, nil
		}

		// 回读卷标识，供后续删除/移交精确定位
		hdd.HddFlag = 1
		hdd.HddScsi = slot
		if cfgMap, err := d.pveVMConfigMap(vmid); err == nil {
			if v, ok := cfgMap[slot]; ok {
				hdd.HddPath = pveVolumeOf(v)
			}
		}
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			vmCfg.HddAll[hdd.HddName] = *hdd
		}
		d.mu.Unlock()
	} else {
		// 卸载数据盘：优先使用记录的设备位，避免误删其它磁盘
		slot := hdd.HddScsi
		if slot == "" {
			cfgMap, _ := d.pveVMConfigMap(vmid)
			if key, _ := pveFindDiskKey(cfgMap, hdd); key != "" && !strings.HasPrefix(key, "unused") {
				slot = key
			}
		}
		if slot != "" {
			if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
				Name:  "delete",
				Value: slot,
			}); err != nil {
				return &model.ZMessage{Success: false, Action: "HDDMount", Message: err.Error()}, nil
			}
		}
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			if h, exists := vmCfg.HddAll[hdd.HddName]; exists {
				h.HddFlag = 0
				h.HddScsi = ""
				vmCfg.HddAll[hdd.HddName] = h
			}
		}
		d.mu.Unlock()
	}

	action := "挂载"
	if !mount {
		action = "卸载"
	}
	return &model.ZMessage{Success: true, Action: "HDDMount", Message: "磁盘" + action + "成功"}, nil
}

// MountISO 挂载/卸载ISO
func (d *ProxmoxDriver) MountISO(vmUUID string, iso *model.IMConfig, mount bool) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "ISOMount", Message: err.Error()}, nil
	}

	if mount {
		dvdStorage := "local"
		if d.Config.DvdromPath != "" {
			dvdStorage = d.Config.DvdromPath
		}
		isoVal := fmt.Sprintf("%s:iso/%s,media=cdrom", dvdStorage, iso.IsoFile)
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "ide2",
			Value: isoVal,
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "ISOMount", Message: err.Error()}, nil
		}
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			vmCfg.IsoAll[iso.IsoName] = *iso
		}
		d.mu.Unlock()
	} else {
		if _, err := vm.Config(context.Background(), proxmox.VirtualMachineOption{
			Name:  "ide2",
			Value: "none,media=cdrom",
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "ISOMount", Message: err.Error()}, nil
		}
		d.mu.Lock()
		if vmCfg, ok := d.VmSaving[vmUUID]; ok {
			delete(vmCfg.IsoAll, iso.IsoName)
		}
		d.mu.Unlock()
	}

	action := "挂载"
	if !mount {
		action = "卸载"
	}
	return &model.ZMessage{Success: true, Action: "ISOMount", Message: "ISO" + action + "成功"}, nil
}

// ============================================================
// 密码设置
// ============================================================

// SetPassword 设置虚拟机密码（通过QEMU guest agent）
func (d *ProxmoxDriver) SetPassword(vmUUID string, password string) (*model.ZMessage, error) {
	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "Password", Message: err.Error()}, nil
	}

	// 通过QEMU guest agent设置密码
	if err := vm.AgentSetUserPassword(context.Background(), "root", password); err != nil {
		// 如果guest agent不可用，只更新内存中的密码
		d.mu.Lock()
		if cfg, ok := d.VmSaving[vmUUID]; ok {
			cfg.OsPass = password
		}
		d.mu.Unlock()
		return &model.ZMessage{Success: true, Action: "Password", Message: "密码已记录（guest agent不可用）"}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		cfg.OsPass = password
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "Password", Message: "密码设置成功"}, nil
}

// ============================================================
// 定时任务
// ============================================================

// Crontab 执行定时任务（同步VM状态）
func (d *ProxmoxDriver) Crontab() bool {
	node, err := d.getNode()
	if err != nil {
		return false
	}

	vms, err := node.VirtualMachines(context.Background())
	if err != nil {
		return false
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	for _, vm := range vms {
		vmUUID := strings.ReplaceAll(vm.Name, "-", "_")
		if cfg, ok := d.VmSaving[vmUUID]; ok {
			switch vm.Status {
			case "running":
				cfg.VmFlag = model.VMPowerStarted
			case "stopped":
				cfg.VmFlag = model.VMPowerStopped
			case "paused":
				cfg.VmFlag = model.VMPowerSuspend
			default:
				cfg.VmFlag = model.VMPowerUnknown
			}
		}
	}

	return true
}
