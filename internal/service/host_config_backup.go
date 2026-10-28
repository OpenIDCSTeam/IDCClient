package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/driver"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

const HostConfigBackupFormat = "openidcs-host-config"
const HostConfigBackupVersion = 1
const MaxHostConfigBackupSize = 10 * 1024 * 1024
const maxBackupHosts = 1000

type HostConfigBackup struct {
	Format     string            `json:"format"`
	Version    int               `json:"version"`
	ExportedAt string            `json:"exported_at"`
	Hosts      []*model.HSConfig `json:"hosts"`
}

type HostConfigRestoreItem struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Addr       string `json:"addr"`
	EnableHost bool   `json:"enable_host"`
	Action     string `json:"action"`
}

func DecodeHostConfigBackup(raw json.RawMessage) (*HostConfigBackup, error) {
	if len(raw) > MaxHostConfigBackupSize {
		return nil, fmt.Errorf("配置文件不能超过 10 MB")
	}
	var backup HostConfigBackup
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&backup); err != nil {
		return nil, fmt.Errorf("配置文件格式错误")
	}
	if err := decoder.Decode(new(interface{})); err != io.EOF {
		return nil, fmt.Errorf("配置文件包含多余内容")
	}
	if backup.Format != HostConfigBackupFormat || backup.Version != HostConfigBackupVersion {
		return nil, fmt.Errorf("不支持的配置文件格式或版本")
	}
	if len(backup.Hosts) == 0 || len(backup.Hosts) > maxBackupHosts {
		return nil, fmt.Errorf("配置文件须包含 1 至 %d 台主机", maxBackupHosts)
	}

	var envelope struct {
		Hosts []map[string]json.RawMessage `json:"hosts"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("配置文件格式错误")
	}
	defaults, _ := json.Marshal(model.NewHSConfig())
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(defaults, &fields)
	seen := make(map[string]bool, len(backup.Hosts))
	for i, cfg := range backup.Hosts {
		if cfg == nil {
			return nil, fmt.Errorf("第 %d 台主机配置为空", i+1)
		}
		for key := range fields {
			value, exists := envelope.Hosts[i][key]
			if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, fmt.Errorf("第 %d 台主机缺少完整字段 %s，请使用导出的配置文件", i+1, key)
			}
		}
		if err := validateBackupHost(cfg); err != nil {
			return nil, fmt.Errorf("第 %d 台主机: %w", i+1, err)
		}
		if seen[cfg.ServerName] {
			return nil, fmt.Errorf("配置文件中的主机名称重复: %s", cfg.ServerName)
		}
		seen[cfg.ServerName] = true
	}
	return &backup, nil
}

func validateBackupHost(cfg *model.HSConfig) error {
	name := cfg.ServerName
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "/\\") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("主机名称无效")
	}
	meta, known := driver.EngineMetaOf(cfg.ServerType)
	if !known || !meta.Enabled {
		return fmt.Errorf("不支持的主机类型: %s", cfg.ServerType)
	}
	for _, port := range []int{cfg.ServerPort, cfg.RemotePort, cfg.PortsStart, cfg.PortsClose} {
		if port < 0 || port > 65535 {
			return fmt.Errorf("端口须在 0 至 65535 之间")
		}
	}
	if cfg.PortsStart > 0 && cfg.PortsClose > 0 && cfg.PortsStart > cfg.PortsClose {
		return fmt.Errorf("起始端口不能大于结束端口")
	}
	if cfg.LimitsNums < 0 {
		return fmt.Errorf("虚拟机数量限制不能为负数")
	}
	for _, price := range []float64{cfg.NCpuPrice, cfg.NMemPrice, cfg.NHddPrice, cfg.NNetPrice} {
		if price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			return fmt.Errorf("单价须为非负有限数值")
		}
	}
	return nil
}

func (s *HostService) ExportHostConfigs(hsName string) (*HostConfigBackup, error) {
	configs := make([]*model.HSConfig, 0)
	if hsName != "" {
		cfg, err := s.GetHost(hsName)
		if err != nil {
			return nil, err
		}
		if cfg == nil {
			return nil, fmt.Errorf("主机不存在")
		}
		configs = append(configs, cfg)
	} else {
		rows, err := s.db.GetAllHosts()
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			cfg, err := rowToHSConfig(row)
			if err != nil {
				return nil, err
			}
			configs = append(configs, cfg)
		}
	}
	if len(configs) == 0 || len(configs) > maxBackupHosts {
		return nil, fmt.Errorf("可导出的主机数量须在 1 至 %d 之间", maxBackupHosts)
	}
	sort.Slice(configs, func(i, j int) bool { return configs[i].ServerName < configs[j].ServerName })
	backup := &HostConfigBackup{
		Format: HostConfigBackupFormat, Version: HostConfigBackupVersion,
		ExportedAt: time.Now().UTC().Format(time.RFC3339), Hosts: configs,
	}
	raw, err := json.Marshal(backup)
	if err != nil {
		return nil, fmt.Errorf("配置序列化失败")
	}
	if len(raw) > MaxHostConfigBackupSize {
		return nil, fmt.Errorf("配置超过 10 MB，请按单台主机导出")
	}
	return backup, nil
}

func (s *HostService) PreviewHostConfigRestore(backup *HostConfigBackup) ([]HostConfigRestoreItem, error) {
	items := make([]HostConfigRestoreItem, 0, len(backup.Hosts))
	for _, cfg := range backup.Hosts {
		existing, err := s.GetHost(cfg.ServerName)
		if err != nil {
			return nil, err
		}
		action := "create"
		if existing != nil {
			if existing.ServerType != cfg.ServerType {
				return nil, fmt.Errorf("主机 %s 的引擎类型不一致，不能覆盖", cfg.ServerName)
			}
			action = "overwrite"
		}
		items = append(items, HostConfigRestoreItem{
			Name: cfg.ServerName, Type: cfg.ServerType, Addr: cfg.ServerAddr,
			EnableHost: cfg.EnableHost, Action: action,
		})
	}
	return items, nil
}

func (s *HostService) RestoreHostConfigs(backup *HostConfigBackup, overwrite bool) error {
	s.initMu.Lock()
	defer s.initMu.Unlock()
	rows := make([]*data.HSConfigRow, 0, len(backup.Hosts))
	for _, cfg := range backup.Hosts {
		row, err := hsConfigToRow(cfg)
		if err != nil {
			return err
		}
		rows = append(rows, row)
	}
	if err := s.db.RestoreHostConfigs(rows, overwrite); err != nil {
		return err
	}
	for _, cfg := range backup.Hosts {
		s.mu.Lock()
		delete(s.drivers, cfg.ServerName)
		s.mu.Unlock()
		s.statusMu.Lock()
		delete(s.statusCache, cfg.ServerName)
		s.statusMu.Unlock()
		if cfg.EnableHost {
			s.initDriver(cfg)
		}
	}
	return nil
}
