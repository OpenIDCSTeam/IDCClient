// Package service 主机间自动同步服务
// 对应 Python 版 HostManage._check_and_execute_sync / _sync_pull / _sync_push
package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
)

// SyncService 主机间自动同步服务
type SyncService struct {
	db      *data.DB
	hostSvc *HostService
	client  *http.Client

	mu       sync.Mutex
	lastSync time.Time
}

// NewSyncService 创建同步服务
func NewSyncService(db *data.DB, hostSvc *HostService) *SyncService {
	return &SyncService{
		db:      db,
		hostSvc: hostSvc,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

// getSettings 读取系统设置（字符串化）
func (s *SyncService) getSettings() map[string]string {
	result := make(map[string]string)
	raw, err := s.db.GetGlobal("system_settings")
	if err != nil || raw == "" {
		return result
	}
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return result
	}
	for k, v := range m {
		switch val := v.(type) {
		case string:
			result[k] = val
		case float64:
			result[k] = strconv.Itoa(int(val))
		case bool:
			if val {
				result[k] = "1"
			} else {
				result[k] = "0"
			}
		}
	}
	return result
}

// CheckAndExecute 根据系统设置与同步间隔，判断并执行一次同步
func (s *SyncService) CheckAndExecute() error {
	settings := s.getSettings()

	if settings["sync_enabled"] != "1" {
		return nil
	}

	baseURL := strings.TrimRight(settings["sync_url"], "/")
	token := strings.TrimSpace(settings["sync_token"])
	if baseURL == "" || token == "" {
		return nil
	}

	interval := 10 // 默认 10 分钟
	if v, ok := settings["sync_interval"]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			interval = n
		}
	}

	mode := settings["sync_mode"]
	if mode == "" {
		mode = "pull"
	}

	// 间隔判断（并发保护）
	s.mu.Lock()
	if !s.lastSync.IsZero() && time.Since(s.lastSync) < time.Duration(interval)*time.Minute {
		s.mu.Unlock()
		return nil
	}
	s.lastSync = time.Now()
	s.mu.Unlock()

	log.Printf("[sync] 开始执行自动同步，模式: %s", mode)

	if mode == "pull" || mode == "both" {
		if err := s.Pull(baseURL, token); err != nil {
			return fmt.Errorf("拉取同步失败: %w", err)
		}
	}
	if mode == "push" || mode == "both" {
		if err := s.Push(baseURL, token); err != nil {
			return fmt.Errorf("推送同步失败: %w", err)
		}
	}

	log.Printf("[sync] 自动同步执行完成")
	return nil
}

// Pull 拉取同步：从远端拉取同名主机下的虚拟机信息并合并到本地
func (s *SyncService) Pull(baseURL, token string) error {
	remoteHosts, err := s.fetchRemoteHosts(baseURL, token)
	if err != nil {
		return err
	}

	hosts, err := s.db.GetAllHosts()
	if err != nil {
		return err
	}

	for _, h := range hosts {
		if _, ok := remoteHosts[h.HsName]; !ok {
			continue
		}

		remoteVMs, err := s.fetchRemoteVMs(baseURL, token, h.HsName)
		if err != nil {
			log.Printf("[sync-pull] 获取主机 %s 远端虚拟机失败: %v", h.HsName, err)
			continue
		}
		if len(remoteVMs) == 0 {
			continue
		}

		n, err := s.hostSvc.MergeRemoteVMs(h.HsName, remoteVMs)
		if err != nil {
			log.Printf("[sync-pull] 合并主机 %s 虚拟机失败: %v", h.HsName, err)
			continue
		}
		if n > 0 {
			log.Printf("[sync-pull] 主机 %s 同步了 %d 台虚拟机", h.HsName, n)
		}
	}
	return nil
}

// Push 推送同步：将本地同名主机下的虚拟机信息推送到远端
func (s *SyncService) Push(baseURL, token string) error {
	remoteHosts, err := s.fetchRemoteHosts(baseURL, token)
	if err != nil {
		return err
	}

	hosts, err := s.db.GetAllHosts()
	if err != nil {
		return err
	}

	for _, h := range hosts {
		if _, ok := remoteHosts[h.HsName]; !ok {
			continue
		}

		vms, err := s.db.GetVMSaving(h.HsName)
		if err != nil || len(vms) == 0 {
			continue
		}

		pushData := make(map[string]interface{}, len(vms))
		for vmUUID, vmJSON := range vms {
			var m map[string]interface{}
			if err := json.Unmarshal([]byte(vmJSON), &m); err != nil {
				continue
			}
			pushData[vmUUID] = m
		}
		if len(pushData) == 0 {
			continue
		}

		if err := s.postJSON(baseURL+"/api/sync/push/"+h.HsName, token,
			map[string]interface{}{"vms": pushData}); err != nil {
			log.Printf("[sync-push] 推送主机 %s 失败: %v", h.HsName, err)
			continue
		}
		log.Printf("[sync-push] 主机 %s 推送了 %d 台虚拟机", h.HsName, len(pushData))
	}
	return nil
}

// ============================================================
// HTTP 辅助
// ============================================================

// fetchRemoteHosts 获取远端主机列表（返回以主机名为 key 的 map）
func (s *SyncService) fetchRemoteHosts(baseURL, token string) (map[string]interface{}, error) {
	var resp struct {
		Code int                    `json:"code"`
		Msg  string                 `json:"msg"`
		Data map[string]interface{} `json:"data"`
	}
	if err := s.doGet(baseURL+"/api/server/detail", token, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 200 {
		return nil, fmt.Errorf("获取远程主机列表失败: %s", resp.Msg)
	}
	if resp.Data == nil {
		return map[string]interface{}{}, nil
	}
	return resp.Data, nil
}

// fetchRemoteVMs 获取远端指定主机的虚拟机列表（解包 {config: {...}} 形式）
func (s *SyncService) fetchRemoteVMs(baseURL, token, hsName string) (map[string]map[string]interface{}, error) {
	var resp struct {
		Code int                                `json:"code"`
		Msg  string                             `json:"msg"`
		Data map[string]map[string]interface{} `json:"data"`
	}
	if err := s.doGet(baseURL+"/api/client/detail/"+hsName, token, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 200 {
		return nil, fmt.Errorf("获取远程虚拟机列表失败: %s", resp.Msg)
	}

	result := make(map[string]map[string]interface{}, len(resp.Data))
	for uuid, vmInfo := range resp.Data {
		if cfg, ok := vmInfo["config"].(map[string]interface{}); ok {
			result[uuid] = cfg
		} else {
			result[uuid] = vmInfo
		}
	}
	return result, nil
}

// doGet 发送带 Bearer 认证的 GET 请求并解析 JSON
func (s *SyncService) doGet(url, token string, out interface{}) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return json.Unmarshal(body, out)
}

// postJSON 发送带 Bearer 认证的 POST JSON 请求
func (s *SyncService) postJSON(url, token string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
