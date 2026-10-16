// Package service Web反向代理下发服务（Caddy）
// 对应 Python 版 HostModule/HttpManager 的 config_all / reload_web 能力。
// 与 Python 的差异：Python 维护内存态 proxys_list，Go 直接从 vm_saving 全量重建配置，
// 避免重启后内存态丢失，配置生成规则保持一致。
package service

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/driver"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/security"
)

// ProxyService 反向代理下发服务
type ProxyService struct {
	db       *data.DB
	resolver func(string) driver.HostDriver
	mu       sync.Mutex
	// publishMu 串行化「重建配置 → 下发」全过程。
	// mu 只保护单次构建，无法阻止两次变更交错：
	// A 读到旧配置、B 先完成下发、A 后完成，会把运行态退回旧版本。
	// 该锁必须在 mu 外层获取，避免与 Rebuild 内部加锁形成自锁。
	publishMu  sync.Mutex
	binaryPath string
	configFile string
	managePort int
	dataDir    string
}

// SetDriverResolver 注入驱动解析器（用于识别平台自有的代理通道，如青州云）
func (s *ProxyService) SetDriverResolver(fn func(string) driver.HostDriver) {
	s.resolver = fn
}

// provider 返回主机驱动实现的平台自有代理通道（若支持）
func (s *ProxyService) provider(hsName string) (driver.NatProxyProvider, bool) {
	if s.resolver == nil {
		return nil, false
	}
	d := s.resolver(hsName)
	if d == nil {
		return nil, false
	}
	p, ok := d.(driver.NatProxyProvider)
	return p, ok
}

// NewProxyService 创建反向代理服务
func NewProxyService(db *data.DB, dataDir string) *ProxyService {
	binary := os.Getenv("CADDY_BIN")
	if binary == "" {
		binary = "caddy"
	}
	return &ProxyService{
		db:         db,
		binaryPath: binary,
		configFile: filepath.Join(dataDir, "Caddyfile"),
		managePort: 2019,
		dataDir:    dataDir,
	}
}

// SetBinaryPath 设置 caddy 可执行文件路径
func (s *ProxyService) SetBinaryPath(path string) {
	if path != "" {
		s.binaryPath = path
	}
}

// ConfigFile 返回配置文件路径
func (s *ProxyService) ConfigFile() string {
	return s.configFile
}

// Rebuild 从数据库全量重建 Caddyfile
func (s *ProxyService) Rebuild() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	hosts, err := s.db.GetAllHosts()
	if err != nil {
		return fmt.Errorf("获取主机列表失败: %w", err)
	}

	// 域名去重（与 Python proxys_list 以 domain 为 key 的行为一致）
	proxies := make(map[string]model.WebProxy)
	for _, h := range hosts {
		// 平台自有代理通道（如青州云）由云端 API 管理，不纳入本地 Caddy 配置
		if _, ok := s.provider(h.HsName); ok {
			continue
		}
		vms, err := s.db.GetVMSaving(h.HsName)
		if err != nil {
			continue
		}
		for _, vmJSON := range vms {
			vm, decodeErr := DecodeVMConfig(vmJSON)
			if decodeErr != nil || vm == nil {
				continue
			}
			// 正在删除（存在未完成墓碑）的虚拟机不参与配置生成：
			// 其本地配置尚未清理时，重建会把已下线的代理重新写回 Caddy。
			if s.db.HasIncompleteTombstone(h.HsName, vm.VmUUID) {
				continue
			}
			for _, web := range vm.WebAll {
				if web.WebAddr == "" {
					continue
				}
				if _, exists := proxies[web.WebAddr]; !exists {
					proxies[web.WebAddr] = web
				}
			}
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("{\n\tadmin localhost:%d\n}\n\n", s.managePort))
	for _, web := range proxies {
		sb.WriteString(buildCaddyBlock(web))
	}

	if err := os.MkdirAll(filepath.Dir(s.configFile), 0755); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	if err := os.WriteFile(s.configFile, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("写入Caddyfile失败: %w", err)
	}
	return nil
}

// Reload 重载 Caddy 配置
// 优先级：
//  1. Admin API 直连（Caddy 进程已运行时可用，无需 caddy 可执行文件）
//  2. caddy reload CLI（未启用 Admin API 或进程由 CLI 管理时）
//  3. caddy start 后台启动（进程尚未运行时）
func (s *ProxyService) Reload() error {
	caddyfile, err := os.ReadFile(s.configFile)
	if err != nil {
		return fmt.Errorf("读取Caddyfile失败: %w", err)
	}

	// 1) Admin API：POST /load?adapter=caddyfile
	if err := s.reloadViaAdminAPI(caddyfile); err == nil {
		return nil
	} else {
		log.Printf("[proxy] Admin API 重载失败，尝试 CLI: %v", err)
	}

	// 2) CLI reload
	args := []string{
		s.binaryPath, "reload",
		"--config", s.configFile,
		"--adapter", "caddyfile",
		"--address", fmt.Sprintf("localhost:%d", s.managePort),
	}
	if out, _, err := security.SafeExecWithTimeout(args, 30*time.Second); err == nil {
		return nil
	} else {
		log.Printf("[proxy] Caddy reload 失败，尝试启动: %v (%s)", err, strings.TrimSpace(out))
	}

	// 3) 回退：后台启动
	startArgs := []string{
		s.binaryPath, "start",
		"--config", s.configFile,
		"--adapter", "caddyfile",
	}
	if out, _, err := security.SafeExecWithTimeout(startArgs, 30*time.Second); err != nil {
		return fmt.Errorf("启动Caddy失败: %v (%s)", err, strings.TrimSpace(out))
	}
	return nil
}

// reloadViaAdminAPI 通过 Caddy Admin API 直接加载 Caddyfile
// Caddy 支持 POST /load 并以 Content-Type + ?adapter 指定配置格式，
// 因此无需引入 caddy Go 模块，也无需本地 caddy 可执行文件。
func (s *ProxyService) reloadViaAdminAPI(caddyfile []byte) error {
	url := fmt.Sprintf("http://localhost:%d/load?adapter=caddyfile", s.managePort)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(caddyfile))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/caddyfile")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// RebuildAndReload 重建配置并重载（串行发布，避免变更交错导致配置回退）。
//
// 新增回滚：原实现在 reload 失败时只返回错误，磁盘上留下的是一份
// 与运行态不一致的新配置；这里先备份上一份可用配置，失败时恢复并重载。
func (s *ProxyService) RebuildAndReload() error {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	backup, backupErr := os.ReadFile(s.configFile)

	if err := s.Rebuild(); err != nil {
		return err
	}

	if err := s.Reload(); err != nil {
		if backupErr == nil {
			if restoreErr := os.WriteFile(s.configFile, backup, 0644); restoreErr != nil {
				log.Printf("[proxy] 回滚 Caddyfile 失败: %v", restoreErr)
			} else if reloadErr := s.Reload(); reloadErr != nil {
				log.Printf("[proxy] 回滚后重新加载失败: %v", reloadErr)
			} else {
				log.Printf("[proxy] 下发失败，已回滚到上一份可用配置")
			}
		}
		return err
	}
	return nil
}

// HasProvider 判断主机是否使用平台自有的代理通道（如青州云）。
// 这类通道由云端 API 管理，删除时需要逐条调用云端接口销账。
func (s *ProxyService) HasProvider(hsName string) bool {
	_, ok := s.provider(hsName)
	return ok
}

// RevokeProxy 撤销一条反向代理。
// 平台自有通道调用云端接口；本地 Caddy 场景触发配置重建。
func (s *ProxyService) RevokeProxy(hsName, vmUUID string, proxy model.WebProxy) error {
	return s.ApplyProxy(hsName, vmUUID, proxy, false)
}

// ApplyProxy 下发反向代理
// 平台自有通道优先（青州云云端 API），否则重建并重载本地 Caddy 配置。
func (s *ProxyService) ApplyProxy(hsName, vmUUID string, proxy model.WebProxy, add bool) error {
	if p, ok := s.provider(hsName); ok {
		msg, err := p.ProxyMap(vmUUID, proxy, add)
		if err != nil {
			return err
		}
		if msg != nil && !msg.Success {
			return fmt.Errorf("%s", msg.Message)
		}
		return nil
	}
	return s.RebuildAndReload()
}

// buildCaddyBlock 生成单个反向代理的 Caddyfile 片段
// 规则与 Python HttpManager.config_all 保持一致
func buildCaddyBlock(web model.WebProxy) string {
	domain := web.WebAddr
	ip := web.LanAddr
	port := web.LanPort
	isHTTPS := web.IsHTTPS

	backendProtocol := "http"
	if isHTTPS {
		backendProtocol = "https"
	}

	// 与 Python 一致：普通场景 listen_port 为空，走默认分支
	url := domain
	if !isHTTPS {
		url = "http://" + domain
	}

	tlsSkip := ""
	if isHTTPS {
		tlsSkip = "\n\t\ttransport http {\n\t\t\ttls_insecure_skip_verify\n\t\t}"
	}

	// 子路径代理
	if idx := strings.Index(domain, "/"); idx >= 0 {
		subPath := domain[idx:]
		return fmt.Sprintf("%s {\n\t@secret path %s\n\treverse_proxy %s://%s:%d\n}\n\n",
			url, subPath, backendProtocol, ip, port)
	}

	return fmt.Sprintf("%s {\n\treverse_proxy %s://%s:%d {%s\n\t}\n}\n\n",
		url, backendProtocol, ip, port, tlsSkip)
}
