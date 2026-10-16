// Package driver 青州云 驱动生命周期实现（HTTP API）
// 与 Python 版 HostServer/QingzhouYun.py 的协议保持一致：
//   - 基础地址 {server_addr}，路径统一为 {base}/api/{path}
//   - 认证：Authorization: Bearer <server_pass>
//   - 成功判定：code == 0
//   - vm_saving 的 key 为虚拟机名称，平台 ID 存于 ExtData["vmid"]，缺失时回退为名称
package driver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// qzBaseURL 返回青州云 API 基地址
func (d *QingzhouDriver) qzBaseURL() string {
	addr := d.Config.ServerAddr
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return strings.TrimRight(addr, "/")
	}
	return "http://" + strings.TrimRight(addr, "/")
}

// qzRequest 发送青州云 API 请求并做统一成功判定
func (d *QingzhouDriver) qzRequest(method, path string, body interface{}) (map[string]interface{}, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, d.qzBaseURL()+"/api/"+strings.TrimLeft(path, "/"), reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+d.Config.ServerPass)

	resp, err := SharedHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	out, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	// 与 Python _ok() 一致：仅 code == 0 视为成功
	if code := toIntAny(result["code"]); code != 0 {
		return result, fmt.Errorf("%s", qzMsg(result))
	}
	return result, nil
}

// platformID 获取虚拟机在平台上的 ID（缺失时回退为名称）
func (d *QingzhouDriver) platformID(vmUUID string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if cfg, ok := d.VmSaving[vmUUID]; ok && cfg.ExtData != nil {
		if v, exists := cfg.ExtData["vmid"]; exists {
			if id := strVal(v); id != "" {
				return id
			}
		}
	}
	return vmUUID
}

// setPlatformID 记录平台 ID
func (d *QingzhouDriver) setPlatformID(cfg *model.VMConfig, id string) {
	if cfg.ExtData == nil {
		cfg.ExtData = make(map[string]interface{})
	}
	cfg.ExtData["vmid"] = id
}

// qzPowerFlag 状态文本转枚举
func qzPowerFlag(status string) model.VMPowers {
	switch strings.ToLower(status) {
	case "running":
		return model.VMPowerStarted
	case "stopped":
		return model.VMPowerStopped
	case "suspended", "paused":
		return model.VMPowerSuspend
	case "creating":
		return model.VMPowerOnOpen
	case "reinstalling":
		return model.VMPowerOnStop
	default:
		return model.VMPowerUnknown
	}
}

// GetVMList 获取虚拟机列表
func (d *QingzhouDriver) GetVMList() (map[string]*model.VMConfig, error) {
	result := d.GetVMSaving()

	resp, err := d.qzRequest(http.MethodGet, "vm/list", nil)
	if err != nil {
		return result, nil
	}

	data := normalizeQzMap(resp["data"])
	items := normalizeQzList(data["list"])

	for _, item := range items {
		name := strVal(item["name"])
		if name == "" {
			continue
		}
		id := strVal(item["id"])
		status := strVal(item["status"])

		if cfg, ok := result[name]; ok {
			cfg.VmFlag = qzPowerFlag(status)
			if id != "" {
				d.setPlatformID(cfg, id)
			}
		} else {
			cfg := model.NewVMConfig()
			cfg.VmUUID = name
			cfg.OsName = strVal(item["os"])
			cfg.CpuNum = toIntAny(item["cpu"])
			cfg.MemNum = toIntAny(item["memory"])
			cfg.HddNum = toIntAny(item["disk"])
			cfg.VmFlag = qzPowerFlag(status)
			if id != "" {
				d.setPlatformID(cfg, id)
			}
			result[name] = cfg
		}
	}
	return result, nil
}

// ScanVMs 扫描并同步虚拟机列表
func (d *QingzhouDriver) ScanVMs() (*model.ZMessage, error) {
	vms, err := d.GetVMList()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMDetect", Message: err.Error()}, nil
	}

	prefix := d.Config.FilterName
	d.mu.Lock()
	added := 0
	for name, cfg := range vms {
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		if _, exists := d.VmSaving[name]; !exists {
			d.VmSaving[name] = cfg
			added++
		} else {
			d.VmSaving[name].VmFlag = cfg.VmFlag
		}
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDetect",
		Message: fmt.Sprintf("扫描完成：共 %d 个，新增 %d 个", len(vms), added)}, nil
}

// PowerControl 电源控制
func (d *QingzhouDriver) PowerControl(vmUUID string, action model.VMPowers) (*model.ZMessage, error) {
	var op string
	switch action {
	case model.VMPowerSStart:
		op = "start"
	case model.VMPowerSClose:
		op = "shutdown"
	case model.VMPowerHClose:
		op = "stop"
	case model.VMPowerSReset:
		op = "reboot"
	case model.VMPowerHReset:
		op = "reset"
	case model.VMPowerAPause:
		op = "suspend"
	case model.VMPowerAWaked:
		op = "resume"
	default:
		return d.BaseDriver.PowerControl(vmUUID, action)
	}

	pid := d.platformID(vmUUID)
	if _, err := d.qzRequest(http.MethodPost,
		fmt.Sprintf("vm/%s/power", pid), map[string]interface{}{"action": op}); err != nil {
		return &model.ZMessage{Success: false, Action: "VMPowers",
			Message: "电源操作失败: " + err.Error()}, nil
	}

	if state, ok := PowerStatusMap[d.GetPower(vmUUID)]; ok {
		d.mu.Lock()
		if cfg, exists := d.VmSaving[vmUUID]; exists {
			cfg.VmFlag = state
		}
		d.mu.Unlock()
	}
	return &model.ZMessage{Success: true, Action: "VMPowers", Message: "电源操作已执行"}, nil
}

// CreateVM 创建虚拟机
func (d *QingzhouDriver) CreateVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	osImage := cfg.OsName
	for _, item := range d.Config.SystemMaps {
		if item.SysName == cfg.OsName && item.SysFile != "" {
			osImage = item.SysFile
			break
		}
	}
	password := cfg.OsPass
	if password == "" {
		password = "Qz@123456"
	}

	payload := map[string]interface{}{
		"name":           cfg.VmUUID,
		"cpu":            cfg.CpuNum,
		"memory":         cfg.MemNum,
		"disk":           cfg.HddNum,
		"os_image":       osImage,
		"password":       password,
		"bandwidth_up":   cfg.SpeedU,
		"bandwidth_down": cfg.SpeedD,
		"traffic_limit":  cfg.FluNum,
	}

	resp, err := d.qzRequest(http.MethodPost, "vm/create", payload)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: "创建失败: " + err.Error()}, nil
	}

	data := normalizeQzMap(resp["data"])
	if id := strVal(data["id"]); id != "" {
		d.setPlatformID(cfg, id)
	}

	cfg.VmFlag = model.VMPowerOnOpen
	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMCreate", Message: "虚拟机创建成功"}, nil
}

// DeleteVM 删除虚拟机
func (d *QingzhouDriver) DeleteVM(vmUUID string, removeBackup bool) (*model.ZMessage, error) {
	pid := d.platformID(vmUUID)
	if _, err := d.qzRequest(http.MethodDelete, fmt.Sprintf("vm/%s", pid), nil); err != nil {
		return &model.ZMessage{Success: false, Action: "VMDelete",
			Message: "删除失败: " + err.Error()}, nil
	}

	d.mu.Lock()
	delete(d.VmSaving, vmUUID)
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDelete", Message: "虚拟机删除成功"}, nil
}

// UpdateVM 更新虚拟机配置
func (d *QingzhouDriver) UpdateVM(cfg *model.VMConfig, oldCfg *model.VMConfig) (*model.ZMessage, error) {
	up := make(map[string]interface{})
	if cfg.CpuNum != oldCfg.CpuNum {
		up["cpu"] = cfg.CpuNum
	}
	if cfg.MemNum != oldCfg.MemNum {
		up["memory"] = cfg.MemNum
	}
	if cfg.HddNum != oldCfg.HddNum {
		up["disk"] = cfg.HddNum
	}
	if cfg.SpeedU != oldCfg.SpeedU {
		up["bandwidth_up"] = cfg.SpeedU
	}
	if cfg.SpeedD != oldCfg.SpeedD {
		up["bandwidth_down"] = cfg.SpeedD
	}
	if cfg.FluNum != oldCfg.FluNum {
		up["traffic_limit"] = cfg.FluNum
	}

	if len(up) > 0 {
		pid := d.platformID(cfg.VmUUID)
		if _, err := d.qzRequest(http.MethodPut,
			fmt.Sprintf("vm/%s/config", pid), up); err != nil {
			return &model.ZMessage{Success: false, Action: "VMUpdate",
				Message: "配置更新失败: " + err.Error()}, nil
		}
	}

	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMUpdate", Message: "虚拟机配置更新成功"}, nil
}

// ReinstallVM 重装系统
func (d *QingzhouDriver) ReinstallVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	osImage := cfg.OsName
	for _, item := range d.Config.SystemMaps {
		if item.SysName == cfg.OsName && item.SysFile != "" {
			osImage = item.SysFile
			break
		}
	}
	password := cfg.OsPass
	if password == "" {
		password = "Qz@123456"
	}

	pid := d.platformID(cfg.VmUUID)
	if _, err := d.qzRequest(http.MethodPost, fmt.Sprintf("vm/%s/reinstall", pid),
		map[string]interface{}{"os_image": osImage, "password": password}); err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall",
			Message: "重装失败: " + err.Error()}, nil
	}
	return &model.ZMessage{Success: true, Action: "VMReinstall", Message: "系统重装成功"}, nil
}

// GetVNCInfo 获取 VNC 控制台信息
func (d *QingzhouDriver) GetVNCInfo(vmUUID string, ipAddr string) (*model.ZMessage, error) {
	pid := d.platformID(vmUUID)
	resp, err := d.qzRequest(http.MethodGet, fmt.Sprintf("vm/%s/vnc", pid), nil)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VCRemote",
			Message: "获取VNC信息失败: " + err.Error()}, nil
	}
	data := normalizeQzMap(resp["data"])
	host := strVal(data["host"])
	if host == "" {
		host = d.Config.ServerAddr
	}
	return &model.ZMessage{Success: true, Action: "VCRemote", Message: "VNC信息获取成功",
		Results: map[string]interface{}{
			"type":     "vnc",
			"host":     host,
			"port":     toIntAny(data["port"]),
			"password": strVal(data["password"]),
		}}, nil
}

// CreateBackup 创建备份
func (d *QingzhouDriver) CreateBackup(vmUUID string, tips string) (*model.ZMessage, error) {
	name := fmt.Sprintf("backup-%s", time.Now().Format("20060102-150405"))
	pid := d.platformID(vmUUID)
	if _, err := d.qzRequest(http.MethodPost, fmt.Sprintf("vm/%s/backup", pid),
		map[string]interface{}{"name": name, "tips": tips}); err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: "创建备份失败: " + err.Error()}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		cfg.Backups = append(cfg.Backups, model.VMBackup{
			BackupName: name, BackupTime: time.Now(), BackupTips: tips,
		})
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMBackup", Message: "备份创建成功",
		Results: map[string]interface{}{"backup_name": name}}, nil
}

// DeleteBackup 删除备份
func (d *QingzhouDriver) DeleteBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	pid := d.platformID(vmUUID)
	if _, err := d.qzRequest(http.MethodDelete,
		fmt.Sprintf("vm/%s/backup/%s", pid, backupName), nil); err != nil {
		return &model.ZMessage{Success: false, Action: "RMBackup",
			Message: "删除备份失败: " + err.Error()}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		kept := cfg.Backups[:0]
		for _, b := range cfg.Backups {
			if b.BackupName != backupName {
				kept = append(kept, b)
			}
		}
		cfg.Backups = kept
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "RMBackup", Message: "备份删除成功"}, nil
}

// SetPassword 设置虚拟机密码（POST vm/{pid}/password）
func (d *QingzhouDriver) SetPassword(vmUUID string, password string) (*model.ZMessage, error) {
	pid := d.platformID(vmUUID)
	if _, err := d.qzRequest(http.MethodPost,
		fmt.Sprintf("vm/%s/password", pid),
		map[string]interface{}{"password": password}); err != nil {
		return &model.ZMessage{Success: false, Action: "Password",
			Message: "修改密码失败: " + err.Error()}, nil
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		cfg.OsPass = password
	}
	d.mu.Unlock()
	return &model.ZMessage{Success: true, Action: "Password", Message: "密码修改成功"}, nil
}

// Crontab 定时任务：同步虚拟机状态
func (d *QingzhouDriver) Crontab() bool {
	vms, err := d.GetVMList()
	if err != nil {
		return false
	}
	d.mu.Lock()
	for name, cfg := range vms {
		if existing, ok := d.VmSaving[name]; ok {
			existing.VmFlag = cfg.VmFlag
		} else {
			d.VmSaving[name] = cfg
		}
	}
	d.mu.Unlock()
	return true
}

// ============================================================
// 端口映射 / 反向代理（青州云云端 API，对应 Python PortsMap / ProxyMap）
// ============================================================

// PortsMap 端口映射下发：POST nat/create、nat/delete
func (d *QingzhouDriver) PortsMap(nat *model.PortData, add bool) (*model.ZMessage, error) {
	if nat == nil {
		return &model.ZMessage{Success: false, Action: "PortsMap", Message: "参数为空"}, nil
	}

	if add {
		if nat.WanPort == 0 {
			nat.WanPort = d.allocWanPort()
		}
		if _, err := d.qzRequest(http.MethodPost, "nat/create", map[string]interface{}{
			"wan_port":    nat.WanPort,
			"lan_port":    nat.LanPort,
			"lan_addr":    nat.LanAddr,
			"protocol":    "tcp",
			"description": nat.NatTips,
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "PortsMap",
				Message: "端口添加失败: " + err.Error()}, nil
		}
		return &model.ZMessage{Success: true, Action: "PortsMap",
			Message: fmt.Sprintf("端口%d添加成功", nat.WanPort)}, nil
	}

	if _, err := d.qzRequest(http.MethodPost, "nat/delete", map[string]interface{}{
		"wan_port": nat.WanPort,
		"lan_addr": nat.LanAddr,
		"lan_port": nat.LanPort,
	}); err != nil {
		return &model.ZMessage{Success: false, Action: "PortsMap",
			Message: "端口删除失败: " + err.Error()}, nil
	}
	return &model.ZMessage{Success: true, Action: "PortsMap",
		Message: fmt.Sprintf("端口%d删除成功", nat.WanPort)}, nil
}

// allocWanPort 在 [ports_start, ports_close] 范围内分配未占用端口
func (d *QingzhouDriver) allocWanPort() int {
	low := d.Config.PortsStart
	if low == 0 {
		low = 10000
	}
	high := d.Config.PortsClose
	if high == 0 {
		high = 60000
	}
	if high <= low {
		high = low + 1
	}

	used := make(map[int]bool)
	d.mu.RLock()
	for _, cfg := range d.VmSaving {
		for _, n := range cfg.NatAll {
			used[n.WanPort] = true
		}
	}
	d.mu.RUnlock()

	for i := 0; i < 200; i++ {
		p := low + rand.Intn(high-low+1)
		if !used[p] {
			return p
		}
	}
	return low
}

// ProxyMap 反向代理下发：POST web/create、web/delete
func (d *QingzhouDriver) ProxyMap(vmUUID string, proxy model.WebProxy, add bool) (*model.ZMessage, error) {
	pid := d.platformID(vmUUID)

	if add {
		// 重复域名校验由 API 层统一负责（Go 侧数据变更在 service 层完成）
		if _, err := d.qzRequest(http.MethodPost, "web/create", map[string]interface{}{
			"vm_id":       pid,
			"domain":      proxy.WebAddr,
			"lan_addr":    proxy.LanAddr,
			"lan_port":    proxy.LanPort,
			"is_https":    proxy.IsHTTPS,
			"description": proxy.WebTips,
		}); err != nil {
			return &model.ZMessage{Success: false, Action: "ProxyMap",
				Message: "添加代理失败: " + err.Error()}, nil
		}
		return &model.ZMessage{Success: true, Action: "ProxyMap", Message: "代理添加成功"}, nil
	}

	if _, err := d.qzRequest(http.MethodPost, "web/delete", map[string]interface{}{
		"domain": proxy.WebAddr,
		"vm_id":  pid,
	}); err != nil {
		return &model.ZMessage{Success: false, Action: "ProxyMap",
			Message: "删除代理失败: " + err.Error()}, nil
	}
	return &model.ZMessage{Success: true, Action: "ProxyMap", Message: "代理删除成功"}, nil
}

// ============================================================
// 辅助函数
// ============================================================

// qzMsg 提取青州云 API 的错误信息
func qzMsg(resp map[string]interface{}) string {
	if resp == nil {
		return "无响应"
	}
	if msg := strVal(resp["msg"]); msg != "" {
		return msg
	}
	if msg := strVal(resp["message"]); msg != "" {
		return msg
	}
	return "未知错误"
}

// normalizeQzList 将 data 归一化为 map 列表
func normalizeQzList(data interface{}) []map[string]interface{} {
	switch v := data.(type) {
	case []interface{}:
		result := make([]map[string]interface{}, 0, len(v))
		for _, it := range v {
			if m, ok := it.(map[string]interface{}); ok {
				result = append(result, m)
			}
		}
		return result
	case map[string]interface{}:
		return []map[string]interface{}{v}
	default:
		return nil
	}
}

// normalizeQzMap 将 data 归一化为单个 map
func normalizeQzMap(data interface{}) map[string]interface{} {
	if m, ok := data.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

// strVal 将接口值安全转为字符串（整数不带小数点）
func strVal(v interface{}) string {
	switch val := v.(type) {
	case nil:
		return ""
	case string:
		return val
	case float64:
		if val == float64(int64(val)) {
			return fmt.Sprintf("%d", int64(val))
		}
		return fmt.Sprintf("%v", val)
	case int:
		return fmt.Sprintf("%d", val)
	case int64:
		return fmt.Sprintf("%d", val)
	case bool:
		if val {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", val)
	}
}

// toIntAny 将接口值安全转为 int
func toIntAny(v interface{}) int {
	switch val := v.(type) {
	case int:
		return val
	case int64:
		return int(val)
	case float64:
		return int(val)
	case string:
		n := 0
		for _, c := range val {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
			}
		}
		return n
	default:
		return 0
	}
}
