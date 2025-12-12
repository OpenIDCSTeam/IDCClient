package vmware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/models"
)

// VRestAPI VMware REST API客户端
type VRestAPI struct {
	HostAddr string
	HostUser string
	HostPass string
	HostPath string
	VerAgent int
	client   *http.Client
}

// NewVRestAPI 创建新的VRestAPI客户端
func NewVRestAPI(hostAddr, hostUser, hostPass, hostPath string, verAgent int) *VRestAPI {
	return &VRestAPI{
		HostAddr: hostAddr,
		HostUser: hostUser,
		HostPass: hostPass,
		HostPath: hostPath,
		VerAgent: verAgent,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// VMRestResponse VMRest API响应结构
type VMRestResponse struct {
	Success bool        `json:"success"`
	Message string      `json:"message"`
	Results interface{} `json:"results"`
}

// VMInfo 虚拟机信息
type VMInfo struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// VMRestAPI 发送VMRest API请求
func (v *VRestAPI) VMRestAPI(url string, data interface{}, method string) *VMRestResponse {
	fullURL := fmt.Sprintf("http://%s/api%s", v.HostAddr, url)
	
	var reqBody io.Reader
	if data != nil {
		jsonData, err := json.Marshal(data)
		if err != nil {
			return &VMRestResponse{
				Success: false,
				Message: fmt.Sprintf("JSON编码失败: %v", err),
			}
		}
		reqBody = bytes.NewBuffer(jsonData)
	}
	
	req, err := http.NewRequest(method, fullURL, reqBody)
	if err != nil {
		return &VMRestResponse{
			Success: false,
			Message: fmt.Sprintf("创建请求失败: %v", err),
		}
	}
	
	req.SetBasicAuth(v.HostUser, v.HostPass)
	req.Header.Set("Content-Type", "application/vnd.vmware.vmw.rest-v1+json")
	
	resp, err := v.client.Do(req)
	if err != nil {
		return &VMRestResponse{
			Success: false,
			Message: fmt.Sprintf("请求失败: %v", err),
		}
	}
	defer resp.Body.Close()
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &VMRestResponse{
			Success: false,
			Message: fmt.Sprintf("读取响应失败: %v", err),
		}
	}
	
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &VMRestResponse{
			Success: false,
			Message: fmt.Sprintf("HTTP错误 %d: %s", resp.StatusCode, string(body)),
		}
	}
	
	var result interface{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &result); err != nil {
			return &VMRestResponse{
				Success: false,
				Message: fmt.Sprintf("解析响应失败: %v", err),
			}
		}
	}
	
	return &VMRestResponse{
		Success: true,
		Message: "请求成功",
		Results: result,
	}
}

// PowersAPI 发送VMRest电源操作请求
func (v *VRestAPI) PowersAPI(url string, power string) *VMRestResponse {
	fullURL := fmt.Sprintf("http://%s/api%s", v.HostAddr, url)
	
	req, err := http.NewRequest("PUT", fullURL, strings.NewReader(power))
	if err != nil {
		return &VMRestResponse{
			Success: false,
			Message: fmt.Sprintf("创建请求失败: %v", err),
		}
	}
	
	req.SetBasicAuth(v.HostUser, v.HostPass)
	req.Header.Set("Content-Type", "application/vnd.vmware.vmw.rest-v1+json")
	
	resp, err := v.client.Do(req)
	if err != nil {
		return &VMRestResponse{
			Success: false,
			Message: fmt.Sprintf("请求失败: %v", err),
		}
	}
	defer resp.Body.Close()
	
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &VMRestResponse{
			Success: false,
			Message: fmt.Sprintf("读取响应失败: %v", err),
		}
	}
	
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &VMRestResponse{
			Success: false,
			Message: fmt.Sprintf("HTTP错误 %d: %s", resp.StatusCode, string(body)),
		}
	}
	
	var result interface{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &result); err != nil {
			return &VMRestResponse{
				Success: false,
				Message: fmt.Sprintf("解析响应失败: %v", err),
			}
		}
	}
	
	return &VMRestResponse{
		Success: true,
		Message: "电源操作成功",
		Results: result,
	}
}

// ReturnVMX 获取所有虚拟机列表
func (v *VRestAPI) ReturnVMX() *VMRestResponse {
	return v.VMRestAPI("/vms", nil, "GET")
}

// SelectVID 根据虚拟机名称获取虚拟机ID
func (v *VRestAPI) SelectVID(vmName string) string {
	result := v.ReturnVMX()
	if !result.Success {
		return ""
	}
	
	vms, ok := result.Results.([]interface{})
	if !ok {
		return ""
	}
	
	for _, vm := range vms {
		vmMap, ok := vm.(map[string]interface{})
		if !ok {
			continue
		}
		
		vmPath, _ := vmMap["path"].(string)
		vmID, _ := vmMap["id"].(string)
		
		// 方式1：直接匹配路径中的虚拟机名称
		if strings.Contains(vmPath, vmName) {
			return vmID
		}
		
		// 方式2：提取.vmx文件名进行匹配
		vmxName := strings.TrimSuffix(filepath.Base(vmPath), filepath.Ext(vmPath))
		if vmxName == vmName {
			return vmID
		}
	}
	
	return ""
}

// PowersGet 获取虚拟机电源状态
func (v *VRestAPI) PowersGet(vmName string) *VMRestResponse {
	vmID := v.SelectVID(vmName)
	if vmID == "" {
		return &VMRestResponse{
			Success: false,
			Message: fmt.Sprintf("未找到虚拟机: %s", vmName),
		}
	}
	return v.VMRestAPI(fmt.Sprintf("/vms/%s/power", vmID), nil, "GET")
}

// PowersSet 设置虚拟机电源状态
func (v *VRestAPI) PowersSet(vmName string, power models.VMPowerState, vmPassword string) *VMRestResponse {
	// 电源状态映射
	powerMap := map[models.VMPowerState]string{
		models.PowerOn:       "on",
		models.PowerOff:      "off",
		models.PowerShutdown: "shutdown",
		models.PowerSuspend:  "suspend",
		models.PowerPause:    "pause",
		models.PowerUnpause:  "unpause",
	}
	
	stateStr, ok := powerMap[power]
	if !ok {
		stateStr = "on"
	}
	
	vmID := v.SelectVID(vmName)
	if vmID == "" {
		return &VMRestResponse{
			Success: false,
			Message: fmt.Sprintf("未找到虚拟机: %s", vmName),
		}
	}
	
	url := fmt.Sprintf("/vms/%s/power", vmID)
	if vmPassword != "" {
		url += fmt.Sprintf("?vmPassword=%s", vmPassword)
	}
	
	return v.PowersAPI(url, stateStr)
}

// LoaderVMX 注册虚拟机到VMware Workstation
func (v *VRestAPI) LoaderVMX(vmxPath string, vmName string) *VMRestResponse {
	if vmName == "" {
		vmName = strings.TrimSuffix(filepath.Base(vmxPath), filepath.Ext(vmxPath))
	}
	
	data := map[string]string{
		"name": vmName,
		"path": vmxPath,
	}
	
	return v.VMRestAPI("/vms/registration", data, "POST")
}

// DeleteVMX 从VMware Workstation中删除虚拟机
func (v *VRestAPI) DeleteVMX(vmName string) *VMRestResponse {
	vmID := v.SelectVID(vmName)
	if vmID == "" {
		return &VMRestResponse{
			Success: false,
			Message: fmt.Sprintf("未找到虚拟机: %s", vmName),
		}
	}
	return v.VMRestAPI(fmt.Sprintf("/vms/%s", vmID), nil, "DELETE")
}

// ConfigGet 获取虚拟机配置信息
func (v *VRestAPI) ConfigGet(vmName string) *VMRestResponse {
	vmID := v.SelectVID(vmName)
	if vmID == "" {
		return &VMRestResponse{
			Success: false,
			Message: fmt.Sprintf("未找到虚拟机: %s", vmName),
		}
	}
	return v.VMRestAPI(fmt.Sprintf("/vms/%s", vmID), nil, "GET")
}

// ConfigSet 更新虚拟机配置
func (v *VRestAPI) ConfigSet(vmName string, config map[string]interface{}) *VMRestResponse {
	vmID := v.SelectVID(vmName)
	if vmID == "" {
		return &VMRestResponse{
			Success: false,
			Message: fmt.Sprintf("未找到虚拟机: %s", vmName),
		}
	}
	return v.VMRestAPI(fmt.Sprintf("/vms/%s", vmID), config, "PUT")
}

// ReturnNet 获取所有虚拟网络
func (v *VRestAPI) ReturnNet() *VMRestResponse {
	return v.VMRestAPI("/vmnet", nil, "GET")
}

// CreateTxt 创建vmx文本
func CreateTxt(config map[string]interface{}, prefix string) string {
	var result strings.Builder
	
	for key, value := range config {
		switch v := value.(type) {
		case map[string]interface{}:
			// 如果值是字典，递归处理
			newPrefix := key + "."
			if prefix != "" {
				newPrefix = prefix + key + "."
			}
			result.WriteString(CreateTxt(v, newPrefix))
		default:
			// 如果值不是字典，直接生成配置行
			fullKey := key
			if prefix != "" {
				fullKey = prefix + key
			}
			
			switch v := value.(type) {
			case string:
				result.WriteString(fmt.Sprintf("%s = \"%s\"\n", fullKey, v))
			default:
				result.WriteString(fmt.Sprintf("%s = %v\n", fullKey, v))
			}
		}
	}
	
	return result.String()
}

// CreateVMX 创建虚拟机VMX配置
func (v *VRestAPI) CreateVMX(vmConf *models.VMConfig, hsConfig *models.HSConfig) string {
	vmxConfig := map[string]interface{}{
		// 编码配置
		".encoding":                  "GBK",
		"config.version":             "8",
		"virtualHW.version":          fmt.Sprintf("%d", v.VerAgent),
		// 基本配置
		"displayName":                vmConf.VMUUID,
		"firmware":                   "efi",
		"guestOS":                    "windows9-64",
		// 硬件配置
		"numvcpus":                   fmt.Sprintf("%d", vmConf.CPUNum),
		"cpuid.coresPerSocket":       fmt.Sprintf("%d", vmConf.CPUNum),
		"memsize":                    fmt.Sprintf("%d", vmConf.MemNum),
		"mem.hotadd":                 "TRUE",
		"mks.enable3d":               "TRUE",
		"svga.graphicsMemoryKB":      fmt.Sprintf("%d", vmConf.GPUMem*1024),
		// 设备配置
		"vmci0.present":              "TRUE",
		"hpet0.present":              "TRUE",
		"usb.present":                "TRUE",
		"ehci.present":               "TRUE",
		"usb_xhci.present":           "TRUE",
		"tools.syncTime":             "TRUE",
		"nvram":                      vmConf.VMUUID + ".nvram",
		"virtualHW.productCompatibility": "hosted",
		"extendedConfigFile":         vmConf.VMUUID + ".vmxf",
		// PCI桥接配置
		"pciBridge0.present":         "TRUE",
		"pciBridge4.present":         "TRUE",
		"pciBridge4.virtualDev":      "pcieRootPort",
		"pciBridge4.functions":       "8",
		// 系统盘配置
		"nvme0.present":              "TRUE",
		"nvme0:0.fileName":           vmConf.VMUUID + ".vmdk",
		"nvme0:0.present":            "TRUE",
		// 远程显示配置
		"RemoteDisplay.vnc.enabled":  "TRUE",
		"RemoteDisplay.vnc.port":     vmConf.VCPort,
		"RemoteDisplay.vnc.password": vmConf.VCPass,
	}
	
	// 网卡配置
	nicUUID := 0
	for _, nicData := range vmConf.NicAll {
		useAuto := nicData.MacAddr == ""
		nicType := ""
		if nicData.NicType == "nat" {
			nicType = hsConfig.NetworkNAT
		} else if nicData.NicType == "pub" {
			nicType = hsConfig.NetworkPub
		}
		
		addressType := "generated"
		if !useAuto {
			addressType = "static"
		}
		
		vmxConfig[fmt.Sprintf("ethernet%d.connectionType", nicUUID)] = nicType
		vmxConfig[fmt.Sprintf("ethernet%d.addressType", nicUUID)] = addressType
		if !useAuto {
			vmxConfig[fmt.Sprintf("ethernet%d.address", nicUUID)] = nicData.MacAddr
		}
		vmxConfig[fmt.Sprintf("ethernet%d.virtualDev", nicUUID)] = "e1000e"
		vmxConfig[fmt.Sprintf("ethernet%d.present", nicUUID)] = "TRUE"
		vmxConfig[fmt.Sprintf("ethernet%d.txbw.limit", nicUUID)] = fmt.Sprintf("%d", vmConf.SpeedU*1024)
		vmxConfig[fmt.Sprintf("ethernet%d.rxbw.limit", nicUUID)] = fmt.Sprintf("%d", vmConf.SpeedD*1024)
		
		nicUUID++
	}
	
	// 数据磁盘
	hddUUID := 1
	for range vmConf.HddAll {
		vmxConfig[fmt.Sprintf("nvme0:%d.fileName", hddUUID)] = fmt.Sprintf("%s-%d.vmdk", vmConf.VMUUID, hddUUID)
		vmxConfig[fmt.Sprintf("nvme0:%d.present", hddUUID)] = "TRUE"
		hddUUID++
	}
	
	return CreateTxt(vmxConfig, "")
}

// UpdateVMX 更新VMX配置文件
func (v *VRestAPI) UpdateVMX(existingVMX string, vmConf *models.VMConfig, hsConfig *models.HSConfig) string {
	// 生成新的配置内容
	newVMXContent := v.CreateVMX(vmConf, hsConfig)
	newConfigLines := strings.Split(strings.TrimSpace(newVMXContent), "\n")
	
	// 解析现有VMX文件
	existingConfig := make(map[string]string)
	existingLines := strings.Split(strings.TrimSpace(existingVMX), "\n")
	
	for _, line := range existingLines {
		line = strings.TrimSpace(line)
		if line != "" && strings.Contains(line, "=") && !strings.HasPrefix(line, "#") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])
				existingConfig[key] = value
			}
		}
	}
	
	// 解析新配置并创建更新后的配置
	updatedConfig := make(map[string]string)
	for k, v := range existingConfig {
		updatedConfig[k] = v
	}
	
	for _, line := range newConfigLines {
		line = strings.TrimSpace(line)
		if line != "" && strings.Contains(line, "=") && !strings.HasPrefix(line, "#") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])
				updatedConfig[key] = value
			}
		}
	}
	
	// 重新构建VMX内容
	var resultLines []string
	processedKeys := make(map[string]bool)
	
	for _, line := range existingLines {
		line = strings.TrimSpace(line)
		if line != "" && strings.Contains(line, "=") && !strings.HasPrefix(line, "#") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				if value, ok := updatedConfig[key]; ok {
					resultLines = append(resultLines, fmt.Sprintf("%s = %s", key, value))
					processedKeys[key] = true
				}
			}
		} else {
			// 保留注释和空行
			resultLines = append(resultLines, line)
		}
	}
	
	// 添加新配置中不存在的项
	for key, value := range updatedConfig {
		if !processedKeys[key] {
			resultLines = append(resultLines, fmt.Sprintf("%s = %s", key, value))
		}
	}
	
	return strings.Join(resultLines, "\n") + "\n"
}

// ExtendHDD 扩展VM磁盘
func (v *VRestAPI) ExtendHDD(vmVMDK string, vmSize int) *VMRestResponse {
	vmDisk := filepath.Join(v.HostPath, "vmware-vdiskmanager.exe")
	
	cmd := exec.Command(vmDisk, "-x", fmt.Sprintf("%dMB", vmSize), vmVMDK)
	output, err := cmd.CombinedOutput()
	
	if err != nil {
		return &VMRestResponse{
			Success: false,
			Message: fmt.Sprintf("硬盘扩展失败: %s - %v", string(output), err),
		}
	}
	
	return &VMRestResponse{
		Success: true,
		Message: "硬盘扩展成功",
		Results: string(output),
	}
}
