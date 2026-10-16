// Package service 网络配置检查与 IP 自动分配
//
// 对应 Python 版 BasicServer.NetCheck + MainObject/Config/IPConfig.py：
// 虚拟机创建/更新前，从主机的 ipaddr_maps 地址池中为缺地址的网卡分配 IP，
// 并按 IP 推导 MAC（NCConfig.send_mac）。
//
// 这一步是 DHCP/ARP 静态绑定的前提：若网卡缺少 IP/MAC，
// 后续 BindStaticForVM 会直接跳过，导致路由器上一条绑定规则都没有。
package service

import (
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// IPAllocService IP 地址分配服务
type IPAllocService struct {
	db *data.DB
}

// NewIPAllocService 创建 IP 分配服务
func NewIPAllocService(db *data.DB) *IPAllocService {
	return &IPAllocService{db: db}
}

// ipPool 单个地址池配置（对应 ipaddr_maps 中的一项）
type ipPool struct {
	Name   string
	Vers   string // ipv4 / ipv6
	Type   string // nat / pub
	From   string
	Nums   int
	Gate   string
	Mask   string
}

// NetCheck 检查并自动分配虚拟机网卡 IP（对应 Python BasicServer.NetCheck）
//
// 行为：
//   - 仅对 ip4_addr / ip6_addr 为空的网卡分配；
//   - IPv4 分配失败直接返回错误（与 Python 一致）；
//   - IPv6 分配失败静默跳过；
//   - 分配成功后回填 dns_addr，并按 IP 推导 MAC（若 MAC 为空）。
func (s *IPAllocService) NetCheck(hsName string, vm *model.VMConfig) error {
	if vm == nil {
		return fmt.Errorf("虚拟机配置为空")
	}
	if len(vm.NicAll) == 0 {
		return nil
	}

	host, err := s.db.GetHost(hsName)
	if err != nil {
		return fmt.Errorf("读取主机配置失败: %w", err)
	}
	if host == nil {
		return fmt.Errorf("主机不存在: %s", hsName)
	}

	pools := parseIPPools(host.IPAddrMaps)
	dns := parseIPDNS(host.IPAddrDDNS)
	allocated := s.collectAllocatedIPs(hsName, vm.VmUUID)

	// 网卡按名称排序，保证分配顺序稳定（map 遍历无序会导致同一配置多次分配结果不同）
	names := make([]string, 0, len(vm.NicAll))
	for name := range vm.NicAll {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		nic := vm.NicAll[name]
		needV4 := strings.TrimSpace(nic.IP4Addr) == ""
		needV6 := strings.TrimSpace(nic.IP6Addr) == ""
		if !needV4 && !needV6 {
			// 已有地址：仅补全缺失的 MAC
			nic.EnsureMAC()
			vm.NicAll[name] = nic
			continue
		}

		nicType := nic.EffectiveNicType()

		// 分配 IPv4：失败即返回错误（与 Python 一致）
		if needV4 {
			ip, gate, mask, ok := allocateIP(pools, "ipv4", nicType, allocated)
			if !ok {
				return fmt.Errorf(
					"无法为网卡 %s 分配IPv4地址（类型: %s）：地址池已耗尽或未配置对应网段",
					name, nicType)
			}
			nic.IP4Addr = ip
			if gate != "" {
				nic.NicGate = gate
			}
			if mask != "" {
				nic.NicMask = mask
			}
			allocated[ip] = true
			if len(dns) > 0 {
				nic.DNSAddr = append([]string(nil), dns...)
			}
		}

		// 分配 IPv6：失败不报错（与 Python 一致）
		if needV6 {
			if ip, _, _, ok := allocateIP(pools, "ipv6", nicType, allocated); ok {
				nic.IP6Addr = ip
				allocated[ip] = true
			}
		}

		// 按 IP 推导 MAC（与 Python NCConfig.send_mac 一致）
		nic.EnsureMAC()
		vm.NicAll[name] = nic
	}

	return nil
}

// parseIPPools 解析主机 ipaddr_maps（JSON 字符串或已解析对象）
func parseIPPools(raw interface{}) []ipPool {
	m := toStringMap(raw)
	if len(m) == 0 {
		return nil
	}

	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)

	pools := make([]ipPool, 0, len(names))
	for _, name := range names {
		item := toStringMap(m[name])
		if len(item) == 0 {
			continue
		}
		pools = append(pools, ipPool{
			Name: name,
			Vers: strings.ToLower(strings.TrimSpace(strValue(item["vers"]))),
			Type: strings.ToLower(strings.TrimSpace(strValue(item["type"]))),
			From: strings.TrimSpace(strValue(item["from"])),
			Nums: intValue(item["nums"]),
			Gate: strValue(item["gate"]),
			Mask: strValue(item["mask"]),
		})
	}
	return pools
}

// parseIPDNS 解析主机 ipaddr_ddns
func parseIPDNS(raw interface{}) []string {
	switch v := raw.(type) {
	case []string:
		return append([]string(nil), v...)
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s := strings.TrimSpace(strValue(item)); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		s := strings.TrimSpace(v)
		if s == "" || s == "[]" {
			return nil
		}
		var arr []string
		if err := json.Unmarshal([]byte(s), &arr); err == nil {
			return arr
		}
	}
	return nil
}

// allocateIP 从地址池中分配一个未占用的 IP（对应 Python IPConfig.allocate_ip）
func allocateIP(pools []ipPool, vers, nicType string, allocated map[string]bool) (ip, gate, mask string, ok bool) {
	for _, pool := range pools {
		if pool.Vers != vers || pool.Type != nicType {
			continue
		}
		if pool.From == "" || pool.Nums <= 0 {
			continue
		}

		base := net.ParseIP(pool.From)
		if base == nil {
			continue
		}
		// 统一为 4/16 字节表示，便于递增
		if v4 := base.To4(); v4 != nil {
			base = v4
		} else {
			base = base.To16()
		}
		if base == nil {
			continue
		}

		for i := 0; i < pool.Nums; i++ {
			candidate := addIP(base, i)
			if candidate == nil {
				break
			}
			ipStr := candidate.String()
			if !allocated[ipStr] {
				return ipStr, pool.Gate, pool.Mask, true
			}
		}
	}
	return "", "", "", false
}

// addIP 返回 base + n 的 IP
func addIP(base net.IP, n int) net.IP {
	if len(base) == 0 {
		return nil
	}
	ip := make(net.IP, len(base))
	copy(ip, base)
	for i := len(ip) - 1; i >= 0 && n > 0; i-- {
		sum := int(ip[i]) + n
		ip[i] = byte(sum & 0xFF)
		n = sum >> 8
	}
	return ip
}

// collectAllocatedIPs 收集主机下所有虚拟机已占用的 IP（排除当前虚拟机自身）
func (s *IPAllocService) collectAllocatedIPs(hsName, excludeUUID string) map[string]bool {
	result := make(map[string]bool)

	raw, err := s.db.GetVMSaving(hsName)
	if err != nil {
		return result
	}
	for vmUUID, jsonStr := range raw {
		if excludeUUID != "" && vmUUID == excludeUUID {
			continue
		}
		cfg, err := model.VMConfigFromJSON([]byte(jsonStr))
		if err != nil {
			continue
		}
		for _, nic := range cfg.NicAll {
			if ip := strings.TrimSpace(nic.IP4Addr); ip != "" {
				result[ip] = true
			}
			if ip := strings.TrimSpace(nic.IP6Addr); ip != "" {
				result[ip] = true
			}
		}
	}
	return result
}

// ============================================================
// 类型转换辅助
// ============================================================

// toStringMap 把 interface{} 归一化为 map[string]interface{}
//
// ipaddr_maps 在不同来源下可能是：
//   - map[string]interface{}（内存对象）
//   - string（数据库 JSON 列）
func toStringMap(v interface{}) map[string]interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		return t
	case string:
		s := strings.TrimSpace(t)
		if s == "" || s == "{}" {
			return nil
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(s), &m); err == nil {
			return m
		}
	case nil:
		return nil
	}
	return nil
}

// strValue 把 interface{} 转成字符串
func strValue(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case bool:
		if t {
			return "true"
		}
		return "false"
	case nil:
		return ""
	}
	return ""
}

// intValue 把 interface{} 转成整数
func intValue(v interface{}) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n)
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n
		}
	}
	return 0
}
