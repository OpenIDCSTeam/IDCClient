package driver

import (
	"strings"
	"testing"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// TestProxmoxNetConf 覆盖 PVE 网卡配置生成（对应 Python ProxmoxQemu.net_conf）。
//
// 回归背景：DHCP 静态绑定与 ARP 绑定都以 MAC 为键。若下发给 PVE 的网卡不带
// macaddr，平台会随机生成一个 MAC，与路由器上绑定的 MAC 对不上，虚拟机拿不到
// 预期 IP；网桥选错则虚拟机根本不在地址池所在的二层网络里。
func TestProxmoxNetConf(t *testing.T) {
	newDriver := func(nat, pub string) *ProxmoxDriver {
		return NewProxmoxDriver(&model.HSConfig{
			ServerAddr: "192.168.1.50",
			NetworkNat: nat,
			NetworkPub: pub,
		})
	}

	t.Run("nat 网卡走 network_nat 并携带 MAC", func(t *testing.T) {
		d := newDriver("vmbr1", "vmbr0")
		cfg := &model.VMConfig{NicAll: map[string]model.NCConfig{
			"ethernet0": {NicName: "ethernet0", IP4Addr: "10.2.20.100", MacAddr: "10:f6:0a:02:14:64"},
		}}
		if got := d.netConf(cfg)["net0"]; got != "e1000e,bridge=vmbr1,macaddr=10:f6:0a:02:14:64" {
			t.Fatalf("net0 = %q", got)
		}
	})

	t.Run("pub 网卡走 network_pub", func(t *testing.T) {
		d := newDriver("vmbr1", "vmbr0")
		cfg := &model.VMConfig{NicAll: map[string]model.NCConfig{
			"ethernet0": {NicName: "ethernet0", NicType: "pub", MacAddr: "00:1c:0a:02:14:64"},
		}}
		if got := d.netConf(cfg)["net0"]; got != "e1000e,bridge=vmbr0,macaddr=00:1c:0a:02:14:64" {
			t.Fatalf("net0 = %q", got)
		}
	})

	t.Run("nic_type 缺省为 nat，无 MAC 时不带 macaddr", func(t *testing.T) {
		d := newDriver("vmbr1", "vmbr0")
		cfg := &model.VMConfig{NicAll: map[string]model.NCConfig{
			"ethernet0": {NicName: "ethernet0"},
		}}
		if got := d.netConf(cfg)["net0"]; got != "e1000e,bridge=vmbr1" {
			t.Fatalf("net0 = %q", got)
		}
	})

	t.Run("多网卡按名称稳定编号", func(t *testing.T) {
		d := newDriver("vmbr1", "vmbr0")
		cfg := &model.VMConfig{NicAll: map[string]model.NCConfig{
			"ethernet1": {NicType: "pub"},
			"ethernet0": {},
		}}
		got := d.netConf(cfg)
		if len(got) != 2 ||
			!strings.Contains(got["net0"], "vmbr1") ||
			!strings.Contains(got["net1"], "vmbr0") {
			t.Fatalf("多网卡编号错误: %v", got)
		}
	})

	t.Run("网桥未配置时回退 vmbr0；无网卡时返回空（与 Python 一致）", func(t *testing.T) {
		d := newDriver("", "")
		cfg := &model.VMConfig{NicAll: map[string]model.NCConfig{"ethernet0": {}}}
		if got := d.netConf(cfg)["net0"]; got != "e1000e,bridge=vmbr0" {
			t.Fatalf("net0 = %q", got)
		}
		if got := d.netConf(&model.VMConfig{}); len(got) != 0 {
			t.Fatalf("nic_all 为空应返回空配置，实际 %v", got)
		}
	})
}
