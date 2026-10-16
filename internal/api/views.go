// Package api 虚拟机响应视图构造
//
// 前端（FrontPage）按 Python 版的响应结构渲染虚拟机：
//
//	GET /api/client/detail/:hs_name        → {uuid: {uuid, config, status, user_permissions}}
//	GET /api/client/detail/:hs_name/:uuid  → {uuid, config, status, user_permissions, is_admin, current_user}
//
// 前端通过 vm.config.own_all 判断所有者、通过 vm.status[0] 展示流量与监控
// （见 DockManage.tsx / UserPanels.tsx / PortManage.tsx）。
// 此前 Go 直接返回扁平 VMConfig，导致 vm.config 为 undefined：
// 所有者过滤拿不到数据、列表始终为空 —— 表现为「扫描不到虚拟机」。
package api

import (
	"encoding/json"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/auth"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// vmStatusHistoryMax 返回给前端的虚拟机状态条数（前端取第 0 条展示）
const vmStatusHistoryMax = 10

// toMap 将结构体转为 map（用于拼接响应）
func toMap(v interface{}) map[string]interface{} {
	result := make(map[string]interface{})
	if v == nil {
		return result
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return result
	}
	if err := json.Unmarshal(raw, &result); err != nil || result == nil {
		return make(map[string]interface{})
	}
	return result
}

// buildVMStatusList 读取虚拟机的历史状态（最近若干条）
func (app *App) buildVMStatusList(hsName, vmUUID string) []interface{} {
	rawList, err := app.DB.GetVMStatus(hsName, vmUUID, 0, 0)
	if err != nil || len(rawList) == 0 {
		return []interface{}{}
	}
	result := make([]interface{}, 0, vmStatusHistoryMax)
	for i, raw := range rawList {
		if i >= vmStatusHistoryMax {
			break
		}
		var item interface{}
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			continue
		}
		result = append(result, item)
	}
	return result
}

// vmUserMask 计算当前用户对该虚拟机的权限掩码。
// 管理员 / Token 登录视为全权限（与 Python 版 _calc_user_vm_permission 一致）。
func vmUserMask(vm *model.VMConfig, uc *auth.UserContext) model.UserMask {
	if uc == nil {
		return model.UserMask{}
	}
	if uc.IsAdmin || uc.IsToken {
		return model.FullUserMask()
	}
	if vm == nil || vm.OwnAll == nil {
		return model.UserMask{}
	}
	if mask, ok := vm.OwnAll[uc.Username]; ok {
		return mask
	}
	return model.UserMask{}
}

// buildVMView 构造虚拟机视图（对应 Python 版 get_vms / get_vm_detail 的单机结构）
func (app *App) buildVMView(hsName, vmUUID string, vm *model.VMConfig, uc *auth.UserContext) map[string]interface{} {
	config := toMap(vm)
	config["vm_uuid"] = vmUUID

	isAdmin := uc != nil && (uc.IsAdmin || uc.IsToken)
	mask := vmUserMask(vm, uc)

	// 无密码 / VNC 权限时屏蔽敏感字段（与 Python 版一致）
	if !isAdmin {
		if !mask.PwdEdits {
			config["os_pass"] = "******"
		}
		if !mask.VncEdits {
			config["vc_pass"] = "******"
		}
	}

	return map[string]interface{}{
		"uuid":             vmUUID,
		"config":           config,
		"status":           app.buildVMStatusList(hsName, vmUUID),
		"user_permissions": mask.ToBits(),
		"is_admin":         isAdmin,
	}
}
