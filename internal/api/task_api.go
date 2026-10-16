// Package api 任务 API 处理器
package api

import (
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/auth"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/task"
	"github.com/OpenIDCSTeam/HostAgent-Go/pkg/utils"
)

// maxTaskPageSize 单页最大任务数，避免全量拉取
const maxTaskPageSize = 200

// taskScope 返回当前用户的任务可见范围。
// 管理员可见全部；普通用户只能看到自己创建的任务。
func taskScope(c *gin.Context) (username string, isAdmin bool) {
	uc := auth.GetUserContext(c)
	if uc == nil {
		return "", false
	}
	return uc.Username, uc.IsAdmin
}

// canAccessTask 判断当前用户是否可访问指定任务
func canAccessTask(c *gin.Context, t *task.Task) bool {
	if t == nil {
		return false
	}
	username, isAdmin := taskScope(c)
	if isAdmin {
		return true
	}
	return username != "" && t.CreatedBy == username
}

// handleTaskList 列出任务
// GET /api/system/async_task/list?hs_name=&status=&task_type=&vm_uuid=&page=1&page_size=20
func (app *App) handleTaskList(c *gin.Context) {
	username, isAdmin := taskScope(c)

	filter := task.TaskFilter{
		Limit:  20,
		Offset: 0,
	}

	if statusStr := c.Query("status"); statusStr != "" {
		statuses := []task.TaskStatus{}
		for _, s := range strings.Split(statusStr, ",") {
			statuses = append(statuses, task.TaskStatus(strings.TrimSpace(s)))
		}
		filter.Status = statuses
	}

	// task_type 为 Python 版参数名，type 为 Go 原有参数名
	if typeStr := c.Query("task_type"); typeStr != "" {
		types := []task.TaskType{}
		for _, t := range strings.Split(typeStr, ",") {
			types = append(types, task.TaskType(strings.TrimSpace(t)))
		}
		filter.Type = types
	} else if typeStr := c.Query("type"); typeStr != "" {
		types := []task.TaskType{}
		for _, t := range strings.Split(typeStr, ",") {
			types = append(types, task.TaskType(strings.TrimSpace(t)))
		}
		filter.Type = types
	}

	// 非管理员强制只能查询自己的任务，忽略请求中的 created_by，
	// 否则任意登录用户可读取其它用户的任务详情（含 metadata）。
	if isAdmin {
		if createdBy := c.Query("created_by"); createdBy != "" {
			filter.CreatedBy = createdBy
		}
	} else {
		if username == "" {
			utils.Unauthorized(c)
			return
		}
		filter.CreatedBy = username
	}

	// 主机与虚拟机过滤（与 Python 版 /api/system/async_task/list 一致）
	if hsName := c.Query("hs_name"); hsName != "" {
		filter.HsName = hsName
	}
	if vmUUID := c.Query("vm_uuid"); vmUUID != "" {
		filter.VmUUID = vmUUID
	}

	// 分页：同时支持 Python 版 page/page_size 与 Go 原有的 limit/offset
	page, pageSize := 1, 20
	if v := c.Query("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pageSize = n
		}
	}
	if v := c.Query("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	if pageSize > maxTaskPageSize {
		pageSize = maxTaskPageSize
	}
	filter.Limit = pageSize
	filter.Offset = (page - 1) * pageSize

	// Go 原有参数可覆盖
	if limitStr := c.Query("limit"); limitStr != "" {
		if limit, err := strconv.Atoi(limitStr); err == nil && limit > 0 {
			if limit > maxTaskPageSize {
				limit = maxTaskPageSize
			}
			filter.Limit = limit
		}
	}
	if offsetStr := c.Query("offset"); offsetStr != "" {
		if offset, err := strconv.Atoi(offsetStr); err == nil && offset >= 0 {
			filter.Offset = offset
		}
	}

	tasks, total, err := app.TaskManager.ListTasks(filter)
	if err != nil {
		utils.InternalError(c, "查询任务失败")
		return
	}

	// items/total 为 Python 版字段；tasks 保留兼容
	utils.Success(c, gin.H{
		"items":     tasks,
		"tasks":     tasks,
		"total":     total,
		"page":      page,
		"page_size": filter.Limit,
		"limit":     filter.Limit,
		"offset":    filter.Offset,
	})
}

// handleTaskDetail 获取任务详情
// GET /api/tasks/:task_id
func (app *App) handleTaskDetail(c *gin.Context) {
	taskID := c.Param("task_id")

	t, err := app.TaskManager.GetTask(taskID)
	if err != nil {
		utils.NotFound(c, "任务不存在")
		return
	}
	if !canAccessTask(c, t) {
		utils.Forbidden(c)
		return
	}

	utils.Success(c, t)
}

// handleTaskCancel 取消任务
// POST /api/tasks/:task_id/cancel
// POST /api/tasks/:task_id/stop (别名)
func (app *App) handleTaskCancel(c *gin.Context) {
	taskID := c.Param("task_id")

	t, err := app.TaskManager.GetTask(taskID)
	if err != nil {
		utils.NotFound(c, "任务不存在")
		return
	}
	if !canAccessTask(c, t) {
		utils.Forbidden(c)
		return
	}

	if err := app.TaskManager.CancelTask(taskID); err != nil {
		utils.BadRequest(c, "取消任务失败: "+err.Error())
		return
	}

	utils.SuccessMsg(c, "任务已取消", gin.H{"task_id": taskID})
}

// handleTaskStats 获取任务统计
// GET /api/tasks/stats
func (app *App) handleTaskStats(c *gin.Context) {
	stats := app.TaskManager.GetStats()
	utils.Success(c, stats)
}

// handleTaskRetry 重试任务
// POST /api/system/async_task/:task_id/retry
func (app *App) handleTaskRetry(c *gin.Context) {
	taskID := c.Param("task_id")

	t, err := app.TaskManager.GetTask(taskID)
	if err != nil {
		utils.NotFound(c, "任务不存在")
		return
	}
	if !canAccessTask(c, t) {
		utils.Forbidden(c)
		return
	}

	if err := app.TaskManager.RetryTask(taskID); err != nil {
		utils.BadRequest(c, "重试任务失败: "+err.Error())
		return
	}

	utils.SuccessMsg(c, "任务已重置为待执行状态", gin.H{"task_id": taskID})
}
