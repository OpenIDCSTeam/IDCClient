package service

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// ============================================================
// WAN 端口自动分配
// ============================================================

// 端口分配必须同时避开本地库与路由器上的占用
func TestPickWanPortSkipsLocalAndReserved(t *testing.T) {
	local := map[int]bool{11000: true, 11001: true}
	reserved := map[int]bool{11002: true}

	// 11000/11001 本地已用，11002 路由器已用 → 应跳到 11003
	if p, ok := pickWanPort(11000, 11005, local, reserved); !ok || p != 11003 {
		t.Errorf("应跳过本地与路由器占用的端口返回 11003，实际 (%d, %v)", p, ok)
	}

	// 范围被全部占用 → 明确失败，而不是返回一个冲突端口
	all := map[int]bool{}
	for p := 11000; p <= 11003; p++ {
		all[p] = true
	}
	if p, ok := pickWanPort(11000, 11003, all, nil); ok {
		t.Errorf("端口耗尽时应返回 ok=false，实际返回 %d", p)
	}

	// reserved 为 nil 时不应 panic
	if p, ok := pickWanPort(12000, 12001, nil, nil); !ok || p != 12000 {
		t.Errorf("空占用集合应返回区间起点，实际 (%d, %v)", p, ok)
	}
}

func TestPortRange(t *testing.T) {
	if low, high := portRange(&data.HSConfigRow{}); low != 10000 || high != 60000 {
		t.Errorf("未配置时缺省范围应为 10000-60000，实际 %d-%d", low, high)
	}
	// high <= low 属于配置错误，兜底为 low+1，避免死循环或空区间
	if low, high := portRange(&data.HSConfigRow{PortsStart: 20000, PortsClose: 20000}); high != low+1 {
		t.Errorf("非法端口范围应兜底为 low+1，实际 %d-%d", low, high)
	}
	if low, high := portRange(&data.HSConfigRow{PortsStart: 30001, PortsClose: 30000}); high != low+1 {
		t.Errorf("倒置端口范围应兜底为 low+1，实际 %d-%d", low, high)
	}
}

// newTestNATService 构造带临时库的 NAT 服务（ikuaiAddr 为空表示未配置爱快）
func newTestNATService(t *testing.T, ikuaiAddr string) *NATService {
	t.Helper()
	db, err := data.NewDB(t.TempDir())
	if err != nil {
		t.Fatalf("创建临时数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.CreateHost(&data.HSConfigRow{
		HsName:     "h1",
		ServerType: "PromoxSetup",
		IKuaiAddr:  ikuaiAddr,
		IKuaiUser:  "admin",
		IKuaiPass:  "pwd",
	}); err != nil {
		t.Fatalf("写入主机配置失败: %v", err)
	}
	return NewNATService(db)
}

// 网卡缺少 IP/MAC 时必须明确记为「跳过」而不是「成功」：
// 原先的无条件成功会让「一条规则都没下发」在日志里表现成绑定成功。
func TestBindStaticForVMSkipsNICWithoutAddress(t *testing.T) {
	// 全部网卡都被跳过时不会发起任何网络请求，地址可以是不可达的
	s := newTestNATService(t, "http://127.0.0.1:1")
	if !s.StaticBindConfigured("h1") {
		t.Fatal("配置了爱快地址时 StaticBindConfigured 应为 true")
	}

	vm := &model.VMConfig{NicAll: map[string]model.NCConfig{
		"ethernet0": {NicName: "ethernet0"},
	}}
	res, err := s.BindStaticForVM("h1", "vm-1", vm)
	if err != nil {
		t.Fatalf("跳过场景不应返回错误: %v", err)
	}
	if res.Bound != 0 || len(res.Skipped) != 1 || res.NoNIC || res.NotConfigured {
		t.Fatalf("应记录 1 张被跳过的网卡，实际 %+v", res)
	}
	if !strings.Contains(res.Skipped[0], "ethernet0") || !strings.Contains(res.Skipped[0], "mac") {
		t.Errorf("跳过原因应包含网卡名与缺失字段，实际 %q", res.Skipped[0])
	}
	if !strings.Contains(res.Summary(), "跳过 1 张") {
		t.Errorf("摘要应体现跳过数量，实际 %q", res.Summary())
	}
}

func TestBindStaticForVMNoNIC(t *testing.T) {
	s := newTestNATService(t, "http://127.0.0.1:1")
	res, err := s.BindStaticForVM("h1", "vm-1", nil)
	if err != nil {
		t.Fatalf("无网卡不应返回错误: %v", err)
	}
	if !res.NoNIC || res.Bound != 0 {
		t.Fatalf("应标记为没有网卡配置，实际 %+v", res)
	}
	if !strings.Contains(res.Summary(), "没有网卡配置") {
		t.Errorf("摘要应说明缺少网卡，实际 %q", res.Summary())
	}
}

// 主机未配置爱快属于可选能力缺失：必须静默跳过（不打日志、不报错），
// 否则每台虚拟机的创建都会带一条「绑定失败/跳过」的噪音。
func TestBindStaticForVMNotConfigured(t *testing.T) {
	s := newTestNATService(t, "")
	if s.StaticBindConfigured("h1") {
		t.Error("未配置爱快时 StaticBindConfigured 应为 false")
	}

	vm := &model.VMConfig{NicAll: map[string]model.NCConfig{
		"ethernet0": {NicName: "ethernet0", IP4Addr: "10.2.20.100", MacAddr: "10:f6:0a:02:14:64"},
	}}
	res, err := s.BindStaticForVM("h1", "vm-1", vm)
	if err != nil {
		t.Fatalf("未配置爱快应静默跳过: %v", err)
	}
	if !res.NotConfigured || res.Bound != 0 || len(res.Skipped) != 0 {
		t.Fatalf("应标记为未配置爱快，实际 %+v", res)
	}
	if !strings.Contains(res.Summary(), "未配置爱快") {
		t.Errorf("摘要应说明未配置爱快，实际 %q", res.Summary())
	}
}

// 爱快4 把原因放在 errors 字段，必须带到日志里（例如 lan_addr 为空）
func TestIKuaiErrMsgIncludesDetail(t *testing.T) {
	got := ikuaiErrMsg(map[string]interface{}{
		"code": 3001, "message": "请求参数不合法",
		"errors": "参数错误: lan_addr", "details": []interface{}{},
	})
	if !strings.Contains(got, "请求参数不合法") || !strings.Contains(got, "参数错误: lan_addr") {
		t.Errorf("错误信息应带上 errors 详情，实际 %q", got)
	}
	if strings.Contains(got, "[]") {
		t.Errorf("details 为数组时不应把 [] 拼进错误信息，实际 %q", got)
	}

	if got := ikuaiErrMsg(map[string]interface{}{"code": 1, "message": "失败"}); got != "失败" {
		t.Errorf("无详情字段时应只返回 message，实际 %q", got)
	}
}

// 拉取路由器现有端口映射（对应 Python 版 get_port 的 wan_list 收集）
func TestIKuaiListWanPorts(t *testing.T) {
	f := &fakeRouter{
		ver:     ikuaiVer4,
		sessKey: "k4",
		rows: map[string][]map[string]interface{}{
			"dnat": {
				{"id": 1, "wan_port": "11000", "lan_addr": "10.2.1.9", "lan_port": 22},
				{"id": 2, "wan_port": 11001, "lan_addr": "10.2.1.10", "lan_port": 80},
				{"id": 3, "wan_port": "", "lan_addr": "10.2.1.11", "lan_port": 443},
			},
		},
	}
	srv := httptest.NewTLSServer(f.handler())
	defer srv.Close()

	c := newIKuaiClient(srv.URL, "admin", "pwd")
	ports, err := c.listWanPorts()
	if err != nil {
		t.Fatalf("拉取端口映射列表失败: %v", err)
	}
	// 爱快把 wan_port 以字符串返回，部分版本为数字，两种都要认
	if !ports[11000] || !ports[11001] {
		t.Errorf("应解析出字符串/数字两种 wan_port，实际 %v", ports)
	}
	if len(ports) != 2 {
		t.Errorf("应解析出 2 个已占用端口（空值忽略），实际 %v", ports)
	}
	if ports[0] {
		t.Errorf("空 wan_port 不应计入占用，实际 %v", ports)
	}

	// 查询参数需与 Python 版 get_port 一致
	assertParam(t, f.lastCallParam(t), map[string]interface{}{
		"TYPE": "total,data", "ORDER_BY": "", "ORDER": "",
	})
}
