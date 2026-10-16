package driver

import (
	"testing"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// TestProxmoxBaseURL 覆盖 Proxmox 地址归一化。
//
// 回归背景：go-proxmox 的 NewClient 要求 baseURL 自带 /api2/json，
// 否则 client.Version 会请求 https://host:8006/version 并返回
// `500 no such file '/version'`。
func TestProxmoxBaseURL(t *testing.T) {
	cases := []struct {
		name string
		addr string
		want string
	}{
		{"裸IP", "192.168.1.50", "https://192.168.1.50:8006/api2/json"},
		{"带端口", "192.168.1.50:8006", "https://192.168.1.50:8006/api2/json"},
		{"带协议", "https://192.168.1.50:8006", "https://192.168.1.50:8006/api2/json"},
		{"带协议与尾斜杠", "https://192.168.1.50:8006/", "https://192.168.1.50:8006/api2/json"},
		{"已含API路径", "https://192.168.1.50:8006/api2/json", "https://192.168.1.50:8006/api2/json"},
		{"自定义端口", "pve.example.com:8443", "https://pve.example.com:8443/api2/json"},
		{"反向代理前缀", "https://pve.example.com/pve", "https://pve.example.com/pve/api2/json"},
		{"反向代理显式端口", "https://pve.example.com:8443/pve", "https://pve.example.com:8443/pve/api2/json"},
		{"空地址", "  ", "https://127.0.0.1:8006/api2/json"},
		{"IPv6", "[::1]:8006", "https://[::1]:8006/api2/json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := NewProxmoxDriver(&model.HSConfig{ServerAddr: tc.addr})
			if got := d.proxmoxBaseURL(); got != tc.want {
				t.Fatalf("proxmoxBaseURL(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

// TestProxmoxHostPort 校验 VNC 直连场景使用的 host:port（不带协议与路径）
func TestProxmoxHostPort(t *testing.T) {
	cases := []struct {
		addr string
		want string
	}{
		{"192.168.1.50", "192.168.1.50:8006"},
		{"https://192.168.1.50:8006/", "192.168.1.50:8006"},
		// 反向代理场景不带默认端口，沿用 https 默认端口
		{"https://pve.example.com/pve/api2/json", "pve.example.com"},
	}

	for _, tc := range cases {
		d := NewProxmoxDriver(&model.HSConfig{ServerAddr: tc.addr})
		if got := d.proxmoxHostPort(); got != tc.want {
			t.Fatalf("proxmoxHostPort(%q) = %q, want %q", tc.addr, got, tc.want)
		}
	}
}
