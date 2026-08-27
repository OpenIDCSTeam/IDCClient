// Package model User模型
package model

import "time"

// User 用户模型（替代 map[string]interface{} 以避免类型断言panic）
type User struct {
	ID            int       `json:"id"`
	Username      string    `json:"username"`
	Password      string    `json:"password"`
	Email         string    `json:"email"`
	IsAdmin       bool      `json:"is_admin"`
	IsActive      bool      `json:"is_active"`
	EmailVerified bool      `json:"email_verified"`
	VerifyToken   string    `json:"verify_token"`
	ResetToken    string    `json:"reset_token"`
	
	// 权限
	CanCreateVM bool `json:"can_create_vm"`
	CanDeleteVM bool `json:"can_delete_vm"`
	CanModifyVM bool `json:"can_modify_vm"`
	
	// 配额
	QuotaCPU          int `json:"quota_cpu"`
	QuotaRAM          int `json:"quota_ram"`
	QuotaSSD          int `json:"quota_ssd"`
	QuotaGPU          int `json:"quota_gpu"`
	QuotaNatPorts     int `json:"quota_nat_ports"`
	QuotaWebProxy     int `json:"quota_web_proxy"`
	QuotaNatIPs       int `json:"quota_nat_ips"`
	QuotaPubIPs       int `json:"quota_pub_ips"`
	QuotaBandwidthUp  int `json:"quota_bandwidth_up"`
	QuotaBandwidthDown int `json:"quota_bandwidth_down"`
	QuotaTraffic      int `json:"quota_traffic"`
	
	// 已用资源
	UsedCPU          int `json:"used_cpu"`
	UsedRAM          int `json:"used_ram"`
	UsedSSD          int `json:"used_ssd"`
	UsedGPU          int `json:"used_gpu"`
	UsedNatPorts     int `json:"used_nat_ports"`
	UsedWebProxy     int `json:"used_web_proxy"`
	UsedNatIPs       int `json:"used_nat_ips"`
	UsedPubIPs       int `json:"used_pub_ips"`
	UsedBandwidthUp  int `json:"used_bandwidth_up"`
	UsedBandwidthDown int `json:"used_bandwidth_down"`
	UsedTraffic      int `json:"used_traffic"`
	
	AssignedHosts []string  `json:"assigned_hosts"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	LastLogin     time.Time `json:"last_login,omitempty"`
}

// ToMap 转换为 map (兼容旧代码)
func (u *User) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"id": u.ID, "username": u.Username, "password": u.Password, "email": u.Email,
		"is_admin": u.IsAdmin, "is_active": u.IsActive, "email_verified": u.EmailVerified,
		"verify_token": u.VerifyToken, "reset_token": u.ResetToken,
		"can_create_vm": u.CanCreateVM, "can_delete_vm": u.CanDeleteVM, "can_modify_vm": u.CanModifyVM,
		"quota_cpu": u.QuotaCPU, "quota_ram": u.QuotaRAM, "quota_ssd": u.QuotaSSD, "quota_gpu": u.QuotaGPU,
		"quota_nat_ports": u.QuotaNatPorts, "quota_web_proxy": u.QuotaWebProxy,
		"quota_nat_ips": u.QuotaNatIPs, "quota_pub_ips": u.QuotaPubIPs,
		"quota_bandwidth_up": u.QuotaBandwidthUp, "quota_bandwidth_down": u.QuotaBandwidthDown, "quota_traffic": u.QuotaTraffic,
		"used_cpu": u.UsedCPU, "used_ram": u.UsedRAM, "used_ssd": u.UsedSSD, "used_gpu": u.UsedGPU,
		"used_nat_ports": u.UsedNatPorts, "used_web_proxy": u.UsedWebProxy,
		"used_nat_ips": u.UsedNatIPs, "used_pub_ips": u.UsedPubIPs,
		"used_bandwidth_up": u.UsedBandwidthUp, "used_bandwidth_down": u.UsedBandwidthDown, "used_traffic": u.UsedTraffic,
		"assigned_hosts": u.AssignedHosts, "created_at": u.CreatedAt, "updated_at": u.UpdatedAt, "last_login": u.LastLogin,
	}
}

// SafeGetString 安全获取map中的字符串值
func SafeGetString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// SafeGetInt 安全获取map中的int值
func SafeGetInt(m map[string]interface{}, key string) int {
	if v, ok := m[key]; ok {
		if i, ok := v.(int); ok {
			return i
		}
		// 尝试float64转int (JSON unmarshal默认数字类型)
		if f, ok := v.(float64); ok {
			return int(f)
		}
	}
	return 0
}

// SafeGetBool 安全获取map中的bool值
func SafeGetBool(m map[string]interface{}, key string) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
		// 尝试int转bool (数据库可能存储为0/1)
		if i, ok := v.(int); ok {
			return i != 0
		}
	}
	return false
}

// SafeGetStringSlice 安全获取map中的字符串切片
func SafeGetStringSlice(m map[string]interface{}, key string) []string {
	if v, ok := m[key]; ok {
		// 直接是[]string
		if s, ok := v.([]string); ok {
			return s
		}
		// 可能是[]interface{}
		if arr, ok := v.([]interface{}); ok {
			result := make([]string, 0, len(arr))
			for _, item := range arr {
				if s, ok := item.(string); ok {
					result = append(result, s)
				}
			}
			return result
		}
	}
	return []string{}
}
