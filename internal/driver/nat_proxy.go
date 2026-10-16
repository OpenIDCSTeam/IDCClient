package driver

import "github.com/OpenIDCSTeam/HostAgent-Go/internal/model"

// NatPortProvider 平台自有的「端口映射」下发能力。
//
// 例如青州云通过云端 API（nat/create、nat/delete）管理端口映射；
// 容器类平台（Docker / LXD / SmolVM）通过宿主机 iptables 管理。
// 实现了该接口的主机，其端口映射在 service 层将走平台通道，而不是 iKuai 路由器。
type NatPortProvider interface {
	// PortsMap 端口映射下发（add=true 添加，false 删除）
	// add 且 nat.WanPort 为 0 时由实现负责分配外网端口并回填
	PortsMap(nat *model.PortData, add bool) (*model.ZMessage, error)
}

// NatProxyProvider 平台自有的「反向代理」下发能力（如青州云 web/create）。
//
// 注意：与 NatPortProvider 分开定义，是为了让容器类平台能够只实现端口映射
// （容器环境没有云端反向代理通道），而不必被迫实现 ProxyMap。
type NatProxyProvider interface {
	// ProxyMap 反向代理下发（add=true 添加，false 删除）
	ProxyMap(vmUUID string, proxy model.WebProxy, add bool) (*model.ZMessage, error)
}
