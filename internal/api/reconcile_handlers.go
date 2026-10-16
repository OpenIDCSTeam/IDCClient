// Package api 外部资源对账与删除墓碑的运维接口
//
// 这些接口全部要求管理员：补偿任务与删除墓碑会暴露主机、虚拟机标识
// 与外部调用失败原因，属于运维排查信息。
package api

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
)

// maxOutboxPageSize 单次返回的最大补偿任务条数
const maxOutboxPageSize = 200

// handleOutboxList GET /api/system/outbox/list?status=&limit=
func (app *App) handleOutboxList(c *gin.Context) {
	if app.ReconcileSvc == nil {
		utils.InternalError(c, "对账服务未初始化")
		return
	}
	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxOutboxPageSize {
		limit = maxOutboxPageSize
	}

	entries, err := app.DB.ListOutbox(c.Query("status"), limit)
	if err != nil {
		utils.InternalError(c, "查询补偿任务失败")
		return
	}
	if entries == nil {
		entries = []*data.OutboxEntry{}
	}

	utils.Success(c, gin.H{
		"items":   entries,
		"total":   len(entries),
		"pending": app.ReconcileSvc.PendingCount(),
	})
}

// handleOutboxRetry POST /api/system/outbox/:outbox_id/retry
// 立即重试一条补偿任务（忽略退避时间）
func (app *App) handleOutboxRetry(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("outbox_id"), 10, 64)
	if err != nil || id <= 0 {
		utils.BadRequest(c, "补偿任务ID无效")
		return
	}
	if err := app.DB.ResetOutboxRetry(id); err != nil {
		utils.InternalError(c, "重置补偿任务失败")
		return
	}

	// 立即执行一轮，便于运维直接看到结果
	if app.ReconcileSvc != nil {
		app.ReconcileSvc.ProcessOutbox(maxOutboxPageSize)
	}
	utils.SuccessMsg(c, "补偿任务已重新排队", gin.H{"outbox_id": id})
}

// handleOutboxDismiss POST /api/system/outbox/:outbox_id/dismiss
// 人工忽略一条补偿任务（外部资源需由运维另行清理）
func (app *App) handleOutboxDismiss(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("outbox_id"), 10, 64)
	if err != nil || id <= 0 {
		utils.BadRequest(c, "补偿任务ID无效")
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	_ = c.ShouldBindJSON(&req)

	if err := app.DB.DismissOutbox(id, req.Reason); err != nil {
		utils.InternalError(c, "忽略补偿任务失败")
		return
	}
	app.Logger.Log("system", "WARNING", "外部资源补偿",
		"人工忽略了补偿任务 id="+strconv.FormatInt(id, 10)+" 原因: "+req.Reason, false)
	utils.SuccessMsg(c, "补偿任务已忽略", gin.H{"outbox_id": id})
}

// handleTombstoneList GET /api/system/tombstone/list
// 列出尚未完成的删除墓碑（即中断的删除流程）
func (app *App) handleTombstoneList(c *gin.Context) {
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxOutboxPageSize {
		limit = maxOutboxPageSize
	}

	tombs, err := app.DB.ListIncompleteTombstones(limit)
	if err != nil {
		utils.InternalError(c, "查询删除墓碑失败")
		return
	}
	if tombs == nil {
		tombs = []*data.Tombstone{}
	}
	utils.Success(c, gin.H{"items": tombs, "total": len(tombs)})
}

// handleTombstoneSweep POST /api/system/tombstone/sweep
// 立即继续执行未完成的删除流程
func (app *App) handleTombstoneSweep(c *gin.Context) {
	if app.DeleteWorkflow == nil {
		utils.InternalError(c, "删除工作流未初始化")
		return
	}
	resumed, failed := app.DeleteWorkflow.SweepIncomplete(50)
	utils.SuccessMsg(c, "删除续做已执行", gin.H{
		"resumed": resumed,
		"failed":  failed,
	})
}
