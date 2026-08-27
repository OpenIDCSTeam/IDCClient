// Package api 任务管理 API
package api

import (
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/task"
)

// TaskHandler 任务处理器（已废弃，使用 task_api.go 中的 Gin 处理器）
type TaskHandler struct {
	manager *task.Manager
}

// NewTaskHandler 创建任务处理器
func NewTaskHandler(manager *task.Manager) *TaskHandler {
	return &TaskHandler{
		manager: manager,
	}
}
