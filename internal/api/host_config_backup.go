package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/service"
	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
)

func (app *App) handleHostConfigExport(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	backup, err := app.HostSvc.ExportHostConfigs(c.Query("hs_name"))
	if err != nil {
		utils.BadRequest(c, "导出主机配置失败: "+err.Error())
		return
	}
	app.logEvent(c, "host", "导出主机配置").
		Detail("host_count", len(backup.Hosts)).Info("导出主机配置成功")
	utils.Success(c, backup)
}

func (app *App) handleHostConfigRestore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, service.MaxHostConfigBackupSize+1024)
	var req struct {
		Backup    json.RawMessage `json:"backup"`
		Overwrite bool            `json:"overwrite"`
		DryRun    *bool           `json:"dry_run"`
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil || json.Unmarshal(raw, &req) != nil {
		utils.BadRequest(c, "请求格式错误或配置文件超过 10 MB")
		return
	}
	backup, err := service.DecodeHostConfigBackup(req.Backup)
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	items, err := app.HostSvc.PreviewHostConfigRestore(backup)
	if err != nil {
		utils.BadRequest(c, err.Error())
		return
	}
	if req.DryRun == nil || *req.DryRun {
		utils.Success(c, gin.H{"hosts": items})
		return
	}
	for _, item := range items {
		if item.Action == "overwrite" && !req.Overwrite {
			utils.BadRequest(c, "存在同名主机，请确认覆盖后再还原")
			return
		}
	}
	if err := app.HostSvc.RestoreHostConfigs(backup, req.Overwrite); err != nil {
		app.logEvent(c, "host", "还原主机配置").Err(err).Errorf("还原主机配置失败: %v", err)
		utils.InternalError(c, "还原失败，配置未提交，请刷新预览后重试")
		return
	}
	app.assignRestoredHostsToAdmins(backup)
	for _, item := range items {
		app.logEvent(c, "host", "还原主机配置").Host(item.Name).
			Detail("restore_action", item.Action).Info("主机配置还原成功")
	}
	utils.SuccessMsg(c, "主机配置还原成功", gin.H{"hosts": items})
}

func (app *App) assignRestoredHostsToAdmins(backup *service.HostConfigBackup) {
	users, err := app.DB.GetAllUsers()
	if err != nil {
		app.Logger.Event("host", "分配主机").Err(err).Warn("读取管理员列表失败")
		return
	}
	for _, user := range users {
		if !isAdminUser(user) {
			continue
		}
		hosts := toStringSlice(user["assigned_hosts"])
		seen := make(map[string]bool, len(hosts))
		for _, name := range hosts {
			seen[name] = true
		}
		changed := false
		for _, cfg := range backup.Hosts {
			if !seen[cfg.ServerName] {
				hosts = append(hosts, cfg.ServerName)
				seen[cfg.ServerName] = true
				changed = true
			}
		}
		if !changed {
			continue
		}
		raw, _ := json.Marshal(hosts)
		if err := app.DB.UpdateUser(toIntVal(user["id"]), map[string]interface{}{"assigned_hosts": string(raw)}); err != nil {
			app.Logger.Event("host", "分配主机").Err(err).Warn("分配还原主机到管理员失败")
		}
	}
}
