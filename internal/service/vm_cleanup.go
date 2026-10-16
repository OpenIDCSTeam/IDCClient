// Package service VM资源清理服务
package service

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// VMCleanupService VM资源清理服务
type VMCleanupService struct {
	DB       *data.DB
	natSvc   *NATService
	proxySvc *ProxyService
	mu       sync.Mutex // 防止并发清理同一VM
}

// NewVMCleanupService 创建VM清理服务
func NewVMCleanupService(db *data.DB) *VMCleanupService {
	return &VMCleanupService{
		DB: db,
	}
}

// SetNATService 注入NAT下发服务（可选）
func (s *VMCleanupService) SetNATService(svc *NATService) {
	s.natSvc = svc
}

// SetProxyService 注入反向代理下发服务（可选）
func (s *VMCleanupService) SetProxyService(svc *ProxyService) {
	s.proxySvc = svc
}

// CleanupVMResources 清理VM关联的所有资源
// 在删除VM前调用,清理NAT映射、Web代理、状态缓存等
func (s *VMCleanupService) CleanupVMResources(hsName, vmUUID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	
	// 获取VM配置
	vmJSON, err := s.DB.GetVMConfig(hsName, vmUUID)
	if err != nil || vmJSON == "" {
		log.Printf("VM %s 不存在或已删除,跳过资源清理", vmUUID)
		return nil
	}

	// 解析VM配置
	var vm model.VMConfig
	if err := json.Unmarshal([]byte(vmJSON), &vm); err != nil {
		return fmt.Errorf("解析VM配置失败: %w", err)
	}

	var cleanupErrors []string

	// 1. 清理NAT端口映射
	if len(vm.NatAll) > 0 {
		log.Printf("开始清理VM %s 的 %d 条NAT映射", vmUUID, len(vm.NatAll))
		for _, nat := range vm.NatAll {
			if err := s.deleteNATRule(hsName, vmUUID, nat); err != nil {
				errMsg := fmt.Sprintf("清理NAT映射失败 (端口%d): %v", nat.WanPort, err)
				log.Println(errMsg)
				cleanupErrors = append(cleanupErrors, errMsg)
				// 继续清理其他资源,不要因为一个失败而中断
			} else {
				log.Printf("已删除NAT映射: %d -> %s:%d", nat.WanPort, nat.LanAddr, nat.LanPort)
			}
		}
	}

	// 2. 清理Web反向代理
	if len(vm.WebAll) > 0 {
		log.Printf("开始清理VM %s 的 %d 个Web代理", vmUUID, len(vm.WebAll))
		for _, proxy := range vm.WebAll {
			if err := s.deleteWebProxy(hsName, vmUUID, &proxy); err != nil {
				errMsg := fmt.Sprintf("清理Web代理失败 (%s): %v", proxy.WebAddr, err)
				log.Println(errMsg)
				cleanupErrors = append(cleanupErrors, errMsg)
			} else {
				log.Printf("已删除Web代理: %s -> %s:%d", proxy.WebAddr, proxy.LanAddr, proxy.LanPort)
			}
		}
	}

	// 3. 清理虚拟机状态缓存
	if err := s.DB.DeleteVMStatus(hsName, vmUUID); err != nil {
		log.Printf("清理VM状态缓存失败: %v", err)
		cleanupErrors = append(cleanupErrors, fmt.Sprintf("状态缓存清理失败: %v", err))
	}

	// 4. 清理虚拟机配置记录
	if err := s.DB.DeleteVMConfig(hsName, vmUUID); err != nil {
		log.Printf("删除VM配置记录失败: %v", err)
		cleanupErrors = append(cleanupErrors, fmt.Sprintf("配置记录删除失败: %v", err))
	}

	if len(cleanupErrors) > 0 {
		return fmt.Errorf("部分资源清理失败: %v", cleanupErrors)
	}

	log.Printf("VM %s 的所有资源已成功清理", vmUUID)
	return nil
}

// deleteNATRule 删除NAT端口映射（下发路由器 + 删库）
func (s *VMCleanupService) deleteNATRule(hsName, vmUUID string, nat model.PortData) error {
	if s.natSvc != nil {
		if err := s.natSvc.RevokePortMap(hsName, vmUUID, nat); err != nil {
			return fmt.Errorf("下发NAT删除失败: %w", err)
		}
		log.Printf("已下发删除NAT映射: %s:%d -> %d", nat.LanAddr, nat.LanPort, nat.WanPort)
		return nil
	}

	// 未注入NAT服务时仅删除数据库记录
	if err := s.DB.DeleteNATRule(hsName, vmUUID, nat.WanPort); err != nil {
		return fmt.Errorf("从数据库删除NAT记录失败: %w", err)
	}
	return nil
}

// deleteWebProxy 删除Web反向代理（删库 + 重载代理配置）
func (s *VMCleanupService) deleteWebProxy(hsName, vmUUID string, proxy *model.WebProxy) error {
	if err := s.DB.DeleteWebProxy(hsName, vmUUID, proxy.WebAddr); err != nil {
		return fmt.Errorf("从数据库删除Web代理记录失败: %w", err)
	}

	if s.proxySvc != nil {
		if err := s.proxySvc.ApplyProxy(hsName, vmUUID, *proxy, false); err != nil {
			return fmt.Errorf("下发反向代理删除失败: %w", err)
		}
		log.Printf("已删除Web代理并下发: %s -> %s:%d", proxy.WebAddr, proxy.LanAddr, proxy.LanPort)
	}
	return nil
}

// CleanupOrphanedResources 清理孤立资源(定期任务)
// 扫描数据库中的NAT/Proxy记录,删除VM已不存在的映射
func (s *VMCleanupService) CleanupOrphanedResources() (int, error) {
	cleanedCount := 0

	// 获取所有主机
	hosts, err := s.DB.GetAllHosts()
	if err != nil {
		return 0, fmt.Errorf("获取主机列表失败: %w", err)
	}

	for _, host := range hosts {
		// 获取该主机的所有VM
		vms, err := s.DB.GetVMsByHost(host.HsName)
		if err != nil {
			log.Printf("获取主机 %s 的VM列表失败: %v", host.HsName, err)
			continue
		}

		vmUUIDs := make(map[string]bool)
		for _, vmMap := range vms {
			if vmUUID, ok := vmMap["vm_uuid"].(string); ok && vmUUID != "" {
				vmUUIDs[vmUUID] = true
			}
		}

		// 检查NAT规则
		natRules, err := s.DB.GetNATRulesByHost(host.HsName)
		if err != nil {
			log.Printf("获取主机 %s 的NAT规则失败: %v", host.HsName, err)
			continue
		}

		for _, natMap := range natRules {
			vmUUID, _ := natMap["vm_uuid"].(string)
			wanPort, _ := natMap["wan_port"].(int)
			if !vmUUIDs[vmUUID] {
				log.Printf("发现孤立NAT规则: VM=%s, 端口=%d", vmUUID, wanPort)
				if err := s.DB.DeleteNATRule(host.HsName, vmUUID, wanPort); err != nil {
					log.Printf("删除孤立NAT规则失败: %v", err)
				} else {
					cleanedCount++
				}
			}
		}

		// 检查Web代理
		webProxies, err := s.DB.GetWebProxiesByHost(host.HsName)
		if err != nil {
			log.Printf("获取主机 %s 的Web代理失败: %v", host.HsName, err)
			continue
		}

		for _, proxyMap := range webProxies {
			vmUUID, _ := proxyMap["vm_uuid"].(string)
			webDomain, _ := proxyMap["web_domain"].(string)
			webName, _ := proxyMap["web_name"].(string)
			if !vmUUIDs[vmUUID] {
				log.Printf("发现孤立Web代理: VM=%s, 域名=%s", vmUUID, webDomain)
				if err := s.DB.DeleteWebProxy(host.HsName, vmUUID, webName); err != nil {
					log.Printf("删除孤立Web代理失败: %v", err)
				} else {
					cleanedCount++
				}
			}
		}
	}

	log.Printf("孤立资源清理完成,共清理 %d 条记录", cleanedCount)
	return cleanedCount, nil
}
