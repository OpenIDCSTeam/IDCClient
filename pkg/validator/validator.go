// Package validator 输入验证工具
package validator

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// 安全的字符白名单
var (
	// AlphaNumeric 字母数字
	AlphaNumeric = regexp.MustCompile(`^[a-zA-Z0-9]+$`)
	// AlphaNumericDash 字母数字加横线下划线
	AlphaNumericDash = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	// AlphaNumericDashDot 字母数字加横线下划线点
	AlphaNumericDashDot = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
	// UUID格式
	UUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// SafePath 安全路径（不含路径穿越）
	SafePath = regexp.MustCompile(`^[a-zA-Z0-9_./:-]+$`)
)

// 危险字符/命令模式（命令注入检测）
var dangerousPatterns = []string{
	";", "|", "&", "$", "`", ">", "<", "\\n", "\\r",
	"$(", "${", "||", "&&", "../", "..\\",
}

// ValidateUsername 验证用户名（字母数字下划线横线，3-32字符）
func ValidateUsername(username string) error {
	if len(username) < 3 || len(username) > 32 {
		return fmt.Errorf("用户名长度必须在3-32字符之间")
	}
	if !AlphaNumericDash.MatchString(username) {
		return fmt.Errorf("用户名只能包含字母、数字、下划线和横线")
	}
	return nil
}

// ValidateEmail 验证邮箱格式
func ValidateEmail(email string) error {
	if len(email) < 5 || len(email) > 100 {
		return fmt.Errorf("邮箱长度必须在5-100字符之间")
	}
	emailPattern := regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	if !emailPattern.MatchString(email) {
		return fmt.Errorf("邮箱格式不正确")
	}
	return nil
}

// ValidatePassword 验证密码强度
func ValidatePassword(password string) error {
	if len(password) < 8 {
		return fmt.Errorf("密码长度至少8字符")
	}
	if len(password) > 128 {
		return fmt.Errorf("密码长度不能超过128字符")
	}
	// 检查是否包含大小写字母和数字
	hasLower := regexp.MustCompile(`[a-z]`).MatchString(password)
	hasUpper := regexp.MustCompile(`[A-Z]`).MatchString(password)
	hasDigit := regexp.MustCompile(`[0-9]`).MatchString(password)
	
	if !hasLower || !hasUpper || !hasDigit {
		return fmt.Errorf("密码必须包含大小写字母和数字")
	}
	return nil
}

// ValidateHostname 验证主机名（字母数字横线点，3-64字符）
func ValidateHostname(hostname string) error {
	if len(hostname) < 3 || len(hostname) > 64 {
		return fmt.Errorf("主机名长度必须在3-64字符之间")
	}
	if !AlphaNumericDashDot.MatchString(hostname) {
		return fmt.Errorf("主机名只能包含字母、数字、横线和点")
	}
	return nil
}

// ValidateIPAddress 验证IP地址
func ValidateIPAddress(ip string) error {
	if net.ParseIP(ip) == nil {
		return fmt.Errorf("无效的IP地址: %s", ip)
	}
	return nil
}

// ValidatePort 验证端口号（1-65535）
func ValidatePort(port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("端口号必须在1-65535之间: %d", port)
	}
	return nil
}

// ValidateUUID 验证UUID格式
func ValidateUUID(uuid string) error {
	if !UUIDPattern.MatchString(strings.ToLower(uuid)) {
		return fmt.Errorf("无效的UUID格式: %s", uuid)
	}
	return nil
}

// ValidatePath 验证路径（防止路径穿越）
func ValidatePath(path string) error {
	if strings.Contains(path, "..") {
		return fmt.Errorf("路径包含非法字符: ..")
	}
	if !SafePath.MatchString(path) {
		return fmt.Errorf("路径包含非法字符")
	}
	return nil
}

// SanitizeString 清理字符串（移除危险字符）
func SanitizeString(input string) string {
	for _, pattern := range dangerousPatterns {
		input = strings.ReplaceAll(input, pattern, "")
	}
	return input
}

// CheckCommandInjection 检测命令注入风险
func CheckCommandInjection(input string) error {
	for _, pattern := range dangerousPatterns {
		if strings.Contains(input, pattern) {
			return fmt.Errorf("输入包含潜在危险字符: %s", pattern)
		}
	}
	return nil
}

// ValidateVMName 验证虚拟机名称
func ValidateVMName(name string) error {
	if len(name) < 2 || len(name) > 64 {
		return fmt.Errorf("虚拟机名称长度必须在2-64字符之间")
	}
	if !AlphaNumericDash.MatchString(name) {
		return fmt.Errorf("虚拟机名称只能包含字母、数字、下划线和横线")
	}
	return nil
}

// ValidateDomain 验证域名格式
func ValidateDomain(domain string) error {
	if len(domain) < 4 || len(domain) > 253 {
		return fmt.Errorf("域名长度必须在4-253字符之间")
	}
	domainPattern := regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`)
	if !domainPattern.MatchString(domain) {
		return fmt.Errorf("无效的域名格式: %s", domain)
	}
	return nil
}

// ValidateCPUCount 验证CPU核心数（1-128）
func ValidateCPUCount(cpu int) error {
	if cpu < 1 || cpu > 128 {
		return fmt.Errorf("CPU核心数必须在1-128之间: %d", cpu)
	}
	return nil
}

// ValidateMemorySize 验证内存大小（单位MB，256-524288）
func ValidateMemorySize(mem int) error {
	if mem < 256 || mem > 524288 {
		return fmt.Errorf("内存大小必须在256MB-512GB之间: %d", mem)
	}
	return nil
}

// ValidateDiskSize 验证磁盘大小（单位MB，1024-10485760）
func ValidateDiskSize(disk int) error {
	if disk < 1024 || disk > 10485760 {
		return fmt.Errorf("磁盘大小必须在1GB-10TB之间: %d", disk)
	}
	return nil
}

// ValidateResourceQuota 验证资源配额
func ValidateResourceQuota(quotaType string, value int) error {
	switch quotaType {
	case "cpu":
		return ValidateCPUCount(value)
	case "memory", "ram":
		return ValidateMemorySize(value)
	case "disk", "ssd":
		return ValidateDiskSize(value)
	case "gpu":
		if value < 0 || value > 16 {
			return fmt.Errorf("GPU数量必须在0-16之间: %d", value)
		}
	case "port":
		if value < 0 || value > 1000 {
			return fmt.Errorf("端口配额必须在0-1000之间: %d", value)
		}
	default:
		return fmt.Errorf("未知的配额类型: %s", quotaType)
	}
	return nil
}

// ValidateBatchInput 批量验证输入（用于批量操作）
func ValidateBatchInput(inputs []string, maxCount int, validator func(string) error) error {
	if len(inputs) == 0 {
		return fmt.Errorf("输入列表不能为空")
	}
	if len(inputs) > maxCount {
		return fmt.Errorf("批量操作数量不能超过%d个", maxCount)
	}
	for i, input := range inputs {
		if err := validator(input); err != nil {
			return fmt.Errorf("第%d项验证失败: %v", i+1, err)
		}
	}
	return nil
}
