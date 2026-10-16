// Package service NAT端口映射 / 网络静态绑定下发服务（爱快路由器）
// 对应 Python 版 HostModule/NetsManager 的登录与 dnat/dhcp_static/arp 调用协议，
// 同时兼容爱快 3.x 与爱快 4.x（详见 ikuai_client.go）。
package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/driver"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// NATService NAT / DHCP / ARP 下发服务
type NATService struct {
	db       *data.DB
	resolver func(string) driver.HostDriver
	mu       sync.Mutex
	clients  map[string]*ikuaiClient
}

// NewNATService 创建 NAT 服务
func NewNATService(db *data.DB) *NATService {
	return &NATService{
		db:      db,
		clients: make(map[string]*ikuaiClient),
	}
}

// SetDriverResolver 注入驱动解析器（用于识别平台自有的下发通道，如青州云）
func (s *NATService) SetDriverResolver(fn func(string) driver.HostDriver) {
	s.resolver = fn
}

// provider 返回主机驱动实现的平台自有端口映射通道（若支持）。
//
// 目前两类平台自持通道：
//   - 青州云：云端 NAT API
//   - 容器类（Docker / LXD / SmolVM）：宿主机 iptables
//
// 均未实现时回落到爱快路由器下发。
func (s *NATService) provider(hsName string) (driver.NatPortProvider, bool) {
	if s.resolver == nil {
		return nil, false
	}
	d := s.resolver(hsName)
	if d == nil {
		return nil, false
	}
	p, ok := d.(driver.NatPortProvider)
	return p, ok
}

// client 获取（并缓存）指定路由器的客户端。
// 缓存键使用归一化后的地址，避免大小写/末尾斜杠导致同一路由器被重复建连。
func (s *NATService) client(routerAddr, user, pass string) *ikuaiClient {
	if strings.TrimSpace(routerAddr) == "" {
		return nil
	}
	base, _ := normalizeIKuaiAddr(routerAddr)
	key := base + "|" + user + "|" + pass
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.clients[key]; ok {
		return c
	}
	c := newIKuaiClient(routerAddr, user, pass)
	s.clients[key] = c
	return c
}

// ============================================================
// 对外方法（写库 + 下发）
// ============================================================

// ApplyPortMap 添加端口映射
// 未指定外网端口时自动分配；平台自有通道优先（青州云），否则下发爱快路由器。
// nat 为指针，便于回填自动分配的端口。
//
// 端口分配同时避开两类占用：本地数据库中已分配的端口，以及爱快路由器上已存在的
// 端口映射（对齐 Python 版 BasicServer.PortsMap：先 get_port() 收集 wan_list 再分配）。
func (s *NATService) ApplyPortMap(hsName, vmUUID string, nat *model.PortData, userID int) error {
	if nat == nil {
		return fmt.Errorf("端口映射参数为空")
	}
	host, err := s.db.GetHost(hsName)
	if err != nil || host == nil {
		return fmt.Errorf("主机 %s 不存在", hsName)
	}

	provider, hasProvider := s.provider(hsName)

	// 路由器侧已占用的 WAN 端口：仅爱快通道需要（平台通道由云端自行管理端口）
	var ikuai *ikuaiClient
	routerPorts := map[int]bool{}
	if !hasProvider {
		if strings.TrimSpace(host.IKuaiAddr) == "" {
			return fmt.Errorf("主机 %s 未配置爱快路由器地址", hsName)
		}
		ikuai = s.client(host.IKuaiAddr, host.IKuaiUser, host.IKuaiPass)
		if ikuai == nil {
			return fmt.Errorf("爱快路由器配置无效")
		}
		// 读取失败按「路由器上没有占用」处理：与 Python 版 get_port() 失败后
		// wan_list 为空即继续下发一致，本地库占用仍会参与判断，不阻断业务。
		if ports, portsErr := ikuai.listWanPorts(); portsErr == nil {
			routerPorts = ports
		}
		if nat.WanPort != 0 && routerPorts[nat.WanPort] {
			return fmt.Errorf("外网端口 %d 已被路由器上的其它规则占用，请更换端口", nat.WanPort)
		}
	}

	// 平台自有下发通道
	if hasProvider {
		// 端口分配交给驱动：青州云按云端规则分配，容器类（Docker/LXD/SmolVM）
		// 则需查询**宿主机 iptables 实际占用**才能选到真正可用的端口
		// （对齐 Python 版 BasicServer.PortsMap_TTY 的 allocate_port 语义）。
		// 指定了端口时驱动会自行校验宿主机占用。
		msg, err := provider.PortsMap(nat, true)
		if err != nil {
			return err
		}
		if msg != nil && !msg.Success {
			return fmt.Errorf("%s", msg.Message)
		}
		if nat.WanPort == 0 {
			return fmt.Errorf("平台通道未分配外网端口")
		}
		// 驱动分配后仍需防止覆盖其它虚拟机的端口归属
		if owner, found, err := s.db.NATPortOwner(hsName, nat.WanPort); err == nil && found && owner != vmUUID {
			return fmt.Errorf("外网端口 %d 已被虚拟机 %s 占用，请更换端口", nat.WanPort, owner)
		}
		return s.db.UpsertNATRule(hsName, vmUUID, userID, nat.WanPort, nat.LanPort, nat.LanAddr, nat.NatTips)
	}

	if nat.WanPort == 0 {
		port, allocErr := s.allocateWanPort(host, routerPorts)
		if allocErr != nil {
			return allocErr
		}
		nat.WanPort = port
	}

	// 端口占用检查：避免把已属于其它虚拟机的端口重新分配出去。
	// 数据库冲突分支是 UPSERT，若不做前置检查会静默改写归属。
	if owner, found, err := s.db.NATPortOwner(hsName, nat.WanPort); err == nil && found && owner != vmUUID {
		return fmt.Errorf("外网端口 %d 已被虚拟机 %s 占用，请更换端口", nat.WanPort, owner)
	}

	if err := ikuai.addPortMap(*nat); err != nil {
		return err
	}
	return s.db.UpsertNATRule(hsName, vmUUID, userID, nat.WanPort, nat.LanPort, nat.LanAddr, nat.NatTips)
}

// RevokePortMap 删除端口映射：下发删除后删库（下发失败不阻断，避免记录残留）
func (s *NATService) RevokePortMap(hsName, vmUUID string, nat model.PortData) error {
	if p, ok := s.provider(hsName); ok {
		_, _ = p.PortsMap(&nat, false)
		return s.db.DeleteNATRule(hsName, vmUUID, nat.WanPort)
	}

	if host, err := s.db.GetHost(hsName); err == nil && host != nil && host.IKuaiAddr != "" {
		if c := s.client(host.IKuaiAddr, host.IKuaiUser, host.IKuaiPass); c != nil {
			_ = c.deletePortMap(nat.LanPort, nat.LanAddr)
		}
	}
	return s.db.DeleteNATRule(hsName, vmUUID, nat.WanPort)
}

// allocateWanPort 在主机端口范围内分配未占用的外网端口。
//
// reserved 为路由器侧已被占用的端口（无爱快通道时传 nil），
// 与本地数据库中已分配的端口一起参与去重。
//
// 原实现在随机尝试失败后直接返回 low，即使该端口已被占用：
// 端口池接近耗尽时会必然返回冲突端口。现在改为返回明确错误。
//
// 注意：该实现仍不是原子预留，并发请求可能同时选中同一端口；
// 调用方需依赖 NATPortOwner 冲突检查与数据库唯一约束兜底。
func (s *NATService) allocateWanPort(host *data.HSConfigRow, reserved map[int]bool) (int, error) {
	low, high := portRange(host)

	localUsed := make(map[int]bool)
	if vms, err := s.db.GetVMSaving(host.HsName); err == nil {
		for _, vmJSON := range vms {
			vm, decodeErr := DecodeVMConfig(vmJSON)
			if decodeErr != nil || vm == nil {
				continue
			}
			for _, n := range vm.NatAll {
				localUsed[n.WanPort] = true
			}
		}
	}

	// 先做确定性扫描，保证端口耗尽时一定能给出准确结论
	if port, ok := pickWanPort(low, high, localUsed, reserved); ok {
		return port, nil
	}
	return 0, fmt.Errorf("主机 %s 的端口范围 %d-%d 已全部占用，无法自动分配", host.HsName, low, high)
}

// portRange 返回主机的端口分配范围（未配置时缺省 10000-60000）
func portRange(host *data.HSConfigRow) (int, int) {
	low := host.PortsStart
	if low == 0 {
		low = 10000
	}
	high := host.PortsClose
	if high == 0 {
		high = 60000
	}
	if high <= low {
		high = low + 1
	}
	return low, high
}

// pickWanPort 返回 [low, high] 内第一个既未被本地库分配、
// 也未被路由器占用的端口。两组来源都必须避开：
// localUsed 来自数据库中的虚拟机配置，reserved 来自路由器上现有映射
// （可能包含管理员手工添加、本地库并不知晓的规则）。
func pickWanPort(low, high int, localUsed, reserved map[int]bool) (int, bool) {
	for p := low; p <= high; p++ {
		if !localUsed[p] && !reserved[p] {
			return p, true
		}
	}
	return 0, false
}

// ============================================================
// 网络静态绑定（爱快：DHCP 静态地址 + ARP 绑定）
// 对应 Python 版 BasicServer.NetiKuai / IPBinder_ROS
// ============================================================

// 静态绑定缺省 DNS（对应 Python 版 add_dhcp 的默认值）
const (
	ikuaiDefaultDNS1 = "119.29.29.29"
	ikuaiDefaultDNS2 = "223.5.5.5"
)

// ikuaiClientFor 获取主机对应的爱快客户端。
//
// 返回 (nil, host, nil) 表示该主机未配置爱快路由器：静态绑定属于可选能力，
// 未配置时静默跳过（与 Python 版 NetiKuai 一致），不阻断虚拟机流程。
func (s *NATService) ikuaiClientFor(hsName string) (*ikuaiClient, *data.HSConfigRow, error) {
	host, err := s.db.GetHost(hsName)
	if err != nil {
		return nil, nil, fmt.Errorf("读取主机 %s 配置失败: %w", hsName, err)
	}
	if host == nil {
		return nil, nil, fmt.Errorf("主机 %s 不存在", hsName)
	}
	if strings.TrimSpace(host.IKuaiAddr) == "" {
		return nil, host, nil
	}
	c := s.client(host.IKuaiAddr, host.IKuaiUser, host.IKuaiPass)
	if c == nil {
		return nil, host, fmt.Errorf("爱快路由器配置无效")
	}
	return c, host, nil
}

// ikuaiStaticDNS 返回静态绑定使用的 DNS，优先取主机 ipaddr_ddns 配置。
func ikuaiStaticDNS(host *data.HSConfigRow) (string, string) {
	d1, d2 := ikuaiDefaultDNS1, ikuaiDefaultDNS2
	if host == nil || strings.TrimSpace(host.IPAddrDDNS) == "" {
		return d1, d2
	}
	var dns []string
	if err := json.Unmarshal([]byte(host.IPAddrDDNS), &dns); err != nil {
		return d1, d2
	}
	if len(dns) > 0 && strings.TrimSpace(dns[0]) != "" {
		d1 = strings.TrimSpace(dns[0])
	}
	if len(dns) > 1 && strings.TrimSpace(dns[1]) != "" {
		d2 = strings.TrimSpace(dns[1])
	}
	return d1, d2
}

// BindStatic 为单个 IP/MAC 下发静态绑定。
// vmUUID 作为备注写入，便于在爱快上定位归属；dns1/dns2 为空时取主机配置。
func (s *NATService) BindStatic(hsName, vmUUID, ip, mac, dns1, dns2 string) error {
	ip = strings.TrimSpace(ip)
	mac = strings.TrimSpace(mac)
	if ip == "" || mac == "" {
		return fmt.Errorf("静态绑定参数不完整: ip=%q mac=%q", ip, mac)
	}

	c, host, err := s.ikuaiClientFor(hsName)
	if err != nil || c == nil {
		return err
	}
	defDNS1, defDNS2 := ikuaiStaticDNS(host)
	if strings.TrimSpace(dns1) == "" {
		dns1 = defDNS1
	}
	if strings.TrimSpace(dns2) == "" {
		dns2 = defDNS2
	}

	// DHCP 静态地址：客户端每次都能拿到固定 IP
	if err := c.addDHCPStatic(ip, mac, vmUUID, dns1, dns2); err != nil {
		return err
	}
	// ARP 绑定：防止地址被其他机器抢占
	if err := c.addARP(ip, mac, vmUUID); err != nil {
		return err
	}
	return nil
}

// UnbindStatic 解除单个 IP/MAC 的静态绑定（DHCP 静态地址 + ARP）。
// 记录不存在时视为已解绑，保证重复调用幂等。
func (s *NATService) UnbindStatic(hsName, ip, mac string) error {
	ip = strings.TrimSpace(ip)
	mac = strings.TrimSpace(mac)
	if ip == "" && mac == "" {
		return fmt.Errorf("解绑静态绑定需要 ip 或 mac")
	}

	c, _, err := s.ikuaiClientFor(hsName)
	if err != nil || c == nil {
		return err
	}

	var errs []string
	if err := c.deleteDHCPStatic(ip, mac); err != nil && !errors.Is(err, errIKuaiNotFound) {
		errs = append(errs, err.Error())
	}
	if err := c.deleteARP(ip, mac); err != nil && !errors.Is(err, errIKuaiNotFound) {
		errs = append(errs, err.Error())
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// StaticBindResult 描述一次静态绑定的实际执行情况。
//
// 必须区分 Bound 与 Skipped：网卡缺少 IP/MAC 时绑定无处可下，
// 若这种情况也返回「成功」，调用方会把「什么都没做」记成「绑定成功」，
// 让真正的配置缺失在日志里彻底消失。
type StaticBindResult struct {
	// Bound 实际下发成功的网卡数
	Bound int
	// Skipped 因缺少 IP 或 MAC 而跳过的网卡描述
	Skipped []string
	// NoNIC 表示虚拟机没有任何网卡配置
	NoNIC bool
	// NotConfigured 表示主机未配置爱快路由器。
	// 静态绑定属于可选能力（部分主机走平台自有通道），此时应静默跳过，
	// 不能按「失败」或「跳过」上报，否则每台虚拟机的创建日志都会带噪音。
	NotConfigured bool
}

// Summary 返回便于打日志的一句话摘要
func (r StaticBindResult) Summary() string {
	switch {
	case r.NotConfigured:
		return "主机未配置爱快路由器"
	case r.NoNIC:
		return "虚拟机没有网卡配置"
	case r.Bound == 0 && len(r.Skipped) == 0:
		return "没有需要绑定的网卡"
	default:
		return fmt.Sprintf("成功 %d 张网卡，跳过 %d 张（%s）",
			r.Bound, len(r.Skipped), strings.Join(r.Skipped, ", "))
	}
}

// StaticBindConfigured 返回主机是否配置了爱快路由器。
// 供 HTTP 接口在「未配置」时明确报错，而不是回一个假的成功。
func (s *NATService) StaticBindConfigured(hsName string) bool {
	c, _, err := s.ikuaiClientFor(hsName)
	return err == nil && c != nil
}

// BindStaticForVM 为虚拟机所有已配置 IP/MAC 的网卡下发静态绑定。
// 单张网卡失败不影响其余网卡，返回的错误里带上失败明细。
func (s *NATService) BindStaticForVM(hsName, vmUUID string, vm *model.VMConfig) (StaticBindResult, error) {
	var res StaticBindResult
	if vm == nil || len(vm.NicAll) == 0 {
		res.NoNIC = true
		return res, nil
	}

	// 先判定主机是否配置了爱快：未配置属于可选能力缺失，静默跳过
	if c, _, err := s.ikuaiClientFor(hsName); err != nil {
		return res, err
	} else if c == nil {
		res.NotConfigured = true
		return res, nil
	}

	var errs []string
	for _, item := range sortedNics(vm.NicAll) {
		nic := item.Nic
		if strings.TrimSpace(nic.IP4Addr) == "" || strings.TrimSpace(nic.MacAddr) == "" {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s(ip=%q mac=%q)",
				item.Name, nic.IP4Addr, nic.MacAddr))
			continue
		}
		dns1, dns2 := firstDNS(nic.DNSAddr)
		if err := s.BindStatic(hsName, vmUUID, nic.IP4Addr, nic.MacAddr, dns1, dns2); err != nil {
			errs = append(errs, fmt.Sprintf("%s(%s): %v", nic.IP4Addr, nic.MacAddr, err))
			continue
		}
		res.Bound++
	}
	if len(errs) > 0 {
		return res, errors.New(strings.Join(errs, "; "))
	}
	return res, nil
}

// UnbindStaticForVM 解除虚拟机所有网卡 IP 的静态绑定（删除虚拟机时调用）
func (s *NATService) UnbindStaticForVM(hsName string, vm *model.VMConfig) error {
	if vm == nil || len(vm.NicAll) == 0 {
		return nil
	}
	var errs []string
	for _, item := range sortedNics(vm.NicAll) {
		nic := item.Nic
		if strings.TrimSpace(nic.IP4Addr) == "" {
			continue
		}
		if err := s.UnbindStatic(hsName, nic.IP4Addr, nic.MacAddr); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", nic.IP4Addr, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// namedNic 网卡名与其配置（map 的键即网卡名，可能未写入 NCConfig.NicName）
type namedNic struct {
	Name string
	Nic  model.NCConfig
}

// sortedNics 按网卡名排序返回网卡配置：map 遍历顺序随机，
// 固定顺序可让下发日志与失败明细保持稳定、可对比。
func sortedNics(nics map[string]model.NCConfig) []namedNic {
	names := make([]string, 0, len(nics))
	for name := range nics {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]namedNic, 0, len(nics))
	for _, name := range names {
		out = append(out, namedNic{Name: name, Nic: nics[name]})
	}
	return out
}

// firstDNS 取网卡自身配置的 DNS，缺省返回空串（由主机配置兜底）
func firstDNS(list []string) (string, string) {
	d1, d2 := "", ""
	if len(list) > 0 {
		d1 = strings.TrimSpace(list[0])
	}
	if len(list) > 1 {
		d2 = strings.TrimSpace(list[1])
	}
	return d1, d2
}
