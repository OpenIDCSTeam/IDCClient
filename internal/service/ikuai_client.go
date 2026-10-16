// Package service 爱快（iKuai）路由器 Web API 客户端。
//
// 同时兼容爱快 3.x 与爱快 4.x，两者的差异集中在四处：
//
//	差异项      爱快 3.x                                 爱快 4.x
//	默认协议    http                                     https（多为自签名证书）
//	登录 pass   "salt_11" + 明文密码                       base64("salt_11" + 明文密码)
//	调用响应    {"ErrMsg":"Success","Data":{…}}            {"code":0,"message":"Success","rowid":1}
//	表格参数    依赖 newRow / gateway="auto"               使用 tagname / termname，bind_type 为字符串
//
// 版本识别：按地址的 scheme 决定尝试顺序（https 先试 4.x，http 先试 3.x），
// 命中后把版本缓存到客户端实例，之后所有请求都按该版本构造参数并解析响应。
// 当用户没有显式写 scheme（默认补 http://）时，如果该 scheme 下所有版本都失败，
// 会再换另一个 scheme 兜底，避免「只填 IP」的配置连不上仅开放 443 的爱快 4。
package service

import (
	"bytes"
	"crypto/md5"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// 爱快协议版本（0 表示尚未识别，登录成功后确定）
const (
	ikuaiVerUnknown = 0
	ikuaiVer3       = 3
	ikuaiVer4       = 4
)

const (
	ikuaiSaltPrefix = "salt_11"
	ikuaiLoginPath  = "/Action/login"
	ikuaiCallPath   = "/Action/call"

	// ikuaiRequestTimeout 单次 HTTP 请求超时
	ikuaiRequestTimeout = 15 * time.Second

	// 登录失败熔断：连续失败达到阈值后暂停登录一段时间。
	// 爱快在多次登录失败后会锁定账号，配置写错（密码/地址）时
	// 必须避免每个 API 调用都盲目重试。
	ikuaiLoginFailLimit = 3
	ikuaiLoginCooldown  = 5 * time.Minute
)

// errIKuaiNotFound 目标记录在爱快上不存在。
// 解绑场景（虚拟机删除、重复解绑）下应视为成功，调用方用 errors.Is 判断。
var errIKuaiNotFound = errors.New("爱快上未找到匹配的记录")

// ============================================================
// 客户端
// ============================================================

type ikuaiClient struct {
	baseURL  string
	username string
	password string

	sessKey string
	// ver 当前生效的协议版本（ikuaiVer3 / ikuaiVer4）
	ver int
	// explicitScheme 用户配置里显式写了 http:// 或 https://，
	// 此时不做跨协议兜底，完全尊重用户配置。
	explicitScheme bool

	failCount   int
	blockedTill time.Time
	lastErr     error

	http *http.Client
	mu   sync.Mutex
}

// newIKuaiClient 创建爱快客户端。addr 未带 scheme 时默认补 http://。
func newIKuaiClient(addr, username, password string) *ikuaiClient {
	base, explicit := normalizeIKuaiAddr(addr)
	return &ikuaiClient{
		baseURL:        base,
		username:       username,
		password:       password,
		ver:            ikuaiVerUnknown,
		explicitScheme: explicit,
		http: &http.Client{
			Timeout: ikuaiRequestTimeout,
			Transport: &http.Transport{
				// 爱快 4 默认签发自签名 HTTPS 证书，必须跳过校验，
				// 否则配置成 https:// 的设备永远无法完成登录。
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

// normalizeIKuaiAddr 归一化路由器地址，返回带小写 scheme 的地址，
// 以及用户是否显式指定了 scheme。
func normalizeIKuaiAddr(addr string) (string, bool) {
	base := strings.TrimSpace(addr)
	lower := strings.ToLower(base)
	switch {
	case strings.HasPrefix(lower, "https://"):
		return "https://" + strings.TrimRight(base[len("https://"):], "/"), true
	case strings.HasPrefix(lower, "http://"):
		return "http://" + strings.TrimRight(base[len("http://"):], "/"), true
	}
	return "http://" + strings.TrimRight(base, "/"), false
}

func (c *ikuaiClient) isHTTPS() bool {
	return strings.HasPrefix(c.baseURL, "https://")
}

// altURL 返回另一个 scheme 的地址（http <-> https）
func (c *ikuaiClient) altURL() string {
	if c.isHTTPS() {
		return "http://" + strings.TrimPrefix(c.baseURL, "https://")
	}
	return "https://" + strings.TrimPrefix(c.baseURL, "http://")
}

// versionsForScheme 返回按优先级排列的待尝试协议版本。
// https 地址优先按爱快 4 解析，http 地址优先按爱快 3 解析。
func versionsForScheme(base string) []int {
	if strings.HasPrefix(strings.ToLower(base), "https://") {
		return []int{ikuaiVer4, ikuaiVer3}
	}
	return []int{ikuaiVer3, ikuaiVer4}
}

// ============================================================
// 登录
// ============================================================

// login 登录爱快路由器并缓存 sess_key 与协议版本。
func (c *ikuaiClient) login() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.sessKey != "" {
		return nil
	}
	now := time.Now()
	if !c.blockedTill.IsZero() && now.Before(c.blockedTill) {
		if c.lastErr != nil {
			return c.lastErr
		}
		return fmt.Errorf("爱快登录失败次数过多，暂时停止登录尝试")
	}

	bases := []string{c.baseURL}
	if !c.explicitScheme {
		if alt := c.altURL(); alt != c.baseURL {
			bases = append(bases, alt)
		}
	}

	var lastErr error
	for _, base := range bases {
		for _, ver := range versionsForScheme(base) {
			sessKey, err := c.tryLogin(base, ver)
			if err != nil {
				lastErr = err
				continue
			}
			c.baseURL = base
			c.sessKey = sessKey
			c.ver = ver
			c.failCount = 0
			c.lastErr = nil
			return nil
		}
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("爱快登录失败")
	}
	c.failCount++
	c.lastErr = lastErr
	if c.failCount >= ikuaiLoginFailLimit {
		c.blockedTill = now.Add(ikuaiLoginCooldown)
		c.failCount = 0
	}
	return lastErr
}

// tryLogin 用指定地址与协议版本尝试登录一次，返回 sess_key。
//
// 爱快 3 的响应体为 {"Result":10000,...}；爱快 4 只回 set-cookie，
// 响应体可能为空，因此以「能否拿到 sess_key」作为主要成功依据。
func (c *ikuaiClient) tryLogin(base string, ver int) (string, error) {
	sum := md5.Sum([]byte(c.password))
	salted := ikuaiSaltPrefix + c.password

	payload := map[string]interface{}{
		"username":          c.username,
		"passwd":            hex.EncodeToString(sum[:]),
		"remember_password": "",
	}
	if ver == ikuaiVer4 {
		// 爱快 4：pass 为 base64("salt_11"+密码)
		payload["pass"] = base64.StdEncoding.EncodeToString([]byte(salted))
	} else {
		// 爱快 3：pass 为 "salt_11"+密码 明文
		payload["pass"] = salted
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	resp, err := c.http.Post(base+ikuaiLoginPath, "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("连接爱快路由器失败: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	sessKey := extractSessKey(resp)

	var result map[string]interface{}
	parseErr := json.Unmarshal(respBody, &result)
	if parseErr == nil && len(result) > 0 && !ikuaiSuccess(result) {
		// 明确返回了失败结果（如 Result=10001 / code!=0），
		// 即使带了 sess_key 也不能当作登录成功。
		return "", fmt.Errorf("爱快登录失败: %s", ikuaiErrMsg(result))
	}
	if sessKey == "" {
		if parseErr != nil {
			return "", fmt.Errorf("爱快登录响应无法解析（HTTP %d）", resp.StatusCode)
		}
		return "", fmt.Errorf("爱快登录成功但未获取到 sess_key")
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("爱快登录失败（HTTP %d）", resp.StatusCode)
	}
	return sessKey, nil
}

// extractSessKey 从登录响应中提取 sess_key。
// 优先使用 Cookie 解析（大小写不敏感），再退回手工解析原始 Set-Cookie 头。
func extractSessKey(resp *http.Response) string {
	for _, ck := range resp.Cookies() {
		if ck.Name == "sess_key" && ck.Value != "" {
			return ck.Value
		}
	}
	for _, h := range resp.Header.Values("Set-Cookie") {
		idx := strings.Index(strings.ToLower(h), "sess_key=")
		if idx < 0 {
			continue
		}
		rest := h[idx+len("sess_key="):]
		if end := strings.IndexAny(rest, ";,"); end >= 0 {
			rest = rest[:end]
		}
		if v := strings.TrimSpace(rest); v != "" {
			return v
		}
	}
	return ""
}

func (c *ikuaiClient) clearSession() {
	c.mu.Lock()
	c.sessKey = ""
	c.mu.Unlock()
}

// ============================================================
// 内部 API 调用
// ============================================================

// call 调用爱快内部 API（会话失效时自动重新登录一次）。
// build 回调按当前已识别的协议版本生成 param，保证首次调用时
// 先完成登录与版本识别，再构造参数。
func (c *ikuaiClient) call(funcName, action string, build func(ver int) map[string]interface{}) (map[string]interface{}, error) {
	result, err := c.callOnce(funcName, action, build)
	if err != nil {
		return nil, err
	}
	if isSessionExpired(result) {
		c.clearSession()
		result, err = c.callOnce(funcName, action, build)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (c *ikuaiClient) callOnce(funcName, action string, build func(ver int) map[string]interface{}) (map[string]interface{}, error) {
	if err := c.login(); err != nil {
		return nil, err
	}

	c.mu.Lock()
	base := c.baseURL
	sessKey := c.sessKey
	ver := c.ver
	c.mu.Unlock()

	param := map[string]interface{}{}
	if build != nil {
		if p := build(ver); p != nil {
			param = p
		}
	}
	payload := map[string]interface{}{
		"func_name": funcName,
		"action":    action,
		"param":     param,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, base+ikuaiCallPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", fmt.Sprintf("sess_key=%s; username=%s; login=1", sessKey, c.username))

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用爱快API失败: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	// 会话被清理时爱快返回 401/403，转成可识别的会话失效标记，交由 call 重登
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return map[string]interface{}{"ErrMsg": "session expired"}, nil
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("解析爱快响应失败（HTTP %d）", resp.StatusCode)
	}
	return result, nil
}

// ============================================================
// 端口映射（dnat）
// ============================================================

// addPortMap 添加 TCP/UDP 端口映射
func (c *ikuaiClient) addPortMap(nat model.PortData) error {
	result, err := c.call("dnat", "add", func(ver int) map[string]interface{} {
		param := map[string]interface{}{
			"enabled":   "yes",
			"comment":   nat.NatTips,
			"interface": "wan1",
			"lan_addr":  nat.LanAddr,
			"protocol":  "tcp+udp",
			"wan_port":  nat.WanPort,
			"lan_port":  nat.LanPort,
			"src_addr":  "",
		}
		if ver == ikuaiVer4 {
			// 爱快4 的 dnat 表强制要求行标识，缺失时返回
			// {"code":3001,"message":"请求参数不合法","errors":"参数错误: tagname"}
			tag := dnatTag(nat)
			param["tagname"] = tag
			param["termname"] = tag
		}
		return param
	})
	if err != nil {
		return err
	}
	if !ikuaiSuccess(result) {
		return fmt.Errorf("添加端口映射失败: %s", ikuaiErrMsg(result))
	}
	return nil
}

// dnatTag 生成端口映射的行标识（爱快4 必填，爱快3 忽略）
func dnatTag(nat model.PortData) string {
	if tag := strings.TrimSpace(nat.NatTips); tag != "" {
		return ikuaiTag(tag)
	}
	return fmt.Sprintf("port-%d", nat.WanPort)
}

// ikuaiTagMaxLen tagname / termname 的长度上限（字节）。
//
// 实测（爱快 4，dhcp_static / dnat / arp 三张表一致）：
//
//	15 字节 → code=0 成功
//	16 字节 → {"code":3001,"message":"请求参数不合法","errors":"参数错误: tagname"}
//
// 也就是说超长不会被截断，而是整条请求被拒。而 comment 字段不受此限制
// （40 字节仍可提交）。此前把虚拟机名（长度不可控）直接当 tagname，
// 只要名字稍长，DHCP 静态绑定与端口映射就会整体失败。
const ikuaiTagMaxLen = 15

// ikuaiTag 生成爱快行标识：去空白，超长时按 rune 边界截断。
// 这些字段只是行标签，截断不影响规则语义。
func ikuaiTag(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= ikuaiTagMaxLen {
		return s
	}
	// 按字节截断可能切断多字节字符，回退到合法的 rune 边界
	t := s[:ikuaiTagMaxLen]
	for len(t) > 0 && !utf8.ValidString(t) {
		t = t[:len(t)-1]
	}
	return t
}

// dnatShowParam 端口映射列表查询参数（与 Python 版 get_port 完全一致）
func dnatShowParam(ver int) map[string]interface{} {
	return map[string]interface{}{"TYPE": "total,data", "ORDER_BY": "", "ORDER": ""}
}

// listWanPorts 返回路由器上已被占用的 WAN 端口集合。
//
// 对应 Python 版 BasicServer.PortsMap 里的 get_port() + wan_list 收集：
// 路由器上可能存在本地数据库不认识的规则（管理员手工添加的），
// 自动分配端口时必须把这些端口一起排除，否则会下发冲突的映射。
func (c *ikuaiClient) listWanPorts() (map[int]bool, error) {
	list, err := c.call("dnat", "show", dnatShowParam)
	if err != nil {
		return nil, err
	}
	if !ikuaiSuccess(list) {
		return nil, fmt.Errorf("获取端口映射列表失败: %s", ikuaiErrMsg(list))
	}

	ports := make(map[int]bool)
	for _, it := range ikuaiRows(list) {
		row, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		// 爱快把 wan_port 以字符串返回（如 "65000"），部分版本为数字
		if p, convErr := strconv.Atoi(strVal(row["wan_port"])); convErr == nil && p > 0 {
			ports[p] = true
		}
	}
	return ports, nil
}

// deletePortMap 按内网端口 / 内网地址查找并删除端口映射
func (c *ikuaiClient) deletePortMap(lanPort int, lanAddr string) error {
	list, err := c.call("dnat", "show", dnatShowParam)
	if err != nil {
		return err
	}
	if !ikuaiSuccess(list) {
		return fmt.Errorf("获取端口映射列表失败: %s", ikuaiErrMsg(list))
	}

	id := findDNATID(list, lanPort, lanAddr)
	if id == "" {
		return fmt.Errorf("未找到匹配的端口映射: %s:%d", lanAddr, lanPort)
	}

	result, err := c.call("dnat", "del", func(ver int) map[string]interface{} {
		return map[string]interface{}{"id": id}
	})
	if err != nil {
		return err
	}
	if !ikuaiSuccess(result) {
		return fmt.Errorf("删除端口映射失败: %s", ikuaiErrMsg(result))
	}
	return nil
}

// ============================================================
// DHCP 静态地址绑定（dhcp_static）
// ============================================================

// addDHCPStatic 添加 DHCP 静态地址绑定
func (c *ikuaiClient) addDHCPStatic(ip, mac, comment, dns1, dns2 string) error {
	result, err := c.call("dhcp_static", "add", func(ver int) map[string]interface{} {
		return dhcpStaticParam(ver, "add", ip, mac, comment, dns1, dns2)
	})
	if err != nil {
		return err
	}
	if !ikuaiSuccess(result) {
		return fmt.Errorf("添加DHCP静态地址失败: %s", ikuaiErrMsg(result))
	}
	return nil
}

// deleteDHCPStatic 删除匹配 ip/mac 的 DHCP 静态地址；不存在时返回 errIKuaiNotFound。
func (c *ikuaiClient) deleteDHCPStatic(ip, mac string) error {
	id, err := c.findDHCPStaticID(ip, mac)
	if err != nil {
		return err
	}
	if id == "" {
		return errIKuaiNotFound
	}
	result, err := c.call("dhcp_static", "del", func(ver int) map[string]interface{} {
		return map[string]interface{}{"id": id}
	})
	if err != nil {
		return err
	}
	if !ikuaiSuccess(result) {
		return fmt.Errorf("删除DHCP静态地址失败: %s", ikuaiErrMsg(result))
	}
	return nil
}

func (c *ikuaiClient) findDHCPStaticID(ip, mac string) (string, error) {
	list, err := c.call("dhcp_static", "show", func(ver int) map[string]interface{} {
		return dhcpStaticParam(ver, "show", "", "", "", "", "")
	})
	if err != nil {
		return "", err
	}
	if !ikuaiSuccess(list) {
		return "", fmt.Errorf("获取DHCP静态地址列表失败: %s", ikuaiErrMsg(list))
	}
	return findRowID(ikuaiRows(list), ip, mac), nil
}

// dhcpStaticParam 构造 dhcp_static 的请求参数。
// 爱快 3 依赖 newRow / gateway="auto"；爱快 4 改用 tagname 标识行，
// 并把网关留空交由系统自动填充。
func dhcpStaticParam(ver int, action, ip, mac, comment, dns1, dns2 string) map[string]interface{} {
	if action == "show" {
		// 注意：DHCP 静态地址表必须用 static_total,static_data；
		// 爱快4 上改用 total,data 会命中另一张空表，导致查不到已有绑定。
		return map[string]interface{}{"TYPE": "static_total,static_data", "ORDER_BY": "", "ORDER": ""}
	}
	if ver == ikuaiVer4 {
		if comment == "" {
			comment = ip
		}
		return map[string]interface{}{
			"tagname":   ikuaiTag(comment),
			"interface": "auto",
			"ip_addr":   ip,
			"mac":       mac,
			"gateway":   "",
			"dns1":      dns1,
			"dns2":      dns2,
			"comment":   comment,
			"enabled":   "yes",
			"hostname":  "",
		}
	}
	return map[string]interface{}{
		"newRow":    true,
		"hostname":  "",
		"ip_addr":   ip,
		"mac":       mac,
		"gateway":   "auto",
		"interface": "auto",
		"dns1":      dns1,
		"dns2":      dns2,
		"comment":   comment,
		"enabled":   "yes",
	}
}

// ============================================================
// ARP 绑定（arp）
// ============================================================

// addARP 添加 ARP 绑定
func (c *ikuaiClient) addARP(ip, mac, comment string) error {
	result, err := c.call("arp", "add", func(ver int) map[string]interface{} {
		return arpParam(ver, "add", ip, mac, comment)
	})
	if err != nil {
		return err
	}
	if !ikuaiSuccess(result) {
		return fmt.Errorf("添加ARP绑定失败: %s", ikuaiErrMsg(result))
	}
	return nil
}

// deleteARP 删除匹配 ip/mac 的 ARP 绑定；不存在时返回 errIKuaiNotFound。
//
// 与 Python 版 del_arps 一致：删除时回传列表行内的 IP，
// 而不是调用方传入的 IP（仅按 MAC 解绑时传入的 IP 为空）。
func (c *ikuaiClient) deleteARP(ip, mac string) error {
	row, err := c.findARPRow(ip, mac)
	if err != nil {
		return err
	}
	if row == nil {
		return errIKuaiNotFound
	}

	rowIP := strVal(row["ip_addr"])
	result, err := c.call("arp", "del", func(ver int) map[string]interface{} {
		return map[string]interface{}{
			"id":      ikuaiRowID(row),
			"ip_addr": rowIP,
		}
	})
	if err != nil {
		return err
	}
	if !ikuaiSuccess(result) {
		return fmt.Errorf("删除ARP绑定失败: %s", ikuaiErrMsg(result))
	}
	return nil
}

func (c *ikuaiClient) findARPRow(ip, mac string) (map[string]interface{}, error) {
	list, err := c.call("arp", "show", func(ver int) map[string]interface{} {
		return arpParam(ver, "show", "", "", "")
	})
	if err != nil {
		return nil, err
	}
	if !ikuaiSuccess(list) {
		return nil, fmt.Errorf("获取ARP列表失败: %s", ikuaiErrMsg(list))
	}
	return findRow(ikuaiRows(list), ip, mac), nil
}

// arpParam 构造 arp 的请求参数。
// 爱快 4 需要 tagname / termname，且 bind_type 为字符串。
func arpParam(ver int, action, ip, mac, comment string) map[string]interface{} {
	if action == "show" {
		return map[string]interface{}{
			"TYPE":      "total,data",
			"ORDER_BY":  "ip_addr_int",
			"orderType": "IP",
			"ORDER":     "asc",
		}
	}
	if ver == ikuaiVer4 {
		if comment == "" {
			comment = ip
		}
		tag := ikuaiTag(comment)
		return map[string]interface{}{
			"tagname":     tag,
			"termname":    tag,
			"ip_addr":     ip,
			"mac":         mac,
			"interface":   "lan1",
			"bind_type":   "0",
			"comment":     comment,
			"old_ip_addr": "",
		}
	}
	return map[string]interface{}{
		"bind_type":   0,
		"interface":   "lan1",
		"ip_addr":     ip,
		"mac":         mac,
		"comment":     comment,
		"old_ip_addr": "",
	}
}

// ============================================================
// 响应解析辅助
// ============================================================

// findRow 在表格数据里按 ip/mac 查找条目。
//
// 匹配语义与 Python 版一致（del_dhcp / del_arps 用 ip 或 mac 命中一行），
// 但多了一层严格匹配优先：提供的字段全部一致的行优先于仅部分命中的行。
// 静态绑定表里的 IP / MAC 都可能被管理员手工改动，宽松兜底能提高解绑成功率。
func findRow(rows []interface{}, ip, mac string) map[string]interface{} {
	if ip == "" && mac == "" {
		return nil
	}

	var strict, loose map[string]interface{}
	for _, it := range rows {
		row, ok := it.(map[string]interface{})
		if !ok || ikuaiRowID(row) == "" {
			continue
		}
		ipMatch := ip != "" && strVal(row["ip_addr"]) == ip
		macMatch := mac != "" && strings.EqualFold(strVal(row["mac"]), mac)

		if strict == nil && (ip == "" || ipMatch) && (mac == "" || macMatch) {
			strict = row
		}
		if loose == nil && (ipMatch || macMatch) {
			loose = row
		}
	}
	if strict != nil {
		return strict
	}
	return loose
}

// findRowID 在表格数据里按 ip/mac 查找条目 id
func findRowID(rows []interface{}, ip, mac string) string {
	return ikuaiRowID(findRow(rows, ip, mac))
}

// findDNATID 在 dnat 列表中按内网端口 / 地址查找条目 id
func findDNATID(resp map[string]interface{}, lanPort int, lanAddr string) string {
	wantPort := ""
	if lanPort > 0 {
		wantPort = strconv.Itoa(lanPort)
	}

	for _, it := range ikuaiRows(resp) {
		item, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		itemPort := strVal(item["lan_port"])
		itemAddr := strVal(item["lan_addr"])

		portMatch := wantPort != "" && itemPort == wantPort
		addrMatch := lanAddr != "" && itemAddr == lanAddr

		// 同时提供地址与端口时要求都匹配；只提供一个时按提供的匹配
		if lanAddr != "" && wantPort != "" {
			if portMatch && addrMatch {
				return ikuaiRowID(item)
			}
			continue
		}
		if portMatch || addrMatch {
			return ikuaiRowID(item)
		}
	}
	return ""
}

// ikuaiRowID 从表格行中提取主键（爱快3 为 id，爱快4 为 rowid）
func ikuaiRowID(row map[string]interface{}) string {
	for _, key := range []string{"id", "rowid", "row_id"} {
		if v := strVal(row[key]); v != "" {
			return v
		}
	}
	return ""
}

// ikuaiSuccess 判断爱快响应是否成功：
// 爱快 3 用 ErrMsg="Success" / Result=10000，爱快 4 用 code=0。
func ikuaiSuccess(resp map[string]interface{}) bool {
	if resp == nil {
		return false
	}
	// 爱快 4 的 code 字段优先级最高：存在时即为权威结果
	if v, ok := resp["code"]; ok {
		return toInt(v) == 0
	}
	if strings.EqualFold(strVal(resp["ErrMsg"]), "Success") {
		return true
	}
	if strings.EqualFold(strVal(resp["errmsg"]), "Success") {
		return true
	}
	if toInt(resp["Result"]) == 10000 {
		return true
	}
	if b, ok := resp["success"].(bool); ok {
		return b
	}
	return false
}

// ikuaiErrMsg 提取错误信息（兼容爱快3 的 ErrMsg 与爱快4 的 message/code）。
//
// 爱快4 会把具体原因放在单独的 errors 字段里，例如：
//
//	{"code":3001,"message":"请求参数不合法","errors":"参数错误: lan_addr"}
//
// 只取 message 会退化成没有信息量的「请求参数不合法」，因此一并带上 errors/details。
func ikuaiErrMsg(resp map[string]interface{}) string {
	if resp == nil {
		return "无响应"
	}

	msg := ""
	for _, key := range []string{"ErrMsg", "errmsg", "message", "msg"} {
		if v := strVal(resp[key]); v != "" {
			msg = v
			break
		}
	}
	for _, key := range []string{"errors", "details"} {
		// 只接受字符串形态：errors 是描述文本，details 往往是数组，
		// 直接 strVal 会得到 "[]" 之类的噪声。
		detail, ok := resp[key].(string)
		if !ok {
			continue
		}
		detail = strings.TrimSpace(detail)
		if detail == "" || strings.Contains(msg, detail) {
			continue
		}
		if msg == "" {
			msg = detail
		} else {
			msg = fmt.Sprintf("%s（%s）", msg, detail)
		}
	}

	if msg == "" {
		if v, ok := resp["code"]; ok {
			return fmt.Sprintf("错误码 %s（无错误描述）", strVal(v))
		}
		return "未知错误"
	}
	return msg
}

// isSessionExpired 判断响应是否表示会话失效。
//
// 必须先排除成功响应：成功文案 "Success" 本身包含 "sess"，
// 原实现会把每次成功调用都误判为会话失效，导致重复登录。
func isSessionExpired(resp map[string]interface{}) bool {
	if resp == nil || ikuaiSuccess(resp) {
		return false
	}
	msg := strings.ToLower(ikuaiErrMsg(resp))
	for _, kw := range []string{"login", "登录", "session", "unauthorized", "未认证", "未登录"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// ikuaiRows 从响应中取出表格数据行。
//
// 实测两种形态：
//
//	爱快3：{"ErrMsg":"Success","Data":{"total":N,"data":[…]}}
//	爱快4：{"code":0,"message":"Success","results":{"total":N,"data":[…],"static_total":N,"static_data":[…]}}
//
// 注意爱快4 的容器键是 results（不是 data），漏掉会导致列表查询永远为空、
// 进而使「按列表查找再删除」的销账逻辑全部失效。
func ikuaiRows(resp map[string]interface{}) []interface{} {
	if resp == nil {
		return nil
	}
	for _, key := range []string{"results", "Results", "data", "Data"} {
		if rows, ok := resp[key].([]interface{}); ok {
			return rows
		}
	}

	var containers []map[string]interface{}
	for _, key := range []string{"results", "Results", "Data", "data"} {
		if m, ok := resp[key].(map[string]interface{}); ok {
			containers = append(containers, m)
		}
	}
	for _, c := range containers {
		for _, key := range []string{"data", "static_data", "arps", "list", "rows", "items"} {
			if rows, ok := c[key].([]interface{}); ok {
				return rows
			}
		}
	}
	return nil
}

// strVal 将接口值安全转为字符串（数字不带小数点）
func strVal(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case float64:
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case int:
		return strconv.Itoa(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case bool:
		if val {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", val)
	}
}
