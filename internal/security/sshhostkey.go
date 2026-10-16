// Package security SSH 主机密钥校验
//
// 控制台 SSH 代理此前无条件使用 ssh.InsecureIgnoreHostKey()，
// 无法识别中间人替换目标主机的情况。这里提供已知主机密钥存储与
// 三种策略，默认使用 TOFU（首次信任），既能防中间人，又不需要
// 事先分发 known_hosts 文件。
package security

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// SSHHostKeyPolicy 主机密钥校验策略
type SSHHostKeyPolicy string

const (
	// SSHHostKeyStrict 严格模式：只接受已知且匹配的主机密钥
	SSHHostKeyStrict SSHHostKeyPolicy = "strict"
	// SSHHostKeyTOFU 首次信任：未知主机记录密钥并接受，已知主机必须匹配
	SSHHostKeyTOFU SSHHostKeyPolicy = "tofu"
	// SSHHostKeyInsecure 不校验（仅建议在完全可信的内网中临时使用）
	SSHHostKeyInsecure SSHHostKeyPolicy = "insecure"
)

// 已知主机密钥在数据库全局配置中的键名
const knownHostsGlobalKey = "ssh_known_hosts"

// CurrentSSHHostKeyPolicy 从环境变量读取策略，默认 TOFU。
// OPENIDCS_SSH_HOSTKEY = strict | tofu | insecure
func CurrentSSHHostKeyPolicy() SSHHostKeyPolicy {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OPENIDCS_SSH_HOSTKEY"))) {
	case string(SSHHostKeyStrict):
		return SSHHostKeyStrict
	case string(SSHHostKeyInsecure):
		return SSHHostKeyInsecure
	default:
		return SSHHostKeyTOFU
	}
}

// HostKeyStore 已知主机密钥存储。
// 密钥以 SHA256 指纹形式保存在数据库全局配置中，避免额外文件依赖。
type HostKeyStore struct {
	mu   sync.Mutex
	keys map[string]string
	load func() (string, error)
	save func(string) error
}

// NewHostKeyStore 创建主机密钥存储。
// load/save 用于读写持久化内容（通常是数据库全局配置）。
func NewHostKeyStore(load func() (string, error), save func(string) error) *HostKeyStore {
	s := &HostKeyStore{
		keys: make(map[string]string),
		load: load,
		save: save,
	}
	s.reload()
	return s
}

// reload 从持久化内容加载指纹
func (s *HostKeyStore) reload() {
	if s.load == nil {
		return
	}
	raw, err := s.load()
	if err != nil || strings.TrimSpace(raw) == "" {
		return
	}
	keys := make(map[string]string)
	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		log.Printf("[ssh] 解析已知主机密钥失败: %v", err)
		return
	}
	s.mu.Lock()
	s.keys = keys
	s.mu.Unlock()
}

// persist 写回持久化内容（调用方需持有锁或自行保证串行）
func (s *HostKeyStore) persist() {
	if s.save == nil {
		return
	}
	data, err := json.Marshal(s.keys)
	if err != nil {
		log.Printf("[ssh] 序列化已知主机密钥失败: %v", err)
		return
	}
	if err := s.save(string(data)); err != nil {
		log.Printf("[ssh] 保存已知主机密钥失败: %v", err)
	}
}

// Lookup 查询指定主机已记录的指纹
func (s *HostKeyStore) Lookup(addr string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fp, ok := s.keys[addr]
	return fp, ok
}

// remember 记录主机指纹
func (s *HostKeyStore) remember(addr, fingerprint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keys == nil {
		s.keys = make(map[string]string)
	}
	s.keys[addr] = fingerprint
	s.persist()
}

// Forget 移除某主机的记录（主机重建后需要重新信任）
func (s *HostKeyStore) Forget(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.keys, addr)
	s.persist()
}

// Callback 返回指定策略下的 ssh.HostKeyCallback
func (s *HostKeyStore) Callback(policy SSHHostKeyPolicy) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		// 统一地址形式：优先使用握手时的 host:port，回退到远端地址
		addr := hostname
		if addr == "" && remote != nil {
			addr = remote.String()
		}

		if policy == SSHHostKeyInsecure {
			return nil
		}

		fingerprint := ssh.FingerprintSHA256(key)

		if known, ok := s.Lookup(addr); ok {
			if known == fingerprint {
				return nil
			}
			// 已记录但指纹不一致：可能是主机重建，也可能是中间人
			return fmt.Errorf("主机 %s 的密钥与已知记录不一致（已知 %s，实际 %s）；"+
				"若确认主机已被重建，请清除该主机的已知密钥后重试", addr, known, fingerprint)
		}

		if policy == SSHHostKeyStrict {
			return fmt.Errorf("主机 %s 不在已知主机列表中（当前策略为 strict）", addr)
		}

		// TOFU：首次连接记录并信任
		log.Printf("[ssh] 首次连接 %s，已记录主机密钥 %s", addr, fingerprint)
		s.remember(addr, fingerprint)
		return nil
	}
}
