// Package api 任务 API 处理器
package api

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/task"
	"github.com/gin-gonic/gin"
)

// handleTaskList 列出任务
// GET /api/tasks?status=pending,running&type=vm.create&limit=20&offset=0
func (app *App) handleTaskList(c *gin.Context) {
	// 解析过滤条件
	filter := task.TaskFilter{
		Limit:  20,
		Offset: 0,
	}

	// 解析状态过滤
	if statusStr := c.Query("status"); statusStr != "" {
		statuses := []task.TaskStatus{}
		for _, s := range strings.Split(statusStr, ",") {
			statuses = append(statuses, task.TaskStatus(strings.TrimSpace(s)))
		}
		filter.Status = statuses
	}

	// 解析类型过滤
	if typeStr := c.Query("type"); typeStr != "" {
		types := []task.TaskType{}
		for _, t := range strings.Split(typeStr, ",") {
			types = append(types, task.TaskType(strings.TrimSpace(t)))
		}
		filter.Type = types
	}

	// 解析创建用户过滤
	if createdBy := c.Query("created_by"); createdBy != "" {
		filter.CreatedBy = createdBy
	}

	// 解析分页参数
	if limitStr := c.Query("limit"); limitStr != "" {
		if limit, err := strconv.Atoi(limitStr); err == nil && limit > 0 {
			filter.Limit = limit
		}
	}
	if offsetStr := c.Query("offset"); offsetStr != "" {
		if offset, err := strconv.Atoi(offsetStr); err == nil && offset >= 0 {
			filter.Offset = offset
		}
	}

	// 查询任务
	tasks, total, err := app.TaskManager.ListTasks(filter)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": 500,
			"msg":  "查询任务失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"msg":  "success",
		"data": gin.H{
			"tasks":  tasks,
			"total":  total,
			"limit":  filter.Limit,
			"offset": filter.Offset,
		},
	})
}

// handleTaskDetail 获取任务详情
// GET /api/tasks/:task_id
func (app *App) handleTaskDetail(c *gin.Context) {
	taskID := c.Param("task_id")

	task, err := app.TaskManager.GetTask(taskID)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": 404,
			"msg":  "任务不存在: " + taskID,
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"msg":  "success",
		"data": task,
	})
}

// handleTaskCancel 取消任务
// POST /api/tasks/:task_id/cancel
// POST /api/tasks/:task_id/stop (别名)
func (app *App) handleTaskCancel(c *gin.Context) {
	taskID := c.Param("task_id")

	if err := app.TaskManager.CancelTask(taskID); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": 400,
			"msg":  "取消任务失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"msg":  "任务已取消",
		"data": gin.H{
			"task_id": taskID,
		},
	})
}

// handleTaskStats 获取任务统计
// GET /api/tasks/stats
func (app *App) handleTaskStats(c *gin.Context) {
	stats := app.TaskManager.GetStats()
	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"msg":  "success",
		"data": stats,
	})
}

// handleTaskRetry 重试任务
// POST /api/system/async_task/:task_id/retry
func (app *App) handleTaskRetry(c *gin.Context) {
	taskID := c.Param("task_id")

	if err := app.TaskManager.RetryTask(taskID); err != nil {
		c.JSON(http.StatusOK, gin.H{
			"code": 400,
			"msg":  "重试任务失败: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"code": 200,
		"msg":  "任务已重置为待执行状态",
		"data": gin.H{
			"task_id": taskID,
		},
	})
}
