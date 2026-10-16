// Package driver 容器类平台（Docker / LXD / SmolVM）的端口映射下发。
//
// 对应 Python 版 BasicServer.PortsMap_TTY（LXContainer / OCInterface / SmolVM
// 三者的 PortsMap 都直接委托给该方法）：
//
//  1. wan_port <= 0 → 在主机端口范围内随机分配一个未占用端口
//  2. wan_port 已指定 → 查询宿主机 iptables 已占用端口，冲突则拒绝
//  3. add=true  → 通过 iptables 下发 DNAT + ACCEPT 规则
//     add=false → 删除对应规则
//
// 与「青州云」这类云端通道不同，容器平台没有云端 NAT API，
// 端口映射落在**宿主机 iptables** 上（nat/filter 的 DOCKER 链）。
package driver

import (
	"fmt"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// portsMapTTY 容器专用端口映射（对齐 Python BasicServer.PortsMap_TTY）
func portsMapTTY(cfg *model.HSConfig, nat *model.PortData, add bool) (*model.ZMessage, error) {
	if cfg == nil {
		return &model.ZMessage{Success: false, Action: "PortsMap", Message: "主机配置为空"}, nil
	}
	if nat == nil || nat.LanPort <= 0 || nat.LanAddr == "" {
		return &model.ZMessage{Success: false, Action: "PortsMap",
			Message: "端口映射参数不完整（需要内网地址与内网端口）"}, nil
	}

	pf := newPortForward(cfg)

	// 端口分配 / 占用校验
	if nat.WanPort <= 0 {
		nat.WanPort = pf.allocatePort()
		if nat.WanPort <= 0 {
			return &model.ZMessage{Success: false, Action: "PortsMap",
				Message: "无法分配可用外网端口"}, nil
		}
	} else if pf.hostPortsFirewall()[nat.WanPort] {
		return &model.ZMessage{Success: false, Action: "PortsMap",
			Message: fmt.Sprintf("端口 %d 已被占用", nat.WanPort)}, nil
	}

	if add {
		if err := pf.addPortForwardFirewall(nat.LanAddr, nat.LanPort, nat.WanPort, nat.NatTips); err != nil {
			return &model.ZMessage{Success: false, Action: "PortsMap",
				Message: fmt.Sprintf("端口映射失败: %v", err)}, nil
		}
		return &model.ZMessage{Success: true, Action: "PortsMap",
			Message: fmt.Sprintf("端口 %d 成功映射到 %s:%d", nat.WanPort, nat.LanAddr, nat.LanPort)}, nil
	}

	if err := pf.removePortForwardFirewall(nat.LanAddr, nat.LanPort, nat.WanPort); err != nil {
		return &model.ZMessage{Success: false, Action: "PortsMap",
			Message: fmt.Sprintf("端口映射删除失败: %v", err)}, nil
	}
	return &model.ZMessage{Success: true, Action: "PortsMap",
		Message: fmt.Sprintf("端口 %d 映射已删除", nat.WanPort)}, nil
}

// ============================================================
// Docker / OCI
// ============================================================

// PortsMap 端口映射下发（宿主机 iptables）
func (d *DockerDriver) PortsMap(nat *model.PortData, add bool) (*model.ZMessage, error) {
	return portsMapTTY(d.Config, nat, add)
}

// ============================================================
// LXD
// ============================================================

// PortsMap 端口映射下发（宿主机 iptables）
func (d *LXDDriver) PortsMap(nat *model.PortData, add bool) (*model.ZMessage, error) {
	return portsMapTTY(d.Config, nat, add)
}

// ============================================================
// SmolVM (Firecracker)
// ============================================================

// PortsMap 端口映射下发（宿主机 iptables）
func (d *SmolVMDriver) PortsMap(nat *model.PortData, add bool) (*model.ZMessage, error) {
	return portsMapTTY(d.Config, nat, add)
}
