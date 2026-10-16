// Package service 虚拟机配置合并（主机间同步）
// 合并语义与 Python 版 MainServer.api_sync_push / HostManage._sync_pull 完全一致：
//   - 基本字段：远端非空（nil / 空串）时才覆盖，0 视为有效值
//   - own_all：远端有而本地没有的所有者补充进来
//   - nic_all：远端有而本地没有的网卡补充；本地已有则仅补充为空的 IP/MAC 字段
//   - nat_all / web_all：仅当本地为空时整体补充
//   - 本地不存在的虚拟机：按远端配置新建
package service

import (
	"encoding/json"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// MergeRemoteVMs 将远端虚拟机数据智能合并进本地主机
// 返回成功合并的虚拟机数量
func (s *HostService) MergeRemoteVMs(hsName string, remoteVMs map[string]map[string]interface{}) (int, error) {
	updatedCount := 0

	for vmUUID, remote := range remoteVMs {
		if len(remote) == 0 {
			continue
		}

		localJSON, err := s.db.GetVMConfig(hsName, vmUUID)
		if err != nil {
			continue
		}

		if localJSON == "" {
			// 本地不存在：按远端配置新建
			data, err := json.Marshal(remote)
			if err != nil {
				continue
			}
			newVM, err := model.VMConfigFromJSON(data)
			if err != nil {
				continue
			}
			newVM.VmUUID = vmUUID
			if err := s.db.UpsertVMConfig(hsName, vmUUID, newVM); err != nil {
				continue
			}
			updatedCount++
			continue
		}

		var localVM model.VMConfig
		if err := json.Unmarshal([]byte(localJSON), &localVM); err != nil {
			continue
		}

		mergeVMConfigInto(&localVM, remote)

		if err := s.db.UpsertVMConfig(hsName, vmUUID, &localVM); err != nil {
			continue
		}
		updatedCount++
	}

	return updatedCount, nil
}

// mergeVMConfigInto 将远端配置合并进本地配置（就地修改 local）
func mergeVMConfigInto(local *model.VMConfig, remote map[string]interface{}) {
	// 基本字段：远端非 nil / 非空串时才覆盖（0 是有效值）
	setIntField := func(key string, target *int) {
		v, ok := remote[key]
		if !ok || v == nil {
			return
		}
		if str, isStr := v.(string); isStr && str == "" {
			return
		}
		*target = toInt(v)
	}
	setIntField("cpu_num", &local.CpuNum)
	setIntField("mem_num", &local.MemNum)
	setIntField("hdd_num", &local.HddNum)
	setIntField("gpu_mem", &local.GpuMem)
	setIntField("speed_u", &local.SpeedU)
	setIntField("speed_d", &local.SpeedD)
	setIntField("nat_num", &local.NatNum)
	setIntField("web_num", &local.WebNum)

	if v, ok := remote["os_name"].(string); ok && v != "" {
		local.OsName = v
	}

	// own_all：远端有而本地没有的用户补充进来
	if remoteOwn, ok := remote["own_all"].(map[string]interface{}); ok && len(remoteOwn) > 0 {
		if local.OwnAll == nil {
			local.OwnAll = make(map[string]model.UserMask)
		}
		for username, maskData := range remoteOwn {
			if _, exists := local.OwnAll[username]; !exists {
				local.OwnAll[username] = parseUserMask(maskData)
			}
		}
	}

	// nic_all：远端有而本地没有的网卡补充；本地已有则补充为空的 IP/MAC
	if remoteNic, ok := remote["nic_all"].(map[string]interface{}); ok && len(remoteNic) > 0 {
		if local.NicAll == nil {
			local.NicAll = make(map[string]model.NCConfig)
		}
		for nicName, nicData := range remoteNic {
			nicMap, ok := nicData.(map[string]interface{})
			if !ok {
				continue
			}
			if existing, exists := local.NicAll[nicName]; exists {
				if v, ok := nicMap["ip4_addr"].(string); ok && v != "" && existing.IP4Addr == "" {
					existing.IP4Addr = v
				}
				if v, ok := nicMap["ip6_addr"].(string); ok && v != "" && existing.IP6Addr == "" {
					existing.IP6Addr = v
				}
				if v, ok := nicMap["mac_addr"].(string); ok && v != "" && existing.MacAddr == "" {
					existing.MacAddr = v
				}
				local.NicAll[nicName] = existing
				continue
			}
			var nic model.NCConfig
			if err := unmarshalMap(nicMap, &nic); err != nil {
				continue
			}
			if nic.NicName == "" {
				nic.NicName = nicName
			}
			local.NicAll[nicName] = nic
		}
	}

	// nat_all：仅当本地为空时整体补充
	if remoteNat, ok := remote["nat_all"].([]interface{}); ok && len(remoteNat) > 0 && len(local.NatAll) == 0 {
		local.NatAll = make([]model.PortData, 0, len(remoteNat))
		for _, nat := range remoteNat {
			natMap, ok := nat.(map[string]interface{})
			if !ok {
				continue
			}
			var pd model.PortData
			if err := unmarshalMap(natMap, &pd); err != nil {
				continue
			}
			local.NatAll = append(local.NatAll, pd)
		}
	}

	// web_all：仅当本地为空时整体补充
	if remoteWeb, ok := remote["web_all"].([]interface{}); ok && len(remoteWeb) > 0 && len(local.WebAll) == 0 {
		local.WebAll = make([]model.WebProxy, 0, len(remoteWeb))
		for _, web := range remoteWeb {
			webMap, ok := web.(map[string]interface{})
			if !ok {
				continue
			}
			var wp model.WebProxy
			if err := unmarshalMap(webMap, &wp); err != nil {
				continue
			}
			local.WebAll = append(local.WebAll, wp)
		}
	}
}

// parseUserMask 解析远端所有者权限掩码
// 支持 dict（字段->bool）、数字（16位掩码）、其他（默认全权限）
func parseUserMask(v interface{}) model.UserMask {
	switch val := v.(type) {
	case map[string]interface{}:
		var m model.UserMask
		if err := unmarshalMap(val, &m); err == nil {
			return m
		}
		return model.FullUserMask()
	case float64:
		return model.UserMaskFromBits(int(val))
	case int:
		return model.UserMaskFromBits(val)
	case int64:
		return model.UserMaskFromBits(int(val))
	default:
		return model.FullUserMask()
	}
}

// unmarshalMap 通过 JSON 中转将 map 解析为目标结构体
func unmarshalMap(src interface{}, dst interface{}) error {
	data, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}
