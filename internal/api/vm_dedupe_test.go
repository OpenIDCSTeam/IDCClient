package api

import (
	"testing"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// TestVMConfigScore 校验「保留哪条记录」的评分：有业务数据的一条应高于空壳记录
func TestVMConfigScore(t *testing.T) {
	shell := model.NewVMConfig()
	shell.OwnAll = map[string]model.UserMask{}
	shell.OsName = ""

	created := model.NewVMConfig() // NewVMConfig 默认带 admin 所有者
	created.OsName = "win-11-lite-cloud.qcow2"
	created.Backups = []model.VMBackup{{BackupName: "b1"}}

	if vmConfigScore(created) <= vmConfigScore(shell) {
		t.Fatalf("创建流程写入的记录应评分更高: created=%d shell=%d",
			vmConfigScore(created), vmConfigScore(shell))
	}
	if vmConfigScore(nil) != -1 {
		t.Fatal("nil 配置应返回 -1")
	}
}

// TestMergeVMConfigInto 校验合并时「互补而不覆盖」的语义
func TestMergeVMConfigInto(t *testing.T) {
	dst := model.NewVMConfig()
	dst.VmUUID = "pve-9p9Q9Ual"
	dst.OsName = "keep-os"
	dst.OwnAll = map[string]model.UserMask{"admin": model.FullUserMask()}
	dst.Backups = []model.VMBackup{{BackupName: "b1"}}
	dst.NatAll = []model.PortData{{WanPort: 10000}}

	src := model.NewVMConfig()
	src.VmUUID = "pve_9p9Q9Ual"
	src.OsName = "other-os" // 不应覆盖
	src.OsPass = "Secret123"
	src.OwnAll = map[string]model.UserMask{
		"admin": model.FullUserMask(), // 重复用户，保持 dst 的
		"user1": model.FullUserMask(), // 新用户，应合并进来
	}
	src.Backups = []model.VMBackup{
		{BackupName: "b1"}, // 重复备份，应去重
		{BackupName: "b2"},
	}
	src.NatAll = []model.PortData{{WanPort: 10001}}

	mergeVMConfigInto(dst, src)

	if dst.OsName != "keep-os" {
		t.Fatalf("已有值不应被覆盖: %q", dst.OsName)
	}
	if dst.OsPass != "Secret123" {
		t.Fatalf("空缺字段应被补齐: %q", dst.OsPass)
	}
	if _, ok := dst.OwnAll["user1"]; !ok {
		t.Fatal("另一条记录的用户应被合并进来")
	}
	if len(dst.OwnAll) != 2 {
		t.Fatalf("用户数应为 2，实际 %d", len(dst.OwnAll))
	}
	if len(dst.Backups) != 2 {
		t.Fatalf("备份应去重后为 2 条，实际 %d", len(dst.Backups))
	}
	if len(dst.NatAll) != 2 {
		t.Fatalf("NAT 规则应追加为 2 条，实际 %d", len(dst.NatAll))
	}
	if dst.VmUUID != "pve-9p9Q9Ual" {
		t.Fatalf("VmUUID 应保持保留者的值: %q", dst.VmUUID)
	}
}

// TestMergeVMConfigIntoDeletedFlagIgnored 确认软删除标记不会被另一条记录带过来
func TestMergeVMConfigIntoDeletedFlagIgnored(t *testing.T) {
	dst := model.NewVMConfig()
	src := model.NewVMConfig()
	src.VmDeleted = true

	mergeVMConfigInto(dst, src)
	if dst.VmDeleted {
		t.Fatal("VmDeleted 不应参与合并")
	}
}

// TestMergeVMConfigIntoNil 确认 nil 入参不会 panic
func TestMergeVMConfigIntoNil(t *testing.T) {
	mergeVMConfigInto(nil, model.NewVMConfig())
	mergeVMConfigInto(model.NewVMConfig(), nil)
}
