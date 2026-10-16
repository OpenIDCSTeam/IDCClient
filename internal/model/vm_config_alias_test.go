package model

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSDConfigPythonFieldAlias 校验数据盘配置与 Python 端的字段互通。
//
// 背景：Python 版 SDConfig 使用 hdd_file / hdd_scsi，Go 侧驱动统一使用
// HddPath 定位磁盘镜像。两者必须互为兜底，否则：
//   - Python 写入的 hdd_file 在 Go 侧丢失，重启后驱动拼不出磁盘路径；
//   - Go 写入的 hdd_path 在 Python 侧 hasattr 载入后为空，磁盘"消失"。
func TestSDConfigPythonFieldAlias(t *testing.T) {
	// Python 形态：只有 hdd_file，没有 hdd_path
	pythonJSON := `{"hdd_name":"data1","hdd_size":10240,"hdd_flag":1,` +
		`"hdd_scsi":"scsi1","hdd_file":"vm-100-disk-1.qcow2"}`

	var hdd SDConfig
	if err := json.Unmarshal([]byte(pythonJSON), &hdd); err != nil {
		t.Fatalf("解析 Python 形态失败: %v", err)
	}
	if hdd.HddPath != "vm-100-disk-1.qcow2" {
		t.Fatalf("hdd_file 应兜底到 HddPath: %q", hdd.HddPath)
	}
	if hdd.HddFile != "vm-100-disk-1.qcow2" {
		t.Fatalf("hdd_file 应原样保留: %q", hdd.HddFile)
	}
	if hdd.HddScsi != "scsi1" {
		t.Fatalf("hdd_scsi 应保留: %q", hdd.HddScsi)
	}

	// Go 形态：只有 hdd_path，序列化后必须带出 hdd_file 供 Python 读取
	var goHdd SDConfig
	if err := json.Unmarshal(
		[]byte(`{"hdd_name":"d2","hdd_size":1,"hdd_path":"/data/d2.qcow2"}`),
		&goHdd); err != nil {
		t.Fatalf("解析 Go 形态失败: %v", err)
	}
	out, err := json.Marshal(goHdd)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(out), `"hdd_file":"/data/d2.qcow2"`) {
		t.Fatalf("序列化应回填 hdd_file，实际: %s", out)
	}
	if !strings.Contains(string(out), `"hdd_path":"/data/d2.qcow2"`) {
		t.Fatalf("序列化应保留 hdd_path，实际: %s", out)
	}
}

// TestSDConfigHddType 校验磁盘介质类型可被持久化。
//
// 前端「添加/挂载数据盘」表单会下发 hdd_type（0=HDD，1=SSD）并据此渲染标签，
// 字段缺失会被静默丢弃，表现为用户选择 SSD 后列表仍显示 HDD。
func TestSDConfigHddType(t *testing.T) {
	var hdd SDConfig
	if err := json.Unmarshal([]byte(`{"hdd_name":"d1","hdd_type":1}`), &hdd); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if hdd.HddType != 1 {
		t.Fatalf("hdd_type 应被保留，实际 %d", hdd.HddType)
	}
	out, _ := json.Marshal(hdd)
	if !strings.Contains(string(out), `"hdd_type":1`) {
		t.Fatalf("序列化应包含 hdd_type，实际: %s", out)
	}
}

// TestIMConfigPythonFieldAlias 校验 ISO 配置与 Python 端的字段互通。
func TestIMConfigPythonFieldAlias(t *testing.T) {
	pythonJSON := `{"iso_name":"ubuntu.iso","iso_file":"ubuntu-22.04.iso",` +
		`"iso_hint":"/srv/iso/ubuntu-22.04.iso"}`

	var iso IMConfig
	if err := json.Unmarshal([]byte(pythonJSON), &iso); err != nil {
		t.Fatalf("解析 Python 形态失败: %v", err)
	}
	if iso.IsoPath == "" {
		t.Fatalf("iso_file/iso_hint 应兜底到 IsoPath")
	}
	if iso.IsoHint != "/srv/iso/ubuntu-22.04.iso" {
		t.Fatalf("iso_hint 应原样保留: %q", iso.IsoHint)
	}

	// Go 形态序列化后应带出 iso_file
	out, err := json.Marshal(IMConfig{IsoName: "x.iso", IsoPath: "/data/x.iso"})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if !strings.Contains(string(out), `"iso_file":"/data/x.iso"`) {
		t.Fatalf("序列化应回填 iso_file，实际: %s", out)
	}
}

// TestNCConfigPythonFields 校验网卡配置保留 Python 侧全部字段。
//
// nic_type 决定 IP 分配使用的地址池（nat/pub），nic_gate/nic_mask 决定
// 静态 IP 配置，缺失会让下发的网络配置与平台侧不一致。
func TestNCConfigPythonFields(t *testing.T) {
	raw := `{"nic_name":"eth0","nic_type":"pub","mac_addr":"00:1c:0a:02:01:05",` +
		`"ip4_addr":"10.2.1.105","ip6_addr":"","nic_gate":"10.2.1.1",` +
		`"nic_mask":"255.255.255.0","dns_addr":["8.8.8.8"]}`

	var nic NCConfig
	if err := json.Unmarshal([]byte(raw), &nic); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if nic.NicType != "pub" {
		t.Fatalf("nic_type 应保留: %q", nic.NicType)
	}
	if nic.EffectiveNicType() != "pub" {
		t.Fatalf("EffectiveNicType 应为 pub: %q", nic.EffectiveNicType())
	}
	if nic.NicGate != "10.2.1.1" {
		t.Fatalf("nic_gate 应保留: %q", nic.NicGate)
	}
	if nic.EffectiveMask() != "255.255.255.0" {
		t.Fatalf("EffectiveMask 应保留: %q", nic.EffectiveMask())
	}
	// 缺省值：与 Python NCConfig 一致（nic_type 缺省 nat，掩码缺省 255.255.255.0）
	var empty NCConfig
	if empty.EffectiveNicType() != "nat" {
		t.Fatal("nic_type 缺省应为 nat")
	}
	if empty.EffectiveMask() != "255.255.255.0" {
		t.Fatal("掩码缺省应为 255.255.255.0")
	}
}
