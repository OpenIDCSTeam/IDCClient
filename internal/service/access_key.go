// Package service 虚拟机访问密钥服务
//
// 访问密钥是一串 16 位「大小写字母 + 数字」的长期凭据，绑定到单台虚拟机：
//  1. 可作为登录凭据在登录页直接登录，登录后进入该虚拟机的管理页面；
//  2. 可通过 URL 携带（/api/client/keylogin?key=xxx）直接跳转到管理页面；
//  3. 登录后的会话权限与「临时凭据」（/api/client/temptoken）完全一致 ——
//     两者使用同一个虚拟用户与同一个权限掩码（VirtualUserMask，35287）。
package service

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// AccessKeyLength 访问密钥长度（固定 16 位）
const AccessKeyLength = 16

// accessKeyAlphabet 密钥字符集：大小写字母 + 数字（62 个字符）
const accessKeyAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// maxAccessKeyGenerateAttempts 生成密钥时的最大重试次数（用于极小概率的随机碰撞）
const maxAccessKeyGenerateAttempts = 8

// GenerateAccessKey 生成 16 位随机访问密钥（密码学安全随机源）
func GenerateAccessKey() (string, error) {
	alphabetLen := big.NewInt(int64(len(accessKeyAlphabet)))
	buf := make([]byte, AccessKeyLength)
	for i := 0; i < AccessKeyLength; i++ {
		idx, err := rand.Int(rand.Reader, alphabetLen)
		if err != nil {
			return "", fmt.Errorf("生成访问密钥失败: %w", err)
		}
		buf[i] = accessKeyAlphabet[idx.Int64()]
	}
	return string(buf), nil
}

// IsValidAccessKey 校验访问密钥格式：必须是 16 位大小写字母或数字
func IsValidAccessKey(key string) bool {
	if len(key) != AccessKeyLength {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// AccessKeyService 虚拟机访问密钥服务
type AccessKeyService struct {
	db *data.DB
}

// NewAccessKeyService 创建访问密钥服务
func NewAccessKeyService(db *data.DB) *AccessKeyService {
	return &AccessKeyService{db: db}
}

// Get 查询虚拟机当前访问密钥；第二个返回值表示「是否已启用」
func (s *AccessKeyService) Get(hsName, vmUUID string) (string, bool, error) {
	row, err := s.db.GetVMAccessKeyByVM(hsName, vmUUID)
	if err != nil {
		return "", false, err
	}
	if row == nil || row.AccessKey == "" {
		return "", false, nil
	}
	return row.AccessKey, true, nil
}

// Ensure 返回虚拟机当前访问密钥，不存在时生成一个并持久化。
func (s *AccessKeyService) Ensure(hsName, vmUUID, createdBy string) (string, error) {
	if key, ok, err := s.Get(hsName, vmUUID); err != nil {
		return "", err
	} else if ok {
		return key, nil
	}
	return s.Reset(hsName, vmUUID, createdBy)
}

// Reset 重新生成访问密钥（旧密钥立即失效），返回新密钥。
func (s *AccessKeyService) Reset(hsName, vmUUID, createdBy string) (string, error) {
	if hsName == "" || vmUUID == "" {
		return "", fmt.Errorf("主机或虚拟机标识不能为空")
	}
	for attempt := 0; attempt < maxAccessKeyGenerateAttempts; attempt++ {
		key, err := GenerateAccessKey()
		if err != nil {
			return "", err
		}
		// 唯一约束冲突（极小概率）时重试，避免把错误抛给调用方
		existing, err := s.db.GetVMAccessKeyByKey(key)
		if err != nil {
			return "", err
		}
		if existing != nil {
			continue
		}
		if err := s.db.UpsertVMAccessKey(hsName, vmUUID, key, createdBy); err != nil {
			// 并发写入时可能撞唯一约束，继续重试
			continue
		}
		return key, nil
	}
	return "", fmt.Errorf("生成访问密钥失败，请重试")
}

// Revoke 撤销虚拟机的访问密钥，返回是否确实删除了记录。
func (s *AccessKeyService) Revoke(hsName, vmUUID string) (bool, error) {
	return s.db.DeleteVMAccessKey(hsName, vmUUID)
}

// Resolve 按密钥反查所属虚拟机，未命中返回 ok=false。
func (s *AccessKeyService) Resolve(accessKey string) (hsName, vmUUID string, ok bool) {
	if !IsValidAccessKey(accessKey) {
		return "", "", false
	}
	row, err := s.db.GetVMAccessKeyByKey(accessKey)
	if err != nil || row == nil {
		return "", "", false
	}
	return row.HsName, row.VmUUID, true
}

// ============================================================
// 虚拟用户（临时凭据 / 访问密钥登录身份）
// ============================================================

// VMTempVirtualUser 返回虚拟机对应的虚拟用户名（{hs_name}-{vm_uuid}）。
// 与 Python 版 get_temp_token 的命名保持一致，前端据此读取 own_all 中的权限掩码。
func VMTempVirtualUser(hsName, vmUUID string) string {
	return hsName + "-" + vmUUID
}

// EnsureVMTempVirtualUser 确保虚拟机配置的 own_all 中存在虚拟用户，
// 并返回虚拟用户名。
//
// 临时凭据登录与访问密钥登录都以该虚拟用户身份建立会话，
// 前端的 user_permissions 直接来自 own_all[虚拟用户]，因此必须保证该条目存在；
// 历史数据中若残留 NIC_EDITS 权限（旧版掩码 35295），此处会按新掩码剔除。
func (s *HostService) EnsureVMTempVirtualUser(hsName, vmUUID string) (string, error) {
	virtualUser := VMTempVirtualUser(hsName, vmUUID)

	d := s.GetOrCreateDriver(hsName)
	if d == nil {
		return "", fmt.Errorf("主机不存在或未启用")
	}
	vm, err := d.GetVMDetail(vmUUID)
	if err != nil || vm == nil {
		return "", fmt.Errorf("虚拟机不存在")
	}
	if vm.OwnAll == nil {
		vm.OwnAll = make(map[string]model.UserMask)
	}

	changed := false
	if mask, ok := vm.OwnAll[virtualUser]; !ok {
		vm.OwnAll[virtualUser] = model.VirtualUserMask()
		changed = true
	} else if mask.NicEdits {
		mask.NicEdits = false
		vm.OwnAll[virtualUser] = mask
		changed = true
	}
	if !changed {
		return virtualUser, nil
	}

	_, _ = d.UpdateVM(vm, vm)
	if err := s.db.UpsertVMConfig(hsName, vmUUID, vm); err != nil {
		// 落库失败不阻断登录：驱动内存中已生效，仅重启后需要重新补齐
		return virtualUser, nil
	}
	return virtualUser, nil
}
