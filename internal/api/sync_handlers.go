// Package api 同步推送 API 处理器
package api

import (
	"fmt"

	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
	"github.com/gin-gonic/gin"
)

// handleSyncPush 同步推送虚拟机配置（远端主机推送到本地）
// POST /api/sync/push/:hs_name
// Body: {"vms": {"vm_uuid_1": {...}, "vm_uuid_2": {...}}}
func (app *App) handleSyncPush(c *gin.Context) {
	hsName := c.Param("hs_name")

	// 检查主机访问权限
	username := c.GetString("username")
	if !app.HostSvc.CheckHostAccess(username, hsName) {
		utils.Forbidden(c)
		return
	}

	var req struct {
		VMs map[string]map[string]interface{} `json:"vms"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "无效的请求数据")
		return
	}

	if len(req.VMs) == 0 {
		utils.BadRequest(c, "vms 不能为空")
		return
	}

	// 调用服务层合并配置
	updatedCount, err := app.HostSvc.SyncPushVM(hsName, req.VMs)
	if err != nil {
		utils.InternalError(c, "同步推送失败: "+err.Error())
		return
	}

	app.Logger.Log("sync", "INFO", "同步推送", fmt.Sprintf("同步推送完成: 主机=%s, 更新虚拟机数=%d", hsName, updatedCount), true)

	utils.SuccessMsg(c, "同步推送成功", gin.H{
		"hs_name": hsName,
		"updated": updatedCount,
		"total":   len(req.VMs),
	})
}
