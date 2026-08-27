// Package task 任务管理器
package task

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
)

// Manager 任务管理器
type Manager struct {
	db       *data.DB
	tasks    sync.Map           // 内存任务存储 map[string]*Task
	executor *Executor          // 任务执行器
	mu       sync.RWMutex       // 保护管理器状态
	stopCh   chan struct{}      // 停止信号
	wg       sync.WaitGroup     // 等待所有任务完成
}

// NewManager 创建任务管理器
func NewManager(db *data.DB, workers int) *Manager {
	m := &Manager{
		db:     db,
		stopCh: make(chan struct{}),
	}
	m.executor = NewExecutor(m, workers)
	return m
}

// Start 启动任务管理器
func (m *Manager) Start() error {
	// 从数据库恢复未完成的任务
	if err := m.loadPendingTasks(); err != nil {
		return fmt.Errorf("加载待处理任务失败: %w", err)
	}

	// 启动执行器
	m.executor.Start()

	// 启动清理协程
	m.wg.Add(1)
	go m.cleanupLoop()

	return nil
}

// Stop 停止任务管理器
func (m *Manager) Stop() error {
	close(m.stopCh)
	m.executor.Stop()
	m.wg.Wait()
	return nil
}

// CreateTask 创建新任务
func (m *Manager) CreateTask(name string, taskType TaskType, createdBy string) (*Task, error) {
	task := NewTask(name, taskType, createdBy)

	// 保存到数据库
	if err := m.saveTask(task); err != nil {
		return nil, fmt.Errorf("保存任务失败: %w", err)
	}

	// 存入内存
	m.tasks.Store(task.ID, task)

	return task, nil
}

// SubmitTask 提交任务到执行队列
func (m *Manager) SubmitTask(taskID string, executor TaskExecutor, metadata map[string]interface{}) error {
	taskVal, ok := m.tasks.Load(taskID)
	if !ok {
		return fmt.Errorf("任务不存在: %s", taskID)
	}

	task := taskVal.(*Task)
	if task.Status != TaskStatusPending {
		return fmt.Errorf("任务状态不正确: %s (当前: %s)", taskID, task.Status)
	}

	// 更新元数据
	if metadata != nil {
		task.Metadata = metadata
		if err := m.saveTask(task); err != nil {
			return fmt.Errorf("更新任务元数据失败: %w", err)
		}
	}

	// 提交到执行器
	m.executor.Submit(task, executor)

	return nil
}

// GetTask 获取任务
func (m *Manager) GetTask(taskID string) (*Task, error) {
	// 先从内存查找
	if taskVal, ok := m.tasks.Load(taskID); ok {
		return taskVal.(*Task), nil
	}

	// 从数据库查找
	task, err := m.loadTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("任务不存在: %s", taskID)
	}

	// 存入内存
	m.tasks.Store(task.ID, task)

	return task, nil
}

// ListTasks 列出任务
func (m *Manager) ListTasks(filter TaskFilter) ([]*Task, int, error) {
	// 转换为 data.TaskFilter
	dataFilter := data.TaskFilter{
		Status:    make([]data.TaskStatus, len(filter.Status)),
		Type:      make([]data.TaskType, len(filter.Type)),
		CreatedBy: filter.CreatedBy,
		Limit:     filter.Limit,
		Offset:    filter.Offset,
	}
	
	for i, s := range filter.Status {
		dataFilter.Status[i] = data.TaskStatus(s)
	}
	for i, t := range filter.Type {
		dataFilter.Type[i] = data.TaskType(t)
	}
	
	dataTasks, total, err := m.db.ListTasks(dataFilter)
	if err != nil {
		return nil, 0, err
	}
	
	// 转换为 task.Task
	tasks := make([]*Task, len(dataTasks))
	for i, dt := range dataTasks {
		tasks[i] = &Task{
			ID:          dt.ID,
			Name:        dt.Name,
			Type:        TaskType(dt.Type),
			Status:      TaskStatus(dt.Status),
			Progress:    dt.Progress,
			Message:     dt.Message,
			Result:      dt.Result,
			Error:       dt.Error,
			Metadata:    dt.Metadata,
			CreatedBy:   dt.CreatedBy,
			CreatedAt:   dt.CreatedAt,
			StartedAt:   dt.StartedAt,
			CompletedAt: dt.CompletedAt,
		}
	}
	
	return tasks, total, nil
}

// UpdateProgress 更新任务进度
func (m *Manager) UpdateProgress(taskID string, progress int, message string) error {
	taskVal, ok := m.tasks.Load(taskID)
	if !ok {
		return fmt.Errorf("任务不存在: %s", taskID)
	}

	task := taskVal.(*Task)
	task.Progress = progress
	task.Message = message

	// 保存到数据库
	return m.saveTask(task)
}

// CompleteTask 完成任务
func (m *Manager) CompleteTask(taskID string, result map[string]interface{}, err error) error {
	taskVal, ok := m.tasks.Load(taskID)
	if !ok {
		return fmt.Errorf("任务不存在: %s", taskID)
	}

	task := taskVal.(*Task)
	now := time.Now()
	task.CompletedAt = &now
	task.Progress = 100

	if err != nil {
		task.Status = TaskStatusFailed
		task.Error = err.Error()
		task.Message = "执行失败"
	} else {
		task.Status = TaskStatusCompleted
		task.Result = result
		task.Message = "执行完成"
	}

	// 保存到数据库
	return m.saveTask(task)
}

// CancelTask 取消任务
func (m *Manager) CancelTask(taskID string) error {
	taskVal, ok := m.tasks.Load(taskID)
	if !ok {
		return fmt.Errorf("任务不存在: %s", taskID)
	}

	task := taskVal.(*Task)
	if task.IsTerminal() {
		return fmt.Errorf("任务已终止，无法取消")
	}

	// 如果任务正在运行，通知执行器取消
	if task.IsRunning() {
		m.executor.Cancel(taskID)
	}

	task.Status = TaskStatusCancelled
	task.Message = "已取消"
	now := time.Now()
	task.CompletedAt = &now

	// 保存到数据库
	return m.saveTask(task)
}

// MarkTaskRunning 标记任务为运行中
func (m *Manager) MarkTaskRunning(taskID string) error {
	taskVal, ok := m.tasks.Load(taskID)
	if !ok {
		return fmt.Errorf("任务不存在: %s", taskID)
	}

	task := taskVal.(*Task)
	task.Status = TaskStatusRunning
	now := time.Now()
	task.StartedAt = &now
	task.Message = "执行中"

	// 保存到数据库
	return m.saveTask(task)
}

// saveTask 保存任务到数据库
func (m *Manager) saveTask(task *Task) error {
	resultJSON, _ := json.Marshal(task.Result)
	metadataJSON, _ := json.Marshal(task.Metadata)

	return m.db.SaveTask(
		task.ID,
		task.Name,
		string(task.Type),
		string(task.Status),
		task.Progress,
		task.Message,
		string(resultJSON),
		task.Error,
		string(metadataJSON),
		task.CreatedBy,
		task.CreatedAt,
		task.StartedAt,
		task.CompletedAt,
	)
}

// loadTask 从数据库加载任务
func (m *Manager) loadTask(taskID string) (*Task, error) {
	dt, err := m.db.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	
	return &Task{
		ID:          dt.ID,
		Name:        dt.Name,
		Type:        TaskType(dt.Type),
		Status:      TaskStatus(dt.Status),
		Progress:    dt.Progress,
		Message:     dt.Message,
		Result:      dt.Result,
		Error:       dt.Error,
		Metadata:    dt.Metadata,
		CreatedBy:   dt.CreatedBy,
		CreatedAt:   dt.CreatedAt,
		StartedAt:   dt.StartedAt,
		CompletedAt: dt.CompletedAt,
	}, nil
}

// loadPendingTasks 从数据库加载待处理任务
func (m *Manager) loadPendingTasks() error {
	dataTasks, _, err := m.db.ListTasks(data.TaskFilter{
		Status: []data.TaskStatus{data.TaskStatusPending, data.TaskStatusRunning},
		Limit:  1000,
	})
	if err != nil {
		return err
	}

	for _, dt := range dataTasks {
		task := &Task{
			ID:          dt.ID,
			Name:        dt.Name,
			Type:        TaskType(dt.Type),
			Status:      TaskStatus(dt.Status),
			Progress:    dt.Progress,
			Message:     dt.Message,
			Result:      dt.Result,
			Error:       dt.Error,
			Metadata:    dt.Metadata,
			CreatedBy:   dt.CreatedBy,
			CreatedAt:   dt.CreatedAt,
			StartedAt:   dt.StartedAt,
			CompletedAt: dt.CompletedAt,
		}
		
		// 将运行中的任务重置为待处理（异常退出导致）
		if task.Status == TaskStatusRunning {
			task.Status = TaskStatusPending
			task.Message = "等待重新执行"
			m.saveTask(task)
		}
		m.tasks.Store(task.ID, task)
	}

	return nil
}

// cleanupLoop 定期清理已完成的内存任务
func (m *Manager) cleanupLoop() {
	defer m.wg.Done()

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			m.cleanup()
		case <-m.stopCh:
			return
		}
	}
}

// cleanup 清理内存中超过1小时的已完成任务
func (m *Manager) cleanup() {
	now := time.Now()
	var toDelete []string

	m.tasks.Range(func(key, value interface{}) bool {
		task := value.(*Task)
		if task.IsTerminal() && task.CompletedAt != nil {
			if now.Sub(*task.CompletedAt) > time.Hour {
				toDelete = append(toDelete, task.ID)
			}
		}
		return true
	})

	for _, id := range toDelete {
		m.tasks.Delete(id)
	}
}

// GetStats 获取任务统计
func (m *Manager) GetStats() map[string]int {
	stats := map[string]int{
		"total":     0,
		"pending":   0,
		"running":   0,
		"completed": 0,
		"failed":    0,
		"cancelled": 0,
	}

	m.tasks.Range(func(key, value interface{}) bool {
		task := value.(*Task)
		stats["total"]++
		switch task.Status {
		case TaskStatusPending:
			stats["pending"]++
		case TaskStatusRunning:
			stats["running"]++
		case TaskStatusCompleted:
			stats["completed"]++
		case TaskStatusFailed:
			stats["failed"]++
		case TaskStatusCancelled:
			stats["cancelled"]++
		}
		return true
	})

	return stats
}

// RetryTask 重试已失败或取消的任务
func (m *Manager) RetryTask(taskID string) error {
	taskVal, ok := m.tasks.Load(taskID)
	if !ok {
		return fmt.Errorf("任务不存在: %s", taskID)
	}

	task := taskVal.(*Task)
	
	// 只能重试失败或已取消的任务
	if task.Status != TaskStatusFailed && task.Status != TaskStatusCancelled {
		return fmt.Errorf("只能重试失败或已取消的任务，当前状态: %s", task.Status)
	}

	// 重置任务状态
	task.Status = TaskStatusPending
	task.Progress = 0
	task.Message = "等待重新执行"
	task.Error = ""
	task.StartedAt = nil
	task.CompletedAt = nil
	
	// 保存到数据库
	if err := m.saveTask(task); err != nil {
		return fmt.Errorf("保存任务状态失败: %w", err)
	}

	// 注意：重试需要原始的执行器，但当前设计中执行器未存储
	// 这里返回成功，但实际需要调用方重新提交（使用保存的metadata）
	return nil
}
