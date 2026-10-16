// Package driver VMware Workstation 驱动生命周期实现（vmrest REST API）
// 与 Python 版 HostServer/Workstation.py + WorkstationAPI/VMWRestAPI.py 对齐：
//   - 基础地址 http://{server_addr}:{server_port}(默认8697)，路径前缀 /api
//   - 媒体类型 application/vnd.vmware.vmw.rest-v1+json
//   - 虚拟机标识为 vmrest 内部 id，需通过 GET /api/vms 的 path 反查（对应 select_vid）
//   - 创建流程：生成 VMX 文件 → POST /api/vms/registration 注册
//   - 电源：PUT /api/vms/{id}/power，请求体为纯字符串
package driver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

const vmrestContentType = "application/vnd.vmware.vmw.rest-v1+json"

// wsBaseURL 返回 vmrest 服务基地址
func (d *WorkstationDriver) wsBaseURL() string {
	addr := d.Config.ServerAddr
	port := d.Config.ServerPort
	if port <= 0 {
		port = 8697 // vmrest 默认端口
	}
	if strings.Contains(addr, ":") {
		return "http://" + addr
	}
	return fmt.Sprintf("http://%s:%d", addr, port)
}

// wsRequest 发送 vmrest 请求（返回响应体与状态码）
func (d *WorkstationDriver) wsRequest(method, path string, body interface{}) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, d.wsBaseURL()+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", vmrestContentType)
	if d.Config.ServerUser != "" {
		req.SetBasicAuth(d.Config.ServerUser, d.Config.ServerPass)
	}

	resp, err := SharedHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return out, resp.StatusCode, nil
}

// wsVM vmrest 虚拟机条目
type wsVM struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	VMX   string `json:"vmx"`
	Index int    `json:"index"`
}

// selectVid 根据虚拟机名反查 vmrest 内部 id（对应 Python select_vid）
func (d *WorkstationDriver) selectVid(vmName string) string {
	// 优先使用缓存
	d.mu.RLock()
	if cfg, ok := d.VmSaving[vmName]; ok && cfg.ExtData != nil {
		if v, exists := cfg.ExtData["vmid"]; exists {
			if id := strVal(v); id != "" {
				return id
			}
		}
	}
	d.mu.RUnlock()

	out, code, err := d.wsRequest(http.MethodGet, "/api/vms", nil)
	if err != nil || code != http.StatusOK {
		return ""
	}
	var list []wsVM
	if err := json.Unmarshal(out, &list); err != nil {
		return ""
	}
	for _, vm := range list {
		if vm.Path != "" && strings.Contains(vm.Path, vmName) {
			return vm.ID
		}
		base := strings.TrimSuffix(filepath.Base(vm.Path), filepath.Ext(vm.Path))
		if base == vmName {
			return vm.ID
		}
	}
	return ""
}

// setVid 缓存 vmrest 内部 id
func (d *WorkstationDriver) setVid(cfg *model.VMConfig, id string) {
	if cfg.ExtData == nil {
		cfg.ExtData = make(map[string]interface{})
	}
	cfg.ExtData["vmid"] = id
}

// GetVMList 获取虚拟机列表
func (d *WorkstationDriver) GetVMList() (map[string]*model.VMConfig, error) {
	result := d.GetVMSaving()

	out, code, err := d.wsRequest(http.MethodGet, "/api/vms", nil)
	if err != nil || code != http.StatusOK {
		return result, nil
	}

	var list []wsVM
	if err := json.Unmarshal(out, &list); err != nil {
		return result, nil
	}

	for _, item := range list {
		if item.ID == "" {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(item.Path), filepath.Ext(item.Path))
		if name == "" {
			name = item.ID
		}

		var flag model.VMPowers = model.VMPowerUnknown
		if s, ok := PowerStatusMap[d.wsGetPower(item.ID)]; ok {
			flag = s
		}

		if cfg, ok := result[name]; ok {
			cfg.VmFlag = flag
			d.setVid(cfg, item.ID)
		} else {
			cfg := model.NewVMConfig()
			cfg.VmUUID = name
			cfg.OsName = name
			cfg.VmFlag = flag
			d.setVid(cfg, item.ID)
			result[name] = cfg
		}
	}
	return result, nil
}

// wsGetPower 通过 vmrest 查询电源状态
func (d *WorkstationDriver) wsGetPower(vmID string) string {
	out, code, err := d.wsRequest(http.MethodGet, fmt.Sprintf("/api/vms/%s/power", vmID), nil)
	if err != nil || code != http.StatusOK {
		return "未知"
	}
	state := strings.Trim(strings.TrimSpace(string(out)), "\"")
	switch strings.ToLower(state) {
	case "poweredon", "on":
		return "运行中"
	case "poweredoff", "off":
		return "已关机"
	case "suspended", "paused":
		return "已暂停"
	default:
		return "未知"
	}
}

// ScanVMs 扫描并同步虚拟机列表
//
// 与 Python 版 Workstation.VMDetect 对齐：使用主机配置的 filter_name
// 作为名称前缀过滤，只纳管带该前缀的虚拟机。
func (d *WorkstationDriver) ScanVMs() (*model.ZMessage, error) {
	vms, err := d.GetVMList()
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMDetect", Message: err.Error()}, nil
	}

	// 前缀过滤（未配置 filter_name 时不过滤）
	prefix := d.Config.FilterName
	d.mu.Lock()
	scanned := 0
	added := 0
	for name, cfg := range vms {
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		scanned++
		if _, exists := d.VmSaving[name]; !exists {
			d.VmSaving[name] = cfg
			added++
		} else {
			d.VmSaving[name].VmFlag = cfg.VmFlag
			d.setVid(d.VmSaving[name], strVal(cfg.ExtData["vmid"]))
		}
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDetect",
		Message: fmt.Sprintf("扫描完成：匹配 %d 个，新增 %d 个", scanned, added)}, nil
}

// PowerControl 电源控制
// 与 Python powers_set 对齐：PUT /api/vms/{id}/power，body 为纯字符串
func (d *WorkstationDriver) PowerControl(vmUUID string, action model.VMPowers) (*model.ZMessage, error) {
	var op string
	switch action {
	case model.VMPowerSStart:
		op = "on"
	case model.VMPowerSClose:
		op = "shutdown"
	case model.VMPowerHClose:
		op = "off"
	case model.VMPowerSReset:
		op = "reset"
	case model.VMPowerHReset:
		op = "reset"
	case model.VMPowerAPause:
		op = "pause"
	case model.VMPowerAWaked:
		op = "unpause"
	default:
		return d.BaseDriver.PowerControl(vmUUID, action)
	}

	vmID := d.selectVid(vmUUID)
	if vmID == "" {
		return &model.ZMessage{Success: false, Action: "VMPowers",
			Message: fmt.Sprintf("未找到虚拟机: %s", vmUUID)}, nil
	}

	req, err := http.NewRequest(http.MethodPut,
		fmt.Sprintf("%s/api/vms/%s/power", d.wsBaseURL(), vmID),
		bytes.NewBufferString(op))
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMPowers", Message: err.Error()}, nil
	}
	req.Header.Set("Content-Type", vmrestContentType)
	if d.Config.ServerUser != "" {
		req.SetBasicAuth(d.Config.ServerUser, d.Config.ServerPass)
	}
	resp, err := SharedHTTPClient.Do(req)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMPowers",
			Message: "电源操作失败: " + err.Error()}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return &model.ZMessage{Success: false, Action: "VMPowers",
			Message: fmt.Sprintf("电源操作失败: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))}, nil
	}

	if s, ok := PowerStatusMap[d.wsGetPower(vmID)]; ok {
		d.mu.Lock()
		if cfg, exists := d.VmSaving[vmUUID]; exists {
			cfg.VmFlag = s
		}
		d.mu.Unlock()
	}
	return &model.ZMessage{Success: true, Action: "VMPowers", Message: "电源操作已执行"}, nil
}

// CreateVM 创建虚拟机（生成 VMX → 注册到 vmrest）
func (d *WorkstationDriver) CreateVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	dir := d.Config.SystemPath
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: "创建目录失败: " + err.Error()}, nil
	}
	vmxPath := filepath.Join(dir, cfg.VmUUID+".vmx")

	if err := os.WriteFile(vmxPath, []byte(d.buildVMX(cfg)), 0644); err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: "写入VMX失败: " + err.Error()}, nil
	}

	out, code, err := d.wsRequest(http.MethodPost, "/api/vms/registration",
		map[string]interface{}{"name": cfg.VmUUID, "path": vmxPath})
	if err != nil || (code != http.StatusOK && code != http.StatusCreated) {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: fmt.Sprintf("注册虚拟机失败: %v (HTTP %d, %s)", err, code, strings.TrimSpace(string(out)))}, nil
	}

	cfg.VmFlag = model.VMPowerStopped
	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	// 注册后回填 vmrest id
	if id := d.selectVid(cfg.VmUUID); id != "" {
		d.mu.Lock()
		d.setVid(cfg, id)
		d.mu.Unlock()
	}

	return &model.ZMessage{Success: true, Action: "VMCreate", Message: "虚拟机创建成功"}, nil
}

// buildVMX 生成最小可用的 VMX 配置
func (d *WorkstationDriver) buildVMX(cfg *model.VMConfig) string {
	mem := cfg.MemNum
	if mem <= 0 {
		mem = 2048
	}
	cpu := cfg.CpuNum
	if cpu <= 0 {
		cpu = 2
	}
	guestOS := d.guestOS(cfg.OsName)
	vncPort := cfg.VcPort
	if vncPort == "" {
		vncPort = RandomVNCPort()
	}

	return fmt.Sprintf(`.encoding = "UTF-8"
config.version = "8"
virtualHW.version = "19"
displayName = "%s"
memsize = "%d"
numvcpus = "%d"
guestOS = "%s"
scsi0.present = "TRUE"
scsi0.virtualDev = "lsilogic"
scsi0:0.present = "TRUE"
scsi0:0.fileName = "%s.vmdk"
scsi0:0.deviceType = "scsi-hardDisk"
ethernet0.present = "TRUE"
ethernet0.connectionType = "nat"
ethernet0.virtualDev = "e1000"
ethernet0.startConnected = "TRUE"
floppy0.present = "FALSE"
RemoteDisplay.vnc.enabled = "TRUE"
RemoteDisplay.vnc.port = "%s"
`, cfg.VmUUID, mem, cpu, guestOS, cfg.VmUUID, vncPort)
}

// guestOS 将系统名映射为 VMware guestOS 标识（与 Python create_vmx 的映射思路一致）
func (d *WorkstationDriver) guestOS(osName string) string {
	name := strings.ToLower(strings.TrimSpace(osName))
	switch {
	case strings.Contains(name, "win11"), strings.Contains(name, "windows11"):
		return "windows10-64"
	case strings.Contains(name, "win10"), strings.Contains(name, "windows10"):
		return "windows9-64"
	case strings.Contains(name, "2022"):
		return "windows9Server-64"
	case strings.Contains(name, "2019"):
		return "windows8Server-64"
	case strings.Contains(name, "win"):
		return "windows9-64"
	default:
		return "otherlinux-64"
	}
}

// DeleteVM 删除虚拟机
func (d *WorkstationDriver) DeleteVM(vmUUID string, removeBackup bool) (*model.ZMessage, error) {
	vmID := d.selectVid(vmUUID)
	if vmID == "" {
		return &model.ZMessage{Success: false, Action: "VMDelete",
			Message: fmt.Sprintf("未找到虚拟机: %s", vmUUID)}, nil
	}

	out, code, err := d.wsRequest(http.MethodDelete, "/api/vms/"+vmID, nil)
	if err != nil || (code != http.StatusOK && code != http.StatusNoContent) {
		return &model.ZMessage{Success: false, Action: "VMDelete",
			Message: fmt.Sprintf("删除失败: %v (HTTP %d, %s)", err, code, strings.TrimSpace(string(out)))}, nil
	}

	d.mu.Lock()
	delete(d.VmSaving, vmUUID)
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDelete", Message: "虚拟机删除成功"}, nil
}

// UpdateVM 更新虚拟机配置（PUT /api/vms/{id}）
func (d *WorkstationDriver) UpdateVM(cfg *model.VMConfig, oldCfg *model.VMConfig) (*model.ZMessage, error) {
	vmID := d.selectVid(cfg.VmUUID)
	if vmID != "" {
		payload := make(map[string]interface{})
		if cfg.CpuNum != oldCfg.CpuNum {
			payload["numvcpus"] = cfg.CpuNum
		}
		if cfg.MemNum != oldCfg.MemNum {
			payload["memsize"] = cfg.MemNum
		}
		if len(payload) > 0 {
			if out, code, err := d.wsRequest(http.MethodPut, "/api/vms/"+vmID, payload); err != nil ||
				(code != http.StatusOK && code != http.StatusNoContent) {
				return &model.ZMessage{Success: false, Action: "VMUpdate",
					Message: fmt.Sprintf("更新失败: %v (HTTP %d, %s)", err, code, strings.TrimSpace(string(out)))}, nil
			}
		}
	}

	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMUpdate", Message: "虚拟机配置更新成功"}, nil
}

// SetPassword 设置虚拟机密码
// vmrest 无改密端点，与 Python 基类 VMPasswd 一致：更新 os_pass 配置
func (d *WorkstationDriver) SetPassword(vmUUID string, password string) (*model.ZMessage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	cfg, ok := d.VmSaving[vmUUID]
	if !ok {
		return &model.ZMessage{Success: false, Action: "Password", Message: "虚拟机不存在"}, nil
	}
	cfg.OsPass = password
	return &model.ZMessage{Success: true, Action: "Password", Message: "密码已更新"}, nil
}

// GetVNCInfo 获取远程控制台信息（读取 VMX 中的 VNC 端口）
func (d *WorkstationDriver) GetVNCInfo(vmUUID string, ipAddr string) (*model.ZMessage, error) {
	host := d.Config.ServerAddr
	if host == "" {
		host = "127.0.0.1"
	}
	port := 5900
	d.mu.RLock()
	if cfg, ok := d.VmSaving[vmUUID]; ok && cfg.VcPort != "" {
		fmt.Sscanf(cfg.VcPort, "%d", &port)
	}
	d.mu.RUnlock()

	return &model.ZMessage{Success: true, Action: "VCRemote", Message: "控制台信息获取成功",
		Results: map[string]interface{}{
			"type": "vnc", "host": host, "port": port, "password": "",
		}}, nil
}

// Crontab 定时任务：同步虚拟机状态
func (d *WorkstationDriver) Crontab() bool {
	vms, err := d.GetVMList()
	if err != nil {
		return false
	}
	d.mu.Lock()
	for name, cfg := range vms {
		if existing, ok := d.VmSaving[name]; ok {
			existing.VmFlag = cfg.VmFlag
			if cfg.ExtData != nil {
				d.setVid(existing, strVal(cfg.ExtData["vmid"]))
			}
		} else {
			d.VmSaving[name] = cfg
		}
	}
	d.mu.Unlock()
	return true
}
