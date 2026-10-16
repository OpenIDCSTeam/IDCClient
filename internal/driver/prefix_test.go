package driver

import (
	"testing"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// TestMatchFilterPrefix 覆盖「记录是否匹配 filter_name 前缀」的判定。
//
// 该判定用于清理历史错前缀记录（handleVMCleanup）与「仅移除记录」，
// 判错会误删仍在纳管的虚拟机，因此对两种名称存放方式都做校验：
//   - ESXi / Hyper-V / Workstation 等使用 OsName；
//   - Proxmox 直接把名称（"-" 已替换为 "_"）当作 VmUUID。
func TestMatchFilterPrefix(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		cfg    *model.VMConfig
		want   bool
	}{
		{"空前缀表示不过滤", "", &model.VMConfig{VmUUID: "anything"}, true},
		{"前缀含空格自动裁剪", " web- ", &model.VMConfig{VmUUID: "web_1"}, true},
		{"OsName 匹配", "web-", &model.VMConfig{VmUUID: "uuid-1", OsName: "web-01"}, true},
		{"OsName 不匹配", "web-", &model.VMConfig{VmUUID: "uuid-1", OsName: "db-01"}, false},
		{"Proxmox 下划线归一化后匹配", "web-", &model.VMConfig{VmUUID: "web_01"}, true},
		{"Proxmox 前缀原样命中", "web_", &model.VMConfig{VmUUID: "web_01"}, true},
		{"Proxmox 不匹配", "web-", &model.VMConfig{VmUUID: "db_01"}, false},
		{"两个字段任一匹配即保留", "web-", &model.VMConfig{VmUUID: "db_01", OsName: "web-02"}, true},
		{"名称为空且前缀非空视为不匹配", "web-", &model.VMConfig{VmUUID: ""}, false},
		{"nil 配置在前缀非空时不保留", "web-", nil, false},
		{"nil 配置在前缀为空时保留", "", nil, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchFilterPrefix(tc.prefix, tc.cfg); got != tc.want {
				t.Fatalf("MatchFilterPrefix(%q, %+v) = %v, want %v", tc.prefix, tc.cfg, got, tc.want)
			}
		})
	}
}

// TestProxmoxSameVMName 覆盖 Proxmox 命名变体判重（历史 "_"/"-" 差异）
func TestProxmoxSameVMName(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"pve-9p9Q9Ual", "pve-9p9Q9Ual", true},
		{"pve-9p9Q9Ual", "pve_9p9Q9Ual", true},
		{"pve_9p9Q9Ual", "pve-9p9Q9Ual", true},
		{"pve-9p9Q9Ual", "pve-9p9Q9Ua2", false},
		{"pve-9p9Q9Ual", "", false},
		{"", "pve-9p9Q9Ual", false},
		{"", "", false},
	}

	for _, tc := range cases {
		if got := ProxmoxSameVMName(tc.a, tc.b); got != tc.want {
			t.Fatalf("ProxmoxSameVMName(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestFindExistingVMKeyLocked 确认扫描时能复用历史下划线键，避免重复纳管
func TestFindExistingVMKeyLocked(t *testing.T) {
	d := NewProxmoxDriver(&model.HSConfig{})
	d.VmSaving["pve_9p9Q9Ual"] = model.NewVMConfig()

	if got := d.findExistingVMKeyLocked("pve-9p9Q9Ual"); got != "pve_9p9Q9Ual" {
		t.Fatalf("应复用历史键，实际 %q", got)
	}
	if got := d.findExistingVMKeyLocked("other-vm"); got != "" {
		t.Fatalf("不存在的名称应返回空串，实际 %q", got)
	}
}

// TestRemoveVMSaving 确认只移除内存记录且不影响其它条目
func TestRemoveVMSaving(t *testing.T) {
	b := NewBaseDriver(&model.HSConfig{})
	b.VmSaving["vm_a"] = model.NewVMConfig()
	b.VmSaving["vm_b"] = model.NewVMConfig()

	if !b.RemoveVMSaving("vm_a") {
		t.Fatal("移除已存在的记录应返回 true")
	}
	if _, ok := b.VmSaving["vm_a"]; ok {
		t.Fatal("vm_a 应已被移除")
	}
	if _, ok := b.VmSaving["vm_b"]; !ok {
		t.Fatal("vm_b 不应被移除")
	}
	if b.RemoveVMSaving("vm_a") {
		t.Fatal("移除不存在的记录应返回 false")
	}
}
