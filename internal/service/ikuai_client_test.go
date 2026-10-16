package service

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// ============================================================
// 模拟爱快路由器
// ============================================================

// fakeRouter 模拟爱快路由器的 /Action/login 与 /Action/call，
// 可按 ver 切换「爱快3 / 爱快4」两种协议形态。
type fakeRouter struct {
	ver     int
	sessKey string
	// wantPass 非空时校验登录 pass 字段，不匹配则返回登录失败
	wantPass string
	// failLogin 为 true 时登录恒失败
	failLogin bool
	// expireAt > 0 时，前 expireAt 次 /Action/call 返回「请先登录」
	expireAt int
	// rows 按 func_name 提供 show 动作的表格数据
	rows map[string][]map[string]interface{}

	mu     sync.Mutex
	logins []map[string]interface{}
	calls  []map[string]interface{}
}

func (f *fakeRouter) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/Action/login", f.handleLogin)
	mux.HandleFunc("/Action/call", f.handleCall)
	return mux
}

func (f *fakeRouter) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	f.logins = append(f.logins, body)
	fail := f.failLogin
	wantPass := f.wantPass
	f.mu.Unlock()

	if fail || (wantPass != "" && strVal(body["pass"]) != wantPass) {
		writeFakeJSON(w, f.resp(false, "用户名或密码错误", "", nil))
		return
	}

	http.SetCookie(w, &http.Cookie{Name: "sess_key", Value: f.sessKey, Path: "/"})
	if f.ver == ikuaiVer4 {
		// 爱快 4 的登录响应体只有 Result/code/message（实测），
		// 且 sess_key 只走 set-cookie
		writeFakeJSON(w, map[string]interface{}{"Result": 10000, "code": 0, "message": "成功"})
		return
	}
	writeFakeJSON(w, map[string]interface{}{"Result": 10000, "ErrMsg": ""})
}

func (f *fakeRouter) handleCall(w http.ResponseWriter, r *http.Request) {
	var body map[string]interface{}
	_ = json.NewDecoder(r.Body).Decode(&body)

	f.mu.Lock()
	f.calls = append(f.calls, body)
	nth := len(f.calls)
	expireAt := f.expireAt
	f.mu.Unlock()

	if expireAt > 0 && nth <= expireAt {
		writeFakeJSON(w, f.resp(false, "请先登录", "", nil))
		return
	}

	funcName := strVal(body["func_name"])
	if strVal(body["action"]) == "show" {
		writeFakeJSON(w, f.resp(true, "Success", funcName, f.rows[funcName]))
		return
	}
	writeFakeJSON(w, f.resp(true, "Success", funcName, nil))
}

// resp 按版本生成响应体（形态与真机一致）：
//
//	爱快3：{"ErrMsg":"Success","Data":{"total":N,"data":[…]}}
//	爱快4：{"code":0,"message":"Success","results":{"total":N,"data":[…]}}
func (f *fakeRouter) resp(ok bool, msg, funcName string, rows []map[string]interface{}) map[string]interface{} {
	// DHCP 静态地址表在真机上使用 static_data 键
	listKey := "data"
	if funcName == "dhcp_static" {
		listKey = "static_data"
	}

	if f.ver == ikuaiVer4 {
		if !ok {
			return map[string]interface{}{"code": 1, "message": msg}
		}
		out := map[string]interface{}{"code": 0, "message": msg, "rowid": 1}
		if rows != nil {
			out["results"] = map[string]interface{}{"total": len(rows), listKey: rows}
		}
		return out
	}
	if !ok {
		return map[string]interface{}{"Result": 10001, "ErrMsg": msg}
	}
	out := map[string]interface{}{"ErrMsg": "Success"}
	if rows != nil {
		out["Data"] = map[string]interface{}{"total": len(rows), listKey: rows}
	}
	return out
}

func writeFakeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// lastCallParam 返回最近一次 /Action/call 的 param
func (f *fakeRouter) lastCallParam(t *testing.T) map[string]interface{} {
	t.Helper()
	if len(f.calls) == 0 {
		t.Fatal("没有收到 /Action/call 请求")
	}
	param, ok := f.calls[len(f.calls)-1]["param"].(map[string]interface{})
	if !ok {
		t.Fatalf("请求体缺少 param: %v", f.calls[len(f.calls)-1])
	}
	return param
}

// ============================================================
// 地址归一化与版本优先级
// ============================================================

func TestIKuaiAddrNormalize(t *testing.T) {
	cases := []struct {
		in       string
		wantBase string
		wantExp  bool
	}{
		{"10.2.1.20", "http://10.2.1.20", false},
		{"10.2.1.20/", "http://10.2.1.20", false},
		{"http://10.2.1.20", "http://10.2.1.20", true},
		{"HTTPS://10.2.1.20/", "https://10.2.1.20", true},
		{"  https://10.2.1.20:8443  ", "https://10.2.1.20:8443", true},
	}
	for _, c := range cases {
		base, exp := normalizeIKuaiAddr(c.in)
		if base != c.wantBase || exp != c.wantExp {
			t.Errorf("normalizeIKuaiAddr(%q) = (%q, %v)，期望 (%q, %v)", c.in, base, exp, c.wantBase, c.wantExp)
		}
	}
}

func TestIKuaiVersionsForScheme(t *testing.T) {
	if got := versionsForScheme("https://10.2.1.20"); got[0] != ikuaiVer4 {
		t.Errorf("https 应优先按爱快4 解析，实际 %v", got)
	}
	if got := versionsForScheme("http://10.2.1.20"); got[0] != ikuaiVer3 {
		t.Errorf("http 应优先按爱快3 解析，实际 %v", got)
	}
}

// ============================================================
// 爱快 3：明文 pass + ErrMsg/Data 响应
// ============================================================

func TestIKuaiV3LoginAndParams(t *testing.T) {
	const pass = "p@ss"
	f := &fakeRouter{ver: ikuaiVer3, sessKey: "k3", wantPass: ikuaiSaltPrefix + pass}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := newIKuaiClient(srv.URL, "admin", pass)
	if err := c.login(); err != nil {
		t.Fatalf("爱快3 登录失败: %v", err)
	}
	if c.ver != ikuaiVer3 {
		t.Fatalf("应识别为爱快3，实际 %d", c.ver)
	}
	if c.sessKey != "k3" {
		t.Fatalf("sess_key 提取失败: %q", c.sessKey)
	}
	if len(f.logins) != 1 {
		t.Fatalf("http 地址应一次登录成功，实际尝试 %d 次", len(f.logins))
	}
	if got := strVal(f.logins[0]["pass"]); got != ikuaiSaltPrefix+pass {
		t.Errorf("爱快3 的 pass 应为明文加盐，实际 %q", got)
	}
	sum := md5.Sum([]byte(pass))
	if got := strVal(f.logins[0]["passwd"]); got != hex.EncodeToString(sum[:]) {
		t.Errorf("passwd 应为密码的 md5，实际 %q", got)
	}

	// DHCP 静态地址：爱快3 用 newRow + gateway="auto"
	if err := c.addDHCPStatic("1.1.1.1", "11:22:33:44:55:66", "vm-1", "119.29.29.29", "223.5.5.5"); err != nil {
		t.Fatalf("添加DHCP静态地址失败: %v", err)
	}
	p := f.lastCallParam(t)
	if p["newRow"] != true || p["gateway"] != "auto" {
		t.Errorf("爱快3 的 dhcp_static 参数不匹配: %v", p)
	}
	if _, has := p["tagname"]; has {
		t.Errorf("爱快3 不应带 tagname 字段: %v", p)
	}

	// ARP 绑定：bind_type 为数字 0
	if err := c.addARP("1.1.1.1", "11:22:33:44:55:66", "vm-1"); err != nil {
		t.Fatalf("添加ARP绑定失败: %v", err)
	}
	ap := f.lastCallParam(t)
	if ap["bind_type"] != float64(0) {
		t.Errorf("爱快3 的 arp bind_type 应为数字 0，实际 %#v", ap["bind_type"])
	}

	// 端口映射：爱快3 沿用原有参数集，不带 tagname
	if err := c.addPortMap(model.PortData{WanPort: 10001, LanPort: 22, LanAddr: "10.2.1.9", NatTips: "ssh"}); err != nil {
		t.Fatalf("添加端口映射失败: %v", err)
	}
	dp := f.lastCallParam(t)
	if dp["protocol"] != "tcp+udp" || dp["interface"] != "wan1" {
		t.Errorf("爱快3 的 dnat 参数不匹配: %v", dp)
	}
	if _, has := dp["tagname"]; has {
		t.Errorf("爱快3 不应带 tagname 字段: %v", dp)
	}
}

// ============================================================
// 爱快 4：自签名 https + base64 pass + code/message 响应
// ============================================================

func TestIKuaiV4TLSLoginAndParams(t *testing.T) {
	const pass = "IM807581"
	f := &fakeRouter{
		ver:      ikuaiVer4,
		sessKey:  "707ea6b742f5c1137ad2bad8c55e55fb",
		wantPass: base64.StdEncoding.EncodeToString([]byte(ikuaiSaltPrefix + pass)),
	}
	// httptest 的 TLS 服务端使用自签名证书：能登录成功即证明已跳过证书校验
	srv := httptest.NewTLSServer(f.handler())
	defer srv.Close()

	c := newIKuaiClient(srv.URL, "admin", pass)
	if err := c.login(); err != nil {
		t.Fatalf("自签名 https 登录失败: %v", err)
	}
	if c.ver != ikuaiVer4 {
		t.Fatalf("https 应识别为爱快4，实际 %d", c.ver)
	}
	if c.sessKey != "707ea6b742f5c1137ad2bad8c55e55fb" {
		t.Fatalf("sess_key 提取失败: %q", c.sessKey)
	}
	if len(f.logins) != 1 {
		t.Fatalf("爱快4 地址应一次登录成功，实际尝试 %d 次", len(f.logins))
	}
	// 无响应体也应视为登录成功（爱快4 只回 set-cookie）
	if got := strVal(f.logins[0]["pass"]); got != f.wantPass {
		t.Errorf("爱快4 的 pass 应为 base64(salt_11+密码)，实际 %q", got)
	}

	// DHCP 静态地址：爱快4 用 tagname，不再发送 newRow
	if err := c.addDHCPStatic("1.1.1.1", "11:22:33:44:55:66", "vm-1", "", ""); err != nil {
		t.Fatalf("添加DHCP静态地址失败: %v", err)
	}
	p := f.lastCallParam(t)
	if p["tagname"] != "vm-1" {
		t.Errorf("爱快4 的 dhcp_static 应带 tagname，实际 %v", p)
	}
	if _, has := p["newRow"]; has {
		t.Errorf("爱快4 不应带 newRow 字段: %v", p)
	}
	if _, has := p["gateway"]; !has {
		t.Errorf("爱快4 的 dhcp_static 应显式发送 gateway 字段: %v", p)
	}

	// ARP 绑定：爱快4 需要 tagname / termname，且 bind_type 为字符串
	if err := c.addARP("1.1.1.1", "11:22:33:44:55:66", "vm-1"); err != nil {
		t.Fatalf("添加ARP绑定失败: %v", err)
	}
	ap := f.lastCallParam(t)
	if ap["tagname"] != "vm-1" || ap["termname"] != "vm-1" {
		t.Errorf("爱快4 的 arp 应带 tagname/termname，实际 %v", ap)
	}
	if ap["bind_type"] != "0" {
		t.Errorf("爱快4 的 arp bind_type 应为字符串 \"0\"，实际 %#v", ap["bind_type"])
	}

	// 端口映射：爱快4 的 dnat 缺 tagname 会被拒绝（实测 code=3001），必须补上
	if err := c.addPortMap(model.PortData{WanPort: 10001, LanPort: 22, LanAddr: "10.2.1.9", NatTips: "ssh-22"}); err != nil {
		t.Fatalf("添加端口映射失败: %v", err)
	}
	dp := f.lastCallParam(t)
	if dp["tagname"] != "ssh-22" || dp["termname"] != "ssh-22" {
		t.Errorf("爱快4 的 dnat 应带 tagname/termname，实际 %v", dp)
	}
}

// ============================================================
// 版本回退：https 上其实跑着爱快3
// ============================================================

func TestIKuaiVersionFallbackOnHTTPS(t *testing.T) {
	const pass = "legacy"
	f := &fakeRouter{ver: ikuaiVer3, sessKey: "k3", wantPass: ikuaiSaltPrefix + pass}
	srv := httptest.NewTLSServer(f.handler())
	defer srv.Close()

	c := newIKuaiClient(srv.URL, "admin", pass)
	if err := c.login(); err != nil {
		t.Fatalf("版本回退登录失败: %v", err)
	}
	if c.ver != ikuaiVer3 {
		t.Fatalf("回退后应识别为爱快3，实际 %d", c.ver)
	}
	// 先按爱快4 试一次（被拒绝），再按爱快3 成功
	if len(f.logins) != 2 {
		t.Fatalf("应经历 2 次登录尝试，实际 %d 次", len(f.logins))
	}
}

// ============================================================
// 会话失效自动重登
// ============================================================

func TestIKuaiSessionExpiryTriggersRelogin(t *testing.T) {
	f := &fakeRouter{ver: ikuaiVer4, sessKey: "k4", expireAt: 1}
	srv := httptest.NewTLSServer(f.handler())
	defer srv.Close()

	c := newIKuaiClient(srv.URL, "admin", "pwd")
	if err := c.addARP("1.1.1.1", "11:22:33:44:55:66", "vm-1"); err != nil {
		t.Fatalf("会话失效后重登失败: %v", err)
	}
	if len(f.logins) != 2 {
		t.Errorf("应重新登录 1 次（共 2 次登录），实际 %d 次", len(f.logins))
	}
	if len(f.calls) != 2 {
		t.Errorf("应在重登后重试调用（共 2 次），实际 %d 次", len(f.calls))
	}
}

// 成功响应不能被误判为会话失效（"Success" 里含有 "sess" 子串）
func TestIKuaiSuccessNotTreatedAsSessionExpired(t *testing.T) {
	if isSessionExpired(map[string]interface{}{"ErrMsg": "Success"}) {
		t.Error("爱快3 的成功响应被误判为会话失效")
	}
	if isSessionExpired(map[string]interface{}{"code": 0, "message": "Success"}) {
		t.Error("爱快4 的成功响应被误判为会话失效")
	}
	if !isSessionExpired(map[string]interface{}{"code": 1, "message": "请先登录"}) {
		t.Error("爱快4 的未登录响应应被判为会话失效")
	}
}

// ============================================================
// 列表查询与删除（含 rowid 主键与幂等）
// ============================================================

func TestIKuaiDeleteUsesRowIDAndIsIdempotent(t *testing.T) {
	f := &fakeRouter{
		ver:     ikuaiVer4,
		sessKey: "k4",
		rows: map[string][]map[string]interface{}{
			"dhcp_static": {{"rowid": 7, "ip_addr": "1.1.1.1", "mac": "11:22:33:44:55:66"}},
			"arp":         {{"rowid": 9, "ip_addr": "1.1.1.1", "mac": "11:22:33:44:55:66"}},
		},
	}
	srv := httptest.NewTLSServer(f.handler())
	defer srv.Close()

	c := newIKuaiClient(srv.URL, "admin", "pwd")

	if err := c.deleteDHCPStatic("1.1.1.1", "11:22:33:44:55:66"); err != nil {
		t.Fatalf("删除DHCP静态地址失败: %v", err)
	}
	if f.calls[len(f.calls)-1]["action"] != "del" {
		t.Fatalf("最后一次调用应为 del，实际 %v", f.calls[len(f.calls)-1])
	}
	if got := strVal(f.lastCallParam(t)["id"]); got != "7" {
		t.Errorf("删除 DHCP 应使用列表中的 rowid，实际 %q", got)
	}

	if err := c.deleteARP("1.1.1.1", "11:22:33:44:55:66"); err != nil {
		t.Fatalf("删除ARP绑定失败: %v", err)
	}
	if got := strVal(f.lastCallParam(t)["id"]); got != "9" {
		t.Errorf("删除 ARP 应使用列表中的 rowid，实际 %q", got)
	}

	// 仅按 MAC 解绑时，del 必须回传列表行内的 IP（与 Python 版 del_arps 一致）
	if err := c.deleteARP("", "11:22:33:44:55:66"); err != nil {
		t.Fatalf("按 MAC 解绑 ARP 失败: %v", err)
	}
	if f.calls[len(f.calls)-1]["action"] != "del" {
		t.Fatalf("最后一次调用应为 del，实际 %v", f.calls[len(f.calls)-1])
	}
	delParam := f.lastCallParam(t)
	if strVal(delParam["ip_addr"]) != "1.1.1.1" || strVal(delParam["id"]) != "9" {
		t.Errorf("del 应回传行内 IP/ID，实际 %v", delParam)
	}

	// 不存在的记录：返回可识别的 not found，便于上层按幂等处理
	if err := c.deleteARP("9.9.9.9", ""); !errors.Is(err, errIKuaiNotFound) {
		t.Errorf("不存在的 ARP 记录应返回 errIKuaiNotFound，实际 %v", err)
	}
}

// 爱快3 的查询参数必须与 Python 版 HostModule/NetsManager.py 完全一致，
// 否则改造后老设备会查不到已有条目（进而无法销账）。
func TestIKuaiV3ShowParamsMatchPython(t *testing.T) {
	f := &fakeRouter{ver: ikuaiVer3, sessKey: "k3"}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	c := newIKuaiClient(srv.URL, "admin", "pwd")

	// get_dhcp
	if _, err := c.findDHCPStaticID("1.1.1.1", ""); err != nil {
		t.Fatalf("查询DHCP静态地址列表失败: %v", err)
	}
	assertParam(t, f.lastCallParam(t), map[string]interface{}{
		"TYPE": "static_total,static_data", "ORDER_BY": "", "ORDER": "",
	})

	// get_arps
	if _, err := c.findARPRow("1.1.1.1", ""); err != nil {
		t.Fatalf("查询ARP列表失败: %v", err)
	}
	assertParam(t, f.lastCallParam(t), map[string]interface{}{
		"TYPE": "total,data", "ORDER_BY": "ip_addr_int", "orderType": "IP", "ORDER": "asc",
	})

	// get_port（列表为空，只断言查询参数）
	if err := c.deletePortMap(22, "10.2.1.9"); err == nil {
		t.Error("列表为空时应返回「未找到匹配的端口映射」")
	}
	assertParam(t, f.lastCallParam(t), map[string]interface{}{
		"TYPE": "total,data", "ORDER_BY": "", "ORDER": "",
	})
}

func assertParam(t *testing.T, got, want map[string]interface{}) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("参数键不一致：got=%v want=%v", got, want)
		return
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("参数 %s = %#v，期望 %#v", k, got[k], v)
		}
	}
}

// 爱快对 tagname/termname 有 15 字节上限，超长整条请求被拒（实测 code=3001）。
// 虚拟机名与端口备注长度都不可控，因此下发前必须自行截断。
func TestIKuaiTagTruncation(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"  spaced  ", "spaced"},
		{"pve-1oFN4o6C", "pve-1oFN4o6C"},                 // 12 字节：原样
		{"0123456789abcde", "0123456789abcde"},           // 15 字节：边界值，允许
		{"0123456789abcdef", "0123456789abcde"},          // 16 字节：截断
		{"verify-netcheck-1", "verify-netcheck"},         // 17 字节：截断到 15
		{"端口映射说明文字很长", "端口映射说"},              // 多字节：15 字节恰好 5 个汉字，须落在合法 rune 边界
	}
	for _, c := range cases {
		got := ikuaiTag(c.in)
		if got != c.want {
			t.Errorf("ikuaiTag(%q) = %q，期望 %q", c.in, got, c.want)
		}
		if len(got) > ikuaiTagMaxLen {
			t.Errorf("ikuaiTag(%q) 结果 %q 超过 %d 字节", c.in, got, ikuaiTagMaxLen)
		}
		if !utf8.ValidString(got) {
			t.Errorf("ikuaiTag(%q) 结果 %q 不是合法 UTF-8", c.in, got)
		}
	}

	// 超长虚拟机名/备注经参数构造后必须已被截断
	long := strings.Repeat("x", 40)
	p := dhcpStaticParam(ikuaiVer4, "add", "1.1.1.1", "11:22:33:44:55:66", long, "", "")
	if got := strVal(p["tagname"]); len(got) > ikuaiTagMaxLen {
		t.Errorf("dhcp_static 的 tagname 未截断: %q", got)
	}
	if got := strVal(p["comment"]); got != long {
		t.Errorf("comment 不受限，应保持原样，实际 %q", got)
	}

	ap := arpParam(ikuaiVer4, "add", "1.1.1.1", "11:22:33:44:55:66", long)
	for _, key := range []string{"tagname", "termname"} {
		if got := strVal(ap[key]); len(got) > ikuaiTagMaxLen {
			t.Errorf("arp 的 %s 未截断: %q", key, got)
		}
	}

	// 端口备注（NatTips）同样会被用作 tagname
	tag := dnatTag(model.PortData{WanPort: 10001, NatTips: long})
	if len(tag) > ikuaiTagMaxLen {
		t.Errorf("dnat 的 tagname 未截断: %q", tag)
	}
	// 备注为空时回落为 port-<wan_port>
	if got := dnatTag(model.PortData{WanPort: 12345}); got != "port-12345" {
		t.Errorf("空备注应回落为 port-<端口>，实际 %q", got)
	}
}

// show 响应同时兼容爱快3 的 Data.* 与爱快4 的 results.*（实测形态）
func TestIKuaiRowsCompatibility(t *testing.T) {
	v3 := map[string]interface{}{
		"ErrMsg": "Success",
		"Data":   map[string]interface{}{"total": 1, "static_data": []interface{}{map[string]interface{}{"id": 1}}},
	}
	if rows := ikuaiRows(v3); len(rows) != 1 {
		t.Errorf("爱快3 static_data 解析失败: %v", v3)
	}

	// 真机（爱快4）返回： {"code":0,"message":"Success","results":{"static_total":1,"static_data":[…]}}
	v4 := map[string]interface{}{
		"code":    0,
		"message": "Success",
		"results": map[string]interface{}{
			"static_total": 1,
			"static_data":  []interface{}{map[string]interface{}{"id": 2, "ip_addr": "1.1.1.1"}},
		},
	}
	rows := ikuaiRows(v4)
	if len(rows) != 1 || ikuaiRowID(rows[0].(map[string]interface{})) != "2" {
		t.Errorf("爱快4 results 解析失败: %v", v4)
	}

	// results 下直接是数组的形态
	v4b := map[string]interface{}{"code": 0, "results": []interface{}{map[string]interface{}{"rowid": 3}}}
	if rows := ikuaiRows(v4b); len(rows) != 1 {
		t.Errorf("爱快4 results 数组解析失败: %v", v4b)
	}
}
