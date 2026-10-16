// Package driver 驱动一致性回归测试。
//
// 目的：把「禁止假成功」固化为可执行的断言。
//
// 背景：本项目历史上出现过多处「接口存在、返回 Success:true，但平台侧什么都没做」
// 的实现（典型如数据盘删除只改内存、磁盘移交只改两个内存对象），
// 造成前端显示成功而真实资源未变更，属最高危的失败模式。
//
// 本测试对全部已注册引擎驱动执行「操作不存在的虚拟机」用例，
// 要求一律不返回成功，从而保证：
//   - 不存在「无条件返回成功」的空壳实现；
//   - 驱动在找不到目标时不会 panic。
package driver

import (
	"strings"
	"testing"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// registeredEngineTypes 全部已注册的虚拟化引擎类型（与 engine_config.go 保持一致）
var registeredEngineTypes = []string{
	"VMWareSetup", "PromoxSetup", "VirtualBoxs", "QEMUServer", "HyperVSetup",
	"vSphereESXi", "LxContainer", "OCInterface", "MemuAndroid", "QingzhouYun",
	"SmolVM",
}

// testConfig 构造一个不会真正连上任何平台的测试配置
func testConfig(t *testing.T, engineType string) *model.HSConfig {
	t.Helper()
	dir := t.TempDir()
	cfg := model.NewHSConfig()
	cfg.ServerName = "test-" + engineType
	cfg.ServerType = engineType
	cfg.ServerAddr = "127.0.0.1"
	cfg.ServerUser = "root"
	cfg.ServerPass = "invalid-password-for-test"
	cfg.SystemPath = dir
	cfg.ImagesPath = dir
	cfg.BackupPath = dir
	cfg.ExternPath = dir
	cfg.DvdromPath = dir
	cfg.FilterName = "test"
	return cfg
}

// missingVM 一个必然不存在的虚拟机标识
const missingVM = "hostagent-nonexistent-vm-000"

// TestAllEnginesCreateDriver 所有注册引擎都能构造出驱动实例
func TestAllEnginesCreateDriver(t *testing.T) {
	for _, engine := range registeredEngineTypes {
		engine := engine
		t.Run(engine, func(t *testing.T) {
			if d := NewDriver(testConfig(t, engine)); d == nil {
				t.Fatalf("引擎 %s 未注册驱动实现（NewDriver 返回 nil）", engine)
			}
		})
	}
}

// TestDestructiveOpsNeverFakeSuccess 破坏性 / 不可用操作绝不假成功。
//
// 对不存在的虚拟机执行这些操作，必须不返回成功；
// 若某个驱动返回成功，说明它没有做任何真实校验（或根本没操作平台）。
func TestDestructiveOpsNeverFakeSuccess(t *testing.T) {
	op := func(name string, fn func(d HostDriver) (*model.ZMessage, error)) {
		for _, engine := range registeredEngineTypes {
			engine := engine
			d := NewDriver(testConfig(t, engine))
			if d == nil {
				continue
			}
			t.Run(engine+"/"+name, func(t *testing.T) {
				msg, err := fn(d)
				// 驱动允许返回 error（例如连接失败），但绝不能返回「成功」
				if err == nil && msg != nil && msg.Success {
					t.Fatalf("引擎 %s 的 %s 对不存在的虚拟机返回了成功（假成功）: %s",
						engine, name, msg.Message)
				}
			})
		}
	}

	op("DeleteHDD", func(d HostDriver) (*model.ZMessage, error) {
		return d.DeleteHDD(missingVM, "data1")
	})
	op("TransferHDD", func(d HostDriver) (*model.ZMessage, error) {
		return d.TransferHDD(missingVM, &model.SDConfig{HddName: "data1"}, "target-vm")
	})
	op("MountUSB", func(d HostDriver) (*model.ZMessage, error) {
		return d.MountUSB(missingVM, "046d:c52b", true)
	})
	op("MountISO", func(d HostDriver) (*model.ZMessage, error) {
		return d.MountISO(missingVM, &model.IMConfig{IsoName: "iso1", IsoFile: "x.iso"}, true)
	})
	op("MountHDD", func(d HostDriver) (*model.ZMessage, error) {
		return d.MountHDD(missingVM, &model.SDConfig{HddName: "data1", HddSize: 1024}, true)
	})
	op("SetupUSB", func(d HostDriver) (*model.ZMessage, error) {
		return d.SetupUSB(missingVM, &model.USBInfos{VidUUID: "046d", PidUUID: "c52b"}, "046d:c52b", true)
	})
	op("SetupPCI", func(d HostDriver) (*model.ZMessage, error) {
		return d.SetupPCI(missingVM, &model.VFConfig{GpuUUID: "0000:01:00.0"}, "0000:01:00.0", true)
	})
	op("SetPassword", func(d HostDriver) (*model.ZMessage, error) {
		return d.SetPassword(missingVM, "new-password")
	})
}

// TestSetVMPowerStateRejectsUnknownVM 状态写入必须基于真实存在的记录，
// 否则调度器会把状态写到「幽灵对象」上而前端永远看不到更新。
func TestSetVMPowerStateRejectsUnknownVM(t *testing.T) {
	d := NewDriver(testConfig(t, "QEMUServer"))
	if d == nil {
		t.Skip("QEMU 驱动未注册")
	}
	if ok := d.SetVMPowerState(missingVM, model.VMPowerStarted); ok {
		t.Fatalf("对不存在的虚拟机写入电源状态应返回 false")
	}
	if ok := d.SetVMDeleted(missingVM, true); ok {
		t.Fatalf("对不存在的虚拟机写入删除标记应返回 false")
	}
	if ok := d.RemoveVMSaving(missingVM); ok {
		t.Fatalf("移除不存在的虚拟机记录应返回 false")
	}
}

// TestEngineTypesAreWellFormed 引擎类型必须非空且可构造驱动（防止配置与实现脱节）。
func TestEngineTypesAreWellFormed(t *testing.T) {
	for _, engine := range registeredEngineTypes {
		if strings.TrimSpace(engine) == "" {
			t.Fatal("引擎类型为空")
		}
		if d := NewDriver(testConfig(t, engine)); d == nil {
			t.Fatalf("引擎配置对外暴露了 %s，但无法创建驱动", engine)
		}
	}
}
