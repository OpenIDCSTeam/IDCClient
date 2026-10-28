package service

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/driver"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

func backupTestService(t *testing.T) *HostService {
	t.Helper()
	db, err := data.NewDB(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewHostService(db)
}

func backupTestConfig(name string) *model.HSConfig {
	cfg := model.NewHSConfig()
	cfg.ServerName = name
	cfg.ServerType = "PromoxSetup"
	cfg.ServerAddr = "192.0.2.10"
	cfg.ServerPass = "test-only-password"
	return cfg
}

func backupTestFile(t *testing.T, configs ...*model.HSConfig) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(HostConfigBackup{
		Format: HostConfigBackupFormat, Version: HostConfigBackupVersion,
		ExportedAt: "2026-10-28T00:00:00Z", Hosts: configs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func hostConfigJSONEqual(a, b *model.HSConfig) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && string(left) == string(right)
}

func TestHostConfigBackupRoundTrip(t *testing.T) {
	source := backupTestService(t)
	cfg := backupTestConfig("source-host")
	cfg.SystemMaps = []model.OSConfig{{SysName: "disabled-image", SysFile: "disabled.qcow2", SysFlag: false}}
	cfg.ServerPlan["small"] = model.NewVMConfig()
	cfg.ExtendData["api_token"] = "test-only-token"
	cfg.IPAddrDDNS = []string{}
	cfg.NCpuPrice = 1.25
	if err := source.RestoreHostConfigs(&HostConfigBackup{Hosts: []*model.HSConfig{cfg}}, false); err != nil {
		t.Fatal(err)
	}
	exported, err := source.ExportHostConfigs("")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(exported)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeHostConfigBackup(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !hostConfigJSONEqual(cfg, decoded.Hosts[0]) {
		t.Fatal("export changed persisted configuration, disabled images, credentials or empty DNS")
	}
	destination := backupTestService(t)
	preview, err := destination.PreviewHostConfigRestore(decoded)
	if err != nil || len(preview) != 1 || preview[0].Action != "create" {
		t.Fatalf("unexpected preview: %v, %v", preview, err)
	}
	if existing, _ := destination.GetHost(cfg.ServerName); existing != nil {
		t.Fatal("preview wrote configuration")
	}
	if err := destination.RestoreHostConfigs(decoded, false); err != nil {
		t.Fatal(err)
	}
	restored, err := destination.GetHost(cfg.ServerName)
	if err != nil || !hostConfigJSONEqual(cfg, restored) {
		t.Fatal("configuration did not round trip")
	}
}

func TestHostConfigRestoreClearsValuesAndPreservesVMs(t *testing.T) {
	s := backupTestService(t)
	old := backupTestConfig("existing")
	old.SystemMaps = []model.OSConfig{{SysName: "linux", SysFlag: true}}
	old.ExtendData["token"] = "old-token"
	old.ServerPlan["old"] = model.NewVMConfig()
	if err := s.RestoreHostConfigs(&HostConfigBackup{Hosts: []*model.HSConfig{old}}, false); err != nil {
		t.Fatal(err)
	}
	vm := model.NewVMConfig()
	vm.VmUUID = "test-vm"
	if err := s.db.UpsertVMConfig(old.ServerName, "test-vm", vm); err != nil {
		t.Fatal(err)
	}
	before, err := s.db.GetVMSaving(old.ServerName)
	if err != nil {
		t.Fatal(err)
	}
	s.drivers[old.ServerName] = driver.NewDriver(old)
	s.statusCache = map[string]*hostStatusCacheEntry{old.ServerName: {cachedAt: time.Now()}}
	updated := backupTestConfig(old.ServerName)
	updated.ServerPass = ""
	updated.IPAddrDDNS = []string{}
	if err := s.RestoreHostConfigs(&HostConfigBackup{Hosts: []*model.HSConfig{updated}}, true); err != nil {
		t.Fatal(err)
	}
	actual, err := s.GetHost(old.ServerName)
	if err != nil || !reflect.DeepEqual(actual, updated) {
		t.Fatal("empty values were not restored exactly")
	}
	after, err := s.db.GetVMSaving(old.ServerName)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("restore modified VM records")
	}
	if s.getDriver(old.ServerName) != nil || s.statusCache[old.ServerName] != nil {
		t.Fatal("disabled restore retained a driver or stale status")
	}
}

func TestHostConfigRestoreAtomicConflict(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "unconfirmed", true: "wrong-type"}[overwrite], func(t *testing.T) {
			s := backupTestService(t)
			old := backupTestConfig("existing")
			if err := s.RestoreHostConfigs(&HostConfigBackup{Hosts: []*model.HSConfig{old}}, false); err != nil {
				t.Fatal(err)
			}
			conflict := backupTestConfig(old.ServerName)
			if overwrite {
				conflict.ServerType = "VMWareSetup"
			}
			backup := &HostConfigBackup{Hosts: []*model.HSConfig{backupTestConfig("new-host"), conflict}}
			if err := s.RestoreHostConfigs(backup, overwrite); err == nil {
				t.Fatal("restore accepted conflicting configuration")
			}
			if cfg, _ := s.GetHost("new-host"); cfg != nil {
				t.Fatal("batch restore was partially committed")
			}
			actual, err := s.GetHost(old.ServerName)
			if err != nil || !reflect.DeepEqual(actual, old) {
				t.Fatal("existing configuration changed on failure")
			}
		})
	}
}

func TestDecodeHostConfigBackupValidation(t *testing.T) {
	valid := backupTestFile(t, backupTestConfig("valid"))
	cases := map[string]func(map[string]interface{}){
		"version":   func(m map[string]interface{}) { m["version"] = 2 },
		"format":    func(m map[string]interface{}) { m["format"] = "other" },
		"empty":     func(m map[string]interface{}) { m["hosts"] = []interface{}{} },
		"null-host": func(m map[string]interface{}) { m["hosts"] = []interface{}{nil} },
		"duplicate": func(m map[string]interface{}) {
			m["hosts"] = append(m["hosts"].([]interface{}), m["hosts"].([]interface{})[0])
		},
		"partial": func(m map[string]interface{}) {
			delete(m["hosts"].([]interface{})[0].(map[string]interface{}), "server_pass")
		},
		"null-field": func(m map[string]interface{}) {
			m["hosts"].([]interface{})[0].(map[string]interface{})["server_plan"] = nil
		},
		"wrong-field-type": func(m map[string]interface{}) {
			m["hosts"].([]interface{})[0].(map[string]interface{})["system_maps"] = "invalid"
		},
		"unknown-field": func(m map[string]interface{}) {
			m["hosts"].([]interface{})[0].(map[string]interface{})["vm_saving"] = map[string]interface{}{}
		},
		"unknown-engine": func(m map[string]interface{}) {
			m["hosts"].([]interface{})[0].(map[string]interface{})["server_type"] = "Unknown"
		},
		"invalid-port": func(m map[string]interface{}) {
			m["hosts"].([]interface{})[0].(map[string]interface{})["server_port"] = 65536
		},
		"negative-price": func(m map[string]interface{}) {
			m["hosts"].([]interface{})[0].(map[string]interface{})["n_cpu_price"] = -1
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			var file map[string]interface{}
			if err := json.Unmarshal(valid, &file); err != nil {
				t.Fatal(err)
			}
			mutate(file)
			raw, _ := json.Marshal(file)
			if _, err := DecodeHostConfigBackup(raw); err == nil {
				t.Fatal("accepted invalid backup")
			}
		})
	}
	if _, err := DecodeHostConfigBackup(append(valid, []byte("{}")...)); err == nil {
		t.Fatal("accepted trailing JSON")
	}
	if _, err := DecodeHostConfigBackup(make([]byte, MaxHostConfigBackupSize+1)); err == nil {
		t.Fatal("accepted oversized backup")
	}
}
