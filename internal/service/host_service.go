// Package service 业务逻辑层 - 主机管理服务
package service

import (
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/auth"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/driver"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
)

// statusCacheTTL 主机状态缓存时间。
// 前端每 10 秒轮询一次状态；采集需要访问平台 API 或执行系统命令，
// 缓存 30 秒既保证展示接近实时，又避免重复采集（与 Python 版 get_host 的
// 30 秒新鲜度窗口一致）。
const statusCacheTTL = 30 * time.Second

// hostStatusCacheEntry 主机状态缓存条目
type hostStatusCacheEntry struct {
	status   *model.HWStatus
	cachedAt time.Time
}

// HostService 主机管理服务
type HostService struct {
	db      *data.DB
	drivers map[string]driver.HostDriver // hs_name -> driver
	mu      sync.RWMutex
	// initMu 串行化驱动的惰性创建，避免并发首次访问重复构造同一主机驱动
	initMu sync.Mutex
	// statusMu/statusCache 主机状态短缓存
	statusMu    sync.Mutex
	statusCache map[string]*hostStatusCacheEntry
}

// NewHostService 创建主机管理服务
func NewHostService(db *data.DB) *HostService {
	return &HostService{
		db:      db,
		drivers: make(map[string]driver.HostDriver),
	}
}

// ============================================================
// 主机配置操作
// ============================================================

// GetAllHosts 获取所有主机配置（返回map，key为hs_name）
func (s *HostService) GetAllHosts() (map[string]interface{}, error) {
	rows, err := s.db.GetAllHosts()
	if err != nil {
		return nil, fmt.Errorf("查询主机列表失败: %w", err)
	}

	result := make(map[string]interface{})
	for _, row := range rows {
		cfg, err := rowToHSConfig(row)
		if err != nil {
			continue
		}
		result[row.HsName] = cfg
	}
	return result, nil
}

// GetHost 获取单个主机配置
func (s *HostService) GetHost(hsName string) (*model.HSConfig, error) {
	row, err := s.db.GetHost(hsName)
	if err != nil {
		return nil, fmt.Errorf("查询主机失败: %w", err)
	}
	if row == nil {
		return nil, nil
	}
	return rowToHSConfig(row)
}

// CreateHost 创建主机
func (s *HostService) CreateHost(cfg *model.HSConfig) error {
	row, err := hsConfigToRow(cfg)
	if err != nil {
		return fmt.Errorf("序列化主机配置失败: %w", err)
	}
	if err := s.db.CreateHost(row); err != nil {
		return fmt.Errorf("创建主机失败: %w", err)
	}
	// 初始化驱动
	s.initDriver(cfg)
	return nil
}

// UpdateHost 更新主机配置
func (s *HostService) UpdateHost(cfg *model.HSConfig) error {
	row, err := hsConfigToRow(cfg)
	if err != nil {
		return fmt.Errorf("序列化主机配置失败: %w", err)
	}
	if err := s.db.UpdateHost(row); err != nil {
		return fmt.Errorf("更新主机失败: %w", err)
	}
	// 重新初始化驱动
	s.initDriver(cfg)
	return nil
}

// DeleteHost 删除主机
func (s *HostService) DeleteHost(hsName string) error {
	if err := s.db.DeleteHost(hsName); err != nil {
		return fmt.Errorf("删除主机失败: %w", err)
	}
	s.mu.Lock()
	delete(s.drivers, hsName)
	s.mu.Unlock()
	return nil
}

// SetHostEnable 设置主机启用状态
// 与 Python HostManager.pwr_host 一致：启用时加载驱动（HSLoader），禁用时卸载驱动（HSUnload）。
// 缺少这一步会出现「已启用但无法管理虚拟机」或「已禁用仍可管理」的现象。
func (s *HostService) SetHostEnable(hsName string, enable bool) error {
	if err := s.db.SetHostEnable(hsName, enable); err != nil {
		return err
	}

	if !enable {
		// 禁用：卸载驱动，后续请求将无法访问该主机
		s.mu.Lock()
		delete(s.drivers, hsName)
		s.mu.Unlock()
		return nil
	}

	// 启用：立即加载驱动，无需等待调度器扫描或手动调用 /api/system/loader
	row, err := s.db.GetHost(hsName)
	if err != nil || row == nil {
		return fmt.Errorf("主机 %s 不存在", hsName)
	}
	cfg, err := rowToHSConfig(row)
	if err != nil {
		return fmt.Errorf("解析主机配置失败: %w", err)
	}
	cfg.EnableHost = true
	s.initDriver(cfg)
	return nil
}

// GetHostStatus 获取主机状态（内部使用，返回原始结构）
func (s *HostService) GetHostStatus(hsName string) (interface{}, error) {
	d := s.getDriver(hsName)
	if d == nil {
		return nil, fmt.Errorf("主机 %s 未找到或未启用", hsName)
	}
	return d.GetStatus()
}

// GetHostStatusPayload 返回与 Python 版 get_host_status 一致的响应体。
//
// 前端读取的是 data.status（见 HostManage.tsx），因此必须包一层：
//
//	{status: {...}, source: "driver"|"cache"|"db"|"disabled", cached_at, age_seconds}
//
// 原实现直接返回 HWStatus，导致 data.status 恒为 undefined，
// 前端 hostsStatus 始终为空 —— 表现为「获取不到主机状态信息」。
func (s *HostService) GetHostStatusPayload(hsName string, refresh bool) (map[string]interface{}, error) {
	row, err := s.db.GetHost(hsName)
	if err != nil || row == nil {
		return nil, fmt.Errorf("主机 %s 不存在", hsName)
	}
	now := time.Now()

	// 禁用主机不采集状态
	if row.EnableHost == 0 {
		return map[string]interface{}{
			"status":      map[string]interface{}{},
			"source":      "disabled",
			"cached_at":   now.Unix(),
			"age_seconds": 0,
		}, nil
	}

	if !refresh {
		s.statusMu.Lock()
		entry := s.statusCache[hsName]
		s.statusMu.Unlock()
		if entry != nil && now.Sub(entry.cachedAt) < statusCacheTTL {
			return map[string]interface{}{
				"status":      entry.status,
				"source":      "cached",
				"cached_at":   entry.cachedAt.Unix(),
				"age_seconds": int(now.Sub(entry.cachedAt).Seconds()),
			}, nil
		}
	}

	d := s.GetOrCreateDriver(hsName)
	if d == nil {
		return s.statusFromDB(hsName, now), nil
	}

	hw, statusErr := d.GetStatus()
	if statusErr != nil || hw == nil {
		return s.statusFromDB(hsName, now), nil
	}
	if hw.OnUpdate == 0 {
		hw.OnUpdate = now.Unix()
	}

	// 仅在采集到有效数据时落库与缓存（全 0 不覆盖历史，与 Python 版一致）
	if hw.CpuTotal > 0 || hw.MemTotal > 0 {
		_ = s.db.AddHSStatus(hsName, hw)
		s.statusMu.Lock()
		if s.statusCache == nil {
			s.statusCache = make(map[string]*hostStatusCacheEntry)
		}
		s.statusCache[hsName] = &hostStatusCacheEntry{status: hw, cachedAt: now}
		s.statusMu.Unlock()
	}

	return map[string]interface{}{
		"status":      hw,
		"source":      "driver",
		"cached_at":   now.Unix(),
		"age_seconds": 0,
	}, nil
}

// statusFromDB 采集不可用时回退到最近一次落库的状态
func (s *HostService) statusFromDB(hsName string, now time.Time) map[string]interface{} {
	list, err := s.db.GetHSStatus(hsName, 1)
	if err != nil || len(list) == 0 {
		return map[string]interface{}{
			"status":      map[string]interface{}{},
			"source":      "error",
			"cached_at":   now.Unix(),
			"age_seconds": 0,
		}
	}
	var status interface{}
	if err := json.Unmarshal([]byte(list[0]), &status); err != nil || status == nil {
		status = map[string]interface{}{}
	}
	return map[string]interface{}{
		"status":      status,
		"source":      "db",
		"cached_at":   now.Unix(),
		"age_seconds": 0,
	}
}

// GetHostAreas 获取所有主机区域列表
func (s *HostService) GetHostAreas() ([]map[string]string, error) {
	rows, err := s.db.GetAllHosts()
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var areas []map[string]string
	for _, row := range rows {
		if row.ServerArea == "" {
			continue
		}
		if !seen[row.ServerArea] {
			seen[row.ServerArea] = true
			areas = append(areas, map[string]string{
				"area_code": row.ServerArea,
				"area_name": row.ServerArea,
			})
		}
	}
	return areas, nil
}

// GetHostPlans 获取主机套餐列表
func (s *HostService) GetHostPlans(hsName string) (map[string]interface{}, error) {
	row, err := s.db.GetHost(hsName)
	if err != nil || row == nil {
		return nil, fmt.Errorf("主机 %s 不存在", hsName)
	}
	var plans map[string]interface{}
	if err := json.Unmarshal([]byte(row.ServerPlan), &plans); err != nil {
		return make(map[string]interface{}), nil
	}
	return plans, nil
}

// SetHostPlan 设置主机套餐
func (s *HostService) SetHostPlan(hsName, planName string, planCfg *model.VMConfig) error {
	row, err := s.db.GetHost(hsName)
	if err != nil || row == nil {
		return fmt.Errorf("主机 %s 不存在", hsName)
	}
	var plans map[string]interface{}
	if err := json.Unmarshal([]byte(row.ServerPlan), &plans); err != nil {
		plans = make(map[string]interface{})
	}
	plans[planName] = planCfg
	planJSON, _ := json.Marshal(plans)
	row.ServerPlan = string(planJSON)
	return s.db.UpdateHost(row)
}

// DeleteHostPlan 删除主机套餐
func (s *HostService) DeleteHostPlan(hsName, planName string) error {
	row, err := s.db.GetHost(hsName)
	if err != nil || row == nil {
		return fmt.Errorf("主机 %s 不存在", hsName)
	}
	var plans map[string]interface{}
	if err := json.Unmarshal([]byte(row.ServerPlan), &plans); err != nil {
		return nil
	}
	delete(plans, planName)
	planJSON, _ := json.Marshal(plans)
	row.ServerPlan = string(planJSON)
	return s.db.UpdateHost(row)
}

// GetAvailablePorts 获取主机可用端口范围
func (s *HostService) GetAvailablePorts(hsName string) (map[string]interface{}, error) {
	row, err := s.db.GetHost(hsName)
	if err != nil || row == nil {
		return nil, fmt.Errorf("主机 %s 不存在", hsName)
	}
	return map[string]interface{}{
		"ports_start": row.PortsStart,
		"ports_close": row.PortsClose,
	}, nil
}

// ============================================================
// 驱动管理
// ============================================================

// initDriver 初始化主机驱动（并从数据库恢复虚拟机配置）
func (s *HostService) initDriver(cfg *model.HSConfig) {
	d := driver.NewDriver(cfg)
	if d == nil {
		return
	}
	s.restoreVMSaving(cfg.ServerName, d)
	s.mu.Lock()
	s.drivers[cfg.ServerName] = d
	s.mu.Unlock()
}

// restoreVMSaving 从数据库恢复驱动内存中的虚拟机配置
// 对应 Python BasicServer.HSLoader 中 vm_saving 的加载逻辑。
// 缺少这一步会导致服务重启或主机重新加载后「看不到、也无法管理已有的虚拟机」。
func (s *HostService) restoreVMSaving(hsName string, d driver.HostDriver) {
	rows, err := s.db.GetVMSaving(hsName)
	if err != nil || len(rows) == 0 {
		return
	}
	vms := make(map[string]*model.VMConfig, len(rows))
	for vmUUID, vmJSON := range rows {
		vm, decodeErr := DecodeVMConfig(vmJSON)
		if decodeErr != nil || vm == nil {
			continue
		}
		vms[vmUUID] = vm
	}
	if len(vms) > 0 {
		d.SetVMSaving(vms)
	}
}

// getDriver 获取主机驱动
func (s *HostService) getDriver(hsName string) driver.HostDriver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.drivers[hsName]
}

// GetDriver 公开获取驱动（供API层使用）
// 未加载时按数据库配置惰性创建（仅限已启用主机），
// 保证服务重启后无需等待调度器扫描即可正常管理主机与虚拟机。
func (s *HostService) GetDriver(hsName string) driver.HostDriver {
	return s.GetOrCreateDriver(hsName)
}

// GetOrCreateDriver 获取驱动，未加载时按数据库配置惰性创建（仅限已启用主机）。
//
// 使用独立的初始化锁并二次检查：原实现是「检查不存在 → 构造 → 覆盖」，
// 并发首次访问会重复创建驱动，导致同一主机出现两套内存状态与锁。
func (s *HostService) GetOrCreateDriver(hsName string) driver.HostDriver {
	if d := s.getDriver(hsName); d != nil {
		return d
	}

	s.initMu.Lock()
	defer s.initMu.Unlock()

	// 二次检查：可能已被其它协程创建
	if d := s.getDriver(hsName); d != nil {
		return d
	}
	row, err := s.db.GetHost(hsName)
	if err != nil || row == nil || row.EnableHost == 0 {
		return nil
	}
	cfg, err := rowToHSConfig(row)
	if err != nil {
		return nil
	}
	s.initDriver(cfg)
	return s.getDriver(hsName)
}

// LoadAllDrivers 启动时加载所有已启用主机的驱动
func (s *HostService) LoadAllDrivers() error {
	rows, err := s.db.GetAllHosts()
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.EnableHost == 0 {
			continue
		}
		cfg, err := rowToHSConfig(row)
		if err != nil {
			continue
		}
		s.initDriver(cfg)
	}
	return nil
}

// ============================================================
// 辅助函数：数据库行 <-> 模型转换
// ============================================================

func rowToHSConfig(row *data.HSConfigRow) (*model.HSConfig, error) {
	cfg := model.NewHSConfig()
	cfg.ServerName = row.HsName
	cfg.ServerType = row.ServerType
	cfg.ServerAddr = row.ServerAddr
	cfg.ServerUser = row.ServerUser
	cfg.ServerPass = row.ServerPass
	cfg.ServerPort = row.ServerPort
	cfg.ServerArea = row.ServerArea
	cfg.FilterName = row.FilterName
	cfg.EnableHost = row.EnableHost == 1
	cfg.ImagesPath = row.ImagesPath
	cfg.DvdromPath = row.DvdromPath
	cfg.SystemPath = row.SystemPath
	cfg.BackupPath = row.BackupPath
	cfg.ExternPath = row.ExternPath
	cfg.LaunchPath = row.LaunchPath
	cfg.NetworkNat = row.NetworkNat
	cfg.NetworkPub = row.NetworkPub
	cfg.PortsStart = row.PortsStart
	cfg.PortsClose = row.PortsClose
	cfg.RemotePort = row.RemotePort
	cfg.LimitsNums = row.LimitsNums
	cfg.IKuaiAddr = row.IKuaiAddr
	cfg.IKuaiUser = row.IKuaiUser
	cfg.IKuaiPass = row.IKuaiPass
	cfg.NCpuPrice = row.NCpuPrice
	cfg.NMemPrice = row.NMemPrice
	cfg.NHddPrice = row.NHddPrice
	cfg.NNetPrice = row.NNetPrice

	// 解析JSON字段
	if row.SystemMaps != "" && row.SystemMaps != "{}" && row.SystemMaps != "[]" {
		cfg.SystemMaps = parseOSList(row.SystemMaps)
	}
	if row.ImagesMaps != "" && row.ImagesMaps != "{}" && row.ImagesMaps != "[]" {
		cfg.ImagesMaps = parseOSList(row.ImagesMaps)
	}
	if row.PublicAddr != "" && row.PublicAddr != "[]" {
		_ = json.Unmarshal([]byte(row.PublicAddr), &cfg.PublicAddr)
	}
	if row.ExtendData != "" && row.ExtendData != "{}" {
		_ = json.Unmarshal([]byte(row.ExtendData), &cfg.ExtendData)
	}
	if row.IPAddrMaps != "" && row.IPAddrMaps != "{}" {
		_ = json.Unmarshal([]byte(row.IPAddrMaps), &cfg.IPAddrMaps)
	}
	if row.IPAddrDDNS != "" && row.IPAddrDDNS != "[]" {
		_ = json.Unmarshal([]byte(row.IPAddrDDNS), &cfg.IPAddrDDNS)
	}
	if row.ServerPlan != "" && row.ServerPlan != "{}" {
		_ = json.Unmarshal([]byte(row.ServerPlan), &cfg.ServerPlan)
	}

	return cfg, nil
}

// parseOSList 将数据库中的 system_maps/images_maps JSON 字段解析为 []OSConfig，兼容旧 dict 格式
func parseOSList(raw string) []model.OSConfig {
	result := []model.OSConfig{}
	if raw == "" {
		return result
	}
	// 先尝试新格式: []OSConfig
	var asList []model.OSConfig
	if err := json.Unmarshal([]byte(raw), &asList); err == nil {
		// 兼容历史数据: 若缺失 sys_flag 则默认启用
		var asRaw []map[string]interface{}
		_ = json.Unmarshal([]byte(raw), &asRaw)
		for i := range asList {
			if i < len(asRaw) {
				if _, ok := asRaw[i]["sys_flag"]; !ok {
					asList[i].SysFlag = true
				}
			}
		}
		return asList
	}
	// 兼容旧 dict 格式
	var asDict map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &asDict); err != nil {
		return result
	}
	for name, val := range asDict {
		switch v := val.(type) {
		case []interface{}:
			// 旧 system_maps: {name: [file, size]}
			item := model.OSConfig{SysName: name, SysFlag: true}
			if len(v) >= 1 {
				if s, ok := v[0].(string); ok {
					item.SysFile = s
				}
			}
			if len(v) >= 2 {
				switch sz := v[1].(type) {
				case string:
					item.SysSize = sz
				case float64:
					item.SysSize = fmt.Sprintf("%d", int(sz))
				}
			}
			result = append(result, item)
		case string:
			// 旧 images_maps: {name: file}
			result = append(result, model.OSConfig{SysName: name, SysFile: v, SysFlag: true})
		case map[string]interface{}:
			item := model.OSConfig{SysName: name, SysFlag: true}
			if s, ok := v["sys_name"].(string); ok && s != "" {
				item.SysName = s
			}
			if s, ok := v["sys_file"].(string); ok {
				item.SysFile = s
			}
			if s, ok := v["sys_size"].(string); ok {
				item.SysSize = s
			}
			if s, ok := v["sys_type"].(string); ok {
				item.SysType = s
			}
			if b, ok := v["sys_flag"].(bool); ok {
				item.SysFlag = b
			}
			result = append(result, item)
		}
	}
	return result
}

// ParseOSList 导出：将 system_maps/images_maps 的 JSON 解析为 []OSConfig
// 兼容新版列表格式与前端旧版「名称 -> 值」映射格式
func ParseOSList(raw string) []model.OSConfig {
	return parseOSList(raw)
}

// OSListToArray 将 []OSConfig 转换为接口返回的数组形式。
//
// 与 Python 版 get_os_images 的 _os_list_dump 语义一致：
// system_maps / images_maps 均为 list[OSConfig]，并过滤显式禁用的镜像（sys_flag=false）。
//
// 注意：此前导出的是「名称 -> 值」映射，与前端契约不符 ——
// 前端把 system_maps 声明为 OSConfigItem[] 并直接调用 .filter/.find
// （见 DockCreateModal.tsx、HostManage.tsx），收到对象会抛出
// "hostImages.filter is not a function" 并导致创建弹窗崩溃。
func OSListToArray(list []model.OSConfig) []model.OSConfig {
	result := make([]model.OSConfig, 0, len(list))
	for _, item := range list {
		if item.SysName == "" || !item.SysFlag {
			continue
		}
		result = append(result, item)
	}
	return result
}

// NormalizeOSMaps 就地把请求体中的 system_maps / images_maps 统一为 []OSConfig。
// 前端提交的是「名称 -> 值」映射，直接反序列化到 []OSConfig 会报类型错误。
func NormalizeOSMaps(raw map[string]interface{}) {
	for _, key := range []string{"system_maps", "images_maps"} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		if _, isList := v.([]model.OSConfig); isList {
			continue
		}
		data, err := json.Marshal(v)
		if err != nil {
			continue
		}
		raw[key] = parseOSList(string(data))
	}
}

// HSConfigToAPI 将主机配置转换为接口返回结构
// 字段与 Python HSConfig.__save__() 一致，但 system_maps/images_maps 使用前端的映射形式
func HSConfigToAPI(cfg *model.HSConfig) map[string]interface{} {
	out := make(map[string]interface{})
	if cfg == nil {
		return out
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return out
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return make(map[string]interface{})
	}
	out["system_maps"] = OSListToArray(cfg.SystemMaps)
	out["images_maps"] = OSListToArray(cfg.ImagesMaps)
	return out
}

func hsConfigToRow(cfg *model.HSConfig) (*data.HSConfigRow, error) {
	systemMapsJSON, _ := json.Marshal(cfg.SystemMaps)
	imagesMapsJSON, _ := json.Marshal(cfg.ImagesMaps)
	publicAddrJSON, _ := json.Marshal(cfg.PublicAddr)
	extendDataJSON, _ := json.Marshal(cfg.ExtendData)
	ipAddrMapsJSON, _ := json.Marshal(cfg.IPAddrMaps)
	ipAddrDDNSJSON, _ := json.Marshal(cfg.IPAddrDDNS)
	serverPlanJSON, _ := json.Marshal(cfg.ServerPlan)

	enableHost := 0
	if cfg.EnableHost {
		enableHost = 1
	}

	return &data.HSConfigRow{
		HsName:     cfg.ServerName,
		ServerName: cfg.ServerName,
		ServerType: cfg.ServerType,
		ServerAddr: cfg.ServerAddr,
		ServerUser: cfg.ServerUser,
		ServerPass: cfg.ServerPass,
		ServerPort: cfg.ServerPort,
		ServerArea: cfg.ServerArea,
		FilterName: cfg.FilterName,
		EnableHost: enableHost,
		ImagesPath: cfg.ImagesPath,
		DvdromPath: cfg.DvdromPath,
		SystemPath: cfg.SystemPath,
		BackupPath: cfg.BackupPath,
		ExternPath: cfg.ExternPath,
		LaunchPath: cfg.LaunchPath,
		NetworkNat: cfg.NetworkNat,
		NetworkPub: cfg.NetworkPub,
		PortsStart: cfg.PortsStart,
		PortsClose: cfg.PortsClose,
		RemotePort: cfg.RemotePort,
		LimitsNums: cfg.LimitsNums,
		IKuaiAddr:  cfg.IKuaiAddr,
		IKuaiUser:  cfg.IKuaiUser,
		IKuaiPass:  cfg.IKuaiPass,
		NCpuPrice:  cfg.NCpuPrice,
		NMemPrice:  cfg.NMemPrice,
		NHddPrice:  cfg.NHddPrice,
		NNetPrice:  cfg.NNetPrice,
		SystemMaps: string(systemMapsJSON),
		ImagesMaps: string(imagesMapsJSON),
		PublicAddr: string(publicAddrJSON),
		ExtendData: string(extendDataJSON),
		IPAddrMaps: string(ipAddrMapsJSON),
		IPAddrDDNS: string(ipAddrDDNSJSON),
		ServerPlan: string(serverPlanJSON),
	}, nil
}

// ============================================================
// 系统工具
// ============================================================

// GetSystemIPv4 获取本机所有IPv4地址
func GetSystemIPv4() ([]string, error) {
	var ips []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil {
				ips = append(ips, ip4.String())
			}
		}
	}
	return ips, nil
}

// CheckHostAccess 检查用户是否有访问主机的权限
func CheckHostAccess(hsName string, user map[string]interface{}) bool {
	if model.SafeGetBool(user, "is_admin") {
		return true
	}
	assignedHosts := model.SafeGetStringSlice(user, "assigned_hosts")
	for _, h := range assignedHosts {
		if h == hsName {
			return true
		}
	}
	return false
}

// CheckVMPermission 检查用户虚拟机操作权限
func CheckVMPermission(action string, user map[string]interface{}) (bool, string) {
	if model.SafeGetBool(user, "is_admin") {
		return true, ""
	}
	if !model.SafeGetBool(user, "is_active") {
		return false, "用户已被禁用"
	}
	switch action {
	case "create":
		if !model.SafeGetBool(user, "can_create_vm") {
			return false, "没有创建虚拟机的权限"
		}
	case "delete":
		if !model.SafeGetBool(user, "can_delete_vm") {
			return false, "没有删除虚拟机的权限"
		}
	case "modify":
		if !model.SafeGetBool(user, "can_modify_vm") {
			return false, "没有修改虚拟机的权限"
		}
	}
	return true, ""
}

// CheckResourceQuota 检查用户资源配额
func CheckResourceQuota(user map[string]interface{}, resources map[string]int) (bool, string) {
	if model.SafeGetBool(user, "is_admin") {
		return true, ""
	}
	resourceNames := map[string]string{
		"cpu": "CPU核心", "ram": "内存", "ssd": "磁盘", "gpu": "GPU显存",
		"nat_ports": "NAT端口", "web_proxy": "WEB代理",
		"nat_ips": "内网IP", "pub_ips": "公网IP",
		"traffic": "流量", "bandwidth_up": "上行带宽", "bandwidth_down": "下行带宽",
	}
	for resource, amount := range resources {
		if amount <= 0 {
			continue
		}
		quotaKey := "quota_" + resource
		usedKey := "used_" + resource
		quota := toInt(user[quotaKey])
		used := toInt(user[usedKey])
		if used+amount > quota {
			name := resourceNames[resource]
			if name == "" {
				name = resource
			}
			return false, fmt.Sprintf("%s配额不足，已使用%d/%d", name, used, quota)
		}
	}
	return true, ""
}

func toInt(v interface{}) int {
	switch val := v.(type) {
	case int:
		return val
	case int64:
		return int(val)
	case float64:
		return int(val)
	}
	return 0
}

// ============================================================
// 系统配置服务
// ============================================================

// SystemService 系统配置服务
type SystemService struct {
	db           *data.DB
	token        string
	tokenMu      sync.RWMutex
	emailService *auth.EmailService
}

// NewSystemService 创建系统配置服务
func NewSystemService(db *data.DB) *SystemService {
	s := &SystemService{db: db}
	// 从数据库加载Token
	if t, err := db.GetGlobal("bearer_token"); err == nil && t != "" {
		s.token = t
	}
	// 加载邮件配置
	s.loadEmailConfig()
	return s
}

// GetToken 获取当前Bearer Token
func (s *SystemService) GetToken() string {
	s.tokenMu.RLock()
	defer s.tokenMu.RUnlock()
	return s.token
}

// SetToken 设置Bearer Token
func (s *SystemService) SetToken(token string) error {
	s.tokenMu.Lock()
	s.token = token
	s.tokenMu.Unlock()
	return s.db.SetGlobal("bearer_token", token)
}

// ResetToken 重置Bearer Token（生成新Token）
func (s *SystemService) ResetToken() (string, error) {
	token, err := utils.GenerateToken()
	if err != nil {
		return "", err
	}
	if err := s.SetToken(token); err != nil {
		return "", err
	}
	return token, nil
}

// GetSettings 获取系统设置
func (s *SystemService) GetSettings() (map[string]interface{}, error) {
	data, err := s.db.GetGlobal("system_settings")
	if err != nil || data == "" {
		return make(map[string]interface{}), nil
	}
	var settings map[string]interface{}
	if err := json.Unmarshal([]byte(data), &settings); err != nil {
		return make(map[string]interface{}), nil
	}
	return settings, nil
}

// UpdateSettings 更新系统设置
func (s *SystemService) UpdateSettings(settings map[string]interface{}) error {
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if err := s.db.SetGlobal("system_settings", string(data)); err != nil {
		return err
	}
	// 重新加载邮件配置
	s.loadEmailConfig()
	return nil
}

// loadEmailConfig 从系统设置加载邮件配置
func (s *SystemService) loadEmailConfig() {
	settings, err := s.GetSettings()
	if err != nil {
		return
	}
	apiKey, _ := settings["resend_apikey"].(string)
	fromEmail, _ := settings["resend_email"].(string)
	s.emailService = &auth.EmailService{
		APIKey:    apiKey,
		FromEmail: fromEmail,
	}
}

// GetEmailService 获取邮件服务
func (s *SystemService) GetEmailService() *auth.EmailService {
	return s.emailService
}

// GetDefaultQuotas 获取新用户默认配额和权限配置
// 从系统设置中读取 default_quota_* 和 default_can_* 配置项，缺失时使用硬编码默认值
func (s *SystemService) GetDefaultQuotas() map[string]interface{} {
	settings, _ := s.GetSettings()

	// 辅助函数：从设置中获取整数值，不存在则使用默认值
	getInt := func(key string, defaultVal int) int {
		if v, ok := settings[key]; ok {
			switch val := v.(type) {
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
			case int:
				return val
			}
		}
		return defaultVal
	}

	// 辅助函数：从设置中获取布尔权限值（"1"或true表示允许）
	getBool := func(key string, defaultVal bool) int {
		if v, ok := settings[key]; ok {
			switch val := v.(type) {
			case string:
				if val == "1" || val == "true" {
					return 1
				}
				return 0
			case bool:
				if val {
					return 1
				}
				return 0
			case float64:
				if int(val) == 1 {
					return 1
				}
				return 0
			}
		}
		if defaultVal {
			return 1
		}
		return 0
	}

	return map[string]interface{}{
		// 资源配额
		"quota_cpu":            getInt("default_quota_cpu", 2),
		"quota_ram":            getInt("default_quota_ram", 4),
		"quota_ssd":            getInt("default_quota_ssd", 20),
		"quota_gpu":            getInt("default_quota_gpu", 0),
		"quota_nat_ports":      getInt("default_quota_nat_ports", 5),
		"quota_web_proxy":      getInt("default_quota_web_proxy", 0),
		"quota_bandwidth_up":   getInt("default_quota_bandwidth_up", 10),
		"quota_bandwidth_down": getInt("default_quota_bandwidth_down", 10),
		"quota_traffic":        getInt("default_quota_traffic", 100),
		// 默认权限
		"can_create_vm": getBool("default_can_create_vm", true),
		"can_modify_vm": getBool("default_can_modify_vm", true),
		"can_delete_vm": getBool("default_can_delete_vm", true),
	}
}

// GetSystemStats 获取系统统计信息
func (s *SystemService) GetSystemStats(db *data.DB) (map[string]interface{}, error) {
	hosts, err := db.GetAllHosts()
	if err != nil {
		return nil, err
	}
	hostCount := len(hosts)
	vmCount := 0
	for _, h := range hosts {
		vms, err := db.GetVMSaving(h.HsName)
		if err == nil {
			vmCount += len(vms)
		}
	}
	return map[string]interface{}{
		"host_count": hostCount,
		"vm_count":   vmCount,
	}, nil
}
