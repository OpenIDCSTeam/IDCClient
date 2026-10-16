package service

import (
	"strings"
	"testing"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
)

// newTestDB 创建临时数据库（测试结束后自动清理）
func newTestDB(t *testing.T) *data.DB {
	t.Helper()
	db, err := data.NewDB(t.TempDir())
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestGenerateAccessKey 校验密钥长度与字符集（16 位大小写字母 + 数字）
func TestGenerateAccessKey(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 200; i++ {
		key, err := GenerateAccessKey()
		if err != nil {
			t.Fatalf("生成访问密钥失败: %v", err)
		}
		if len(key) != AccessKeyLength {
			t.Fatalf("密钥长度应为 %d，实际 %d: %q", AccessKeyLength, len(key), key)
		}
		if !IsValidAccessKey(key) {
			t.Fatalf("生成的密钥未通过格式校验: %q", key)
		}
		if seen[key] {
			t.Fatalf("生成的密钥重复: %q", key)
		}
		seen[key] = true
	}
}

// TestIsValidAccessKey 校验格式边界
func TestIsValidAccessKey(t *testing.T) {
	cases := []struct {
		key  string
		want bool
	}{
		{"abcdefghijklmnop", true},
		{"ABCDEFGHIJKLMNOP", true},
		{"0123456789ABCDef", true},
		{"abcdefghijklmno", false},  // 15 位
		{"abcdefghijklmnopq", false}, // 17 位
		{"abcdefghijklmno!", false},  // 含非法字符
		{"abcdefghijklmno-", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsValidAccessKey(c.key); got != c.want {
			t.Errorf("IsValidAccessKey(%q) = %v, want %v", c.key, got, c.want)
		}
	}
}

// TestAccessKeyServiceLifecycle 覆盖生成 / 查询 / 反查 / 重置 / 撤销
func TestAccessKeyServiceLifecycle(t *testing.T) {
	db := newTestDB(t)
	svc := NewAccessKeyService(db)

	if _, enabled, err := svc.Get("h1", "vm1"); err != nil || enabled {
		t.Fatalf("初始状态应为未启用: enabled=%v err=%v", enabled, err)
	}

	key, err := svc.Reset("h1", "vm1", "tester")
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	if !IsValidAccessKey(key) {
		t.Fatalf("生成的密钥格式非法: %q", key)
	}

	got, enabled, err := svc.Get("h1", "vm1")
	if err != nil || !enabled || got != key {
		t.Fatalf("查询密钥失败: got=%q enabled=%v err=%v", got, enabled, err)
	}

	hsName, vmUUID, ok := svc.Resolve(key)
	if !ok || hsName != "h1" || vmUUID != "vm1" {
		t.Fatalf("按密钥反查失败: %s/%s ok=%v", hsName, vmUUID, ok)
	}

	// 重置后旧密钥必须立即失效
	newKey, err := svc.Reset("h1", "vm1", "tester")
	if err != nil {
		t.Fatalf("重置密钥失败: %v", err)
	}
	if newKey == key {
		t.Fatalf("重置后密钥不应与旧密钥相同")
	}
	if _, _, ok := svc.Resolve(key); ok {
		t.Fatalf("旧密钥在重置后仍然有效")
	}
	if _, _, ok := svc.Resolve(newKey); !ok {
		t.Fatalf("新密钥无法反查")
	}

	// 撤销后彻底失效
	removed, err := svc.Revoke("h1", "vm1")
	if err != nil || !removed {
		t.Fatalf("撤销密钥失败: removed=%v err=%v", removed, err)
	}
	if _, _, ok := svc.Resolve(newKey); ok {
		t.Fatalf("撤销后密钥仍然有效")
	}
	if removedAgain, err := svc.Revoke("h1", "vm1"); err != nil || removedAgain {
		t.Fatalf("重复撤销应返回 removed=false: %v %v", removedAgain, err)
	}
}

// TestAccessKeyIsolatedPerVM 不同虚拟机之间的密钥互不影响
func TestAccessKeyIsolatedPerVM(t *testing.T) {
	db := newTestDB(t)
	svc := NewAccessKeyService(db)

	k1, err := svc.Reset("h1", "vm1", "tester")
	if err != nil {
		t.Fatalf("生成 vm1 密钥失败: %v", err)
	}
	k2, err := svc.Reset("h1", "vm2", "tester")
	if err != nil {
		t.Fatalf("生成 vm2 密钥失败: %v", err)
	}
	if k1 == k2 {
		t.Fatalf("不同虚拟机的密钥不应相同")
	}

	if _, err := svc.Revoke("h1", "vm1"); err != nil {
		t.Fatalf("撤销 vm1 密钥失败: %v", err)
	}
	if _, enabled, _ := svc.Get("h1", "vm2"); !enabled {
		t.Fatalf("撤销 vm1 密钥不应影响 vm2")
	}
	if _, _, ok := svc.Resolve(k2); !ok {
		t.Fatalf("vm2 密钥应仍然有效")
	}
}

// TestDeleteVMConfigRemovesAccessKey 删除虚拟机配置时必须同时回收访问密钥
func TestDeleteVMConfigRemovesAccessKey(t *testing.T) {
	db := newTestDB(t)
	svc := NewAccessKeyService(db)

	key, err := svc.Reset("h1", "vm1", "tester")
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	if err := db.DeleteVMConfig("h1", "vm1"); err != nil {
		t.Fatalf("删除虚拟机配置失败: %v", err)
	}
	if _, _, ok := svc.Resolve(key); ok {
		t.Fatalf("删除虚拟机后访问密钥不应残留")
	}
}

// TestVMTempVirtualUserNaming 虚拟用户名需与 Python 版保持一致（{hs_name}-{vm_uuid}）
func TestVMTempVirtualUserNaming(t *testing.T) {
	name := VMTempVirtualUser("host1", "vm-abc")
	if name != "host1-vm-abc" {
		t.Fatalf("虚拟用户名不符合预期: %q", name)
	}
	if !strings.Contains(name, "-") {
		t.Fatalf("虚拟用户名应包含分隔符: %q", name)
	}
}
