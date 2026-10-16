// Package task 任务管理器
package task

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/data"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/logger"
)

// Manager 任务管理器
type Manager struct {
	db       *data.DB
	tasks    sync.Map       // 内存任务存储 map[string]*Task
	executor *Executor      // 任务执行器
	mu       sync.RWMutex   // 保护管理器状态
	stopCh   chan struct{}  // 停止信号
	wg       sync.WaitGroup // 等待所有任务完成

	// log 业务日志（可为 nil：未注入时静默，不影响任务执行）
	log *logger.Logger
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

// SetLogger 注入业务日志器，用于记录任务全生命周期状态。
func (m *Manager) SetLogger(l *logger.Logger) {
	m.log = l
}

// taskEvent 构造任务维度的日志事件。
//
// log 为 nil 时返回的事件不会落地（Event 返回的构建器在 logger 为 nil 时静默丢弃），
// 因此调用方无需判空。
func (m *Manager) taskEvent(action string, t *Task) *logger.Event {
	ev := m.log.Event("task", action)
	if t == nil {
		return ev
	}
	ev.User(t.CreatedBy).Host(t.HsName)
	if t.VmUUID != "" {
		ev.VM(t.VmUUID, "")
	}
	return ev.Detail("task_id", t.ID).
		Detail("task_type", string(t.Type)).
		Detail("task_name", t.Name)
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

// cloneTask 复制任务快照。
//
// sync.Map 只保护映射本身，不保护其中的 *Task 字段。
// 把内部指针交给调用方（HTTP 处理器、执行器）会让其状态
// 与 manager 的写入形成数据竞争（已由 -race 复现）。
func cloneTask(src *Task) *Task {
	if src == nil {
		return nil
	}
	dst := *src
	if src.Result != nil {
		dst.Result = make(map[string]interface{}, len(src.Result))
		for k, v := range src.Result {
			dst.Result[k] = v
		}
	}
	if src.Metadata != nil {
		dst.Metadata = make(map[string]interface{}, len(src.Metadata))
		for k, v := range src.Metadata {
			dst.Metadata[k] = v
		}
	}
	if src.StartedAt != nil {
		t := *src.StartedAt
		dst.StartedAt = &t
	}
	if src.CompletedAt != nil {
		t := *src.CompletedAt
		dst.CompletedAt = &t
	}
	return &dst
}

// CreateTask 创建新任务
func (m *Manager) CreateTask(name string, taskType TaskType, createdBy string) (*Task, error) {
	task := NewTask(name, taskType, createdBy)

	m.mu.Lock()
	defer m.mu.Unlock()

	// 保存到数据库
	if err := m.saveTask(task); err != nil {
		return nil, fmt.Errorf("保存任务失败: %w", err)
	}

	// 存入内存
	m.tasks.Store(task.ID, task)

	// 任务创建与「接口已受理」是同一件事：接口层已用 INFO 记录业务语义，
	// 这里降到 DEBUG 只落文件，避免同一动作在日志页面重复出现。
	m.taskEvent("任务创建", task).Debug("任务已创建：%s", task.Name)

	return cloneTask(task), nil
}

// CreateVMTask 创建带主机与虚拟机标识的任务。
//
// 标识必须在写入内存与数据库之前设定：CreateTask 返回的是快照，
// 之后再修改调用方持有的副本不会影响已存储的任务。
func (m *Manager) CreateVMTask(name string, taskType TaskType, createdBy,
	hsName, vmUUID string, metadata map[string]interface{}) (*Task, error) {

	t := NewTask(name, taskType, createdBy)
	t.HsName = hsName
	t.VmUUID = vmUUID
	if metadata != nil {
		t.Metadata = metadata
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.saveTask(t); err != nil {
		return nil, fmt.Errorf("保存任务失败: %w", err)
	}
	m.tasks.Store(t.ID, t)

	// 同上：接口层已记录「提交创建/删除/更新任务」，此处只做文件级留痕
	m.taskEvent("任务创建", t).Debug("虚拟机任务已创建：%s", t.Name)

	return cloneTask(t), nil
}

// SubmitTask 提交任务到执行队列
func (m *Manager) SubmitTask(taskID string, executor TaskExecutor, metadata map[string]interface{}) error {
	m.mu.Lock()
	taskVal, ok := m.tasks.Load(taskID)
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("任务不存在: %s", taskID)
	}

	task := taskVal.(*Task)
	if task.Status != TaskStatusPending {
		m.mu.Unlock()
		return fmt.Errorf("任务状态不正确: %s (当前: %s)", taskID, task.Status)
	}

	// 更新元数据
	if metadata != nil {
		task.Metadata = metadata
		if err := m.saveTask(task); err != nil {
			m.mu.Unlock()
			return fmt.Errorf("更新任务元数据失败: %w", err)
		}
	}
	m.mu.Unlock()

	// 提交到执行器（在锁外执行，避免队列满时阻塞其它任务状态操作）
	m.executor.Submit(task, executor)

	snapshot := cloneTask(task)
	m.taskEvent("任务入队", snapshot).Debug("任务已提交执行队列（当前队列 %d 项）", m.executor.GetQueueSize())

	return nil
}

// GetTask 获取任务
func (m *Manager) GetTask(taskID string) (*Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 先从内存查找（返回快照，避免调用方直接改内部状态）
	if taskVal, ok := m.tasks.Load(taskID); ok {
		return cloneTask(taskVal.(*Task)), nil
	}

	// 从数据库查找
	task, err := m.loadTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("任务不存在: %s", taskID)
	}

	// 存入内存
	m.tasks.Store(task.ID, task)

	return cloneTask(task), nil
}

// ListTasks 列出任务
func (m *Manager) ListTasks(filter TaskFilter) ([]*Task, int, error) {
	// 转换为 data.TaskFilter
	dataFilter := data.TaskFilter{
		Status:    make([]data.TaskStatus, len(filter.Status)),
		Type:      make([]data.TaskType, len(filter.Type)),
		CreatedBy: filter.CreatedBy,
		HsName:    filter.HsName,
		VmUUID:    filter.VmUUID,
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
			HsName:      dt.HsName,
			VmUUID:      dt.VmUUID,
			CreatedAt:   dt.CreatedAt,
			StartedAt:   dt.StartedAt,
			CompletedAt: dt.CompletedAt,
		}
	}

	return tasks, total, nil
}

// UpdateProgress 更新任务进度
func (m *Manager) UpdateProgress(taskID string, progress int, message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	taskVal, ok := m.tasks.Load(taskID)
	if !ok {
		return fmt.Errorf("任务不存在: %s", taskID)
	}

	task := taskVal.(*Task)
	// 终态任务不再接受进度更新，避免已完成/已取消的任务被重新写成运行态
	if task.IsTerminal() {
		return nil
	}
	task.Progress = progress
	task.Message = message

	// 保存到数据库
	if err := m.saveTask(task); err != nil {
		return err
	}
	// 进度属于高频低价值信息：Debug 级别只落文件，不污染审计日志
	m.taskEvent("任务进度", task).Debug("进度 %d%%：%s", progress, message)
	return nil
}

// CompleteTask 完成任务
func (m *Manager) CompleteTask(taskID string, result map[string]interface{}, err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	taskVal, ok := m.tasks.Load(taskID)
	if !ok {
		return fmt.Errorf("任务不存在: %s", taskID)
	}

	task := taskVal.(*Task)
	// 已取消的任务不应被完成事件覆盖：
	// 否则用户取消后任务会“复活”为完成态，状态机不可信。
	if task.IsTerminal() {
		return nil
	}

	if result == nil {
		result = task.Result
	}

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
	saveErr := m.saveTask(task)

	if task.Status == TaskStatusFailed {
		m.taskEvent("任务失败", task).
			Detail("error", task.Error).
			Errorf("任务执行失败：%s（%s）", task.Name, task.Error)
	} else {
		ev := m.taskEvent("任务完成", task)
		if task.StartedAt != nil {
			ev.Duration(task.CompletedAt.Sub(*task.StartedAt))
		}
		ev.Info("任务执行成功：%s", task.Name)
	}
	return saveErr
}

// CancelTask 取消任务
func (m *Manager) CancelTask(taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

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

	task.Status = TaskStatusStopped
	task.Message = "已终止"
	now := time.Now()
	task.CompletedAt = &now

	// 保存到数据库
	if err := m.saveTask(task); err != nil {
		return err
	}
	m.taskEvent("任务终止", task).Warn("任务已被取消：%s", task.Name)
	return nil
}

// MarkTaskRunning 标记任务为运行中
func (m *Manager) MarkTaskRunning(taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	taskVal, ok := m.tasks.Load(taskID)
	if !ok {
		return fmt.Errorf("任务不存在: %s", taskID)
	}

	task := taskVal.(*Task)
	// 任务在排队期间可能已被取消：此时不得再标记为运行中
	if task.IsTerminal() {
		return fmt.Errorf("任务已终止，无法开始执行: %s", taskID)
	}

	task.Status = TaskStatusRunning
	now := time.Now()
	task.StartedAt = &now
	task.Message = "执行中"

	// 保存到数据库
	if err := m.saveTask(task); err != nil {
		return err
	}
	m.taskEvent("任务开始", task).Info("开始执行任务：%s", task.Name)
	return nil
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
		task.HsName,
		task.VmUUID,
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
		HsName:      dt.HsName,
		VmUUID:      dt.VmUUID,
		CreatedAt:   dt.CreatedAt,
		StartedAt:   dt.StartedAt,
		CompletedAt: dt.CompletedAt,
	}, nil
}

// loadPendingTasks 从数据库加载待处理任务
func (m *Manager) loadPendingTasks() error {
	m.mu.Lock()
	defer m.mu.Unlock()

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
			HsName:      dt.HsName,
			VmUUID:      dt.VmUUID,
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

	if len(dataTasks) > 0 {
		m.log.Event("task", "任务恢复").Detail("count", len(dataTasks)).
			Info("已从数据库恢复 %d 条未完成任务", len(dataTasks))
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
	m.mu.Lock()
	defer m.mu.Unlock()

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

// HasActiveTaskForVM 判断指定主机上的虚拟机是否已有未完成任务。
//
// 与 Python 版 has_running_task_for_vm 语义一致：同一虚拟机同时只允许
// 一个 pending/running 任务，避免对同一虚拟机并发执行创建/删除/更新。
func (m *Manager) HasActiveTaskForVM(hsName, vmUUID string) bool {
	if hsName == "" || vmUUID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	found := false
	m.tasks.Range(func(key, value interface{}) bool {
		task := value.(*Task)
		if task.HsName != hsName || task.VmUUID != vmUUID {
			return true
		}
		if task.Status == TaskStatusPending || task.Status == TaskStatusRunning {
			found = true
			return false
		}
		return true
	})
	if found {
		return true
	}

	// 内存未命中时回查数据库，覆盖重启后尚未加载的乐观场景
	tasks, _, err := m.db.ListTasks(data.TaskFilter{
		HsName: hsName,
		VmUUID: vmUUID,
		Status: []data.TaskStatus{data.TaskStatusPending, data.TaskStatusRunning},
		Limit:  1,
	})
	return err == nil && len(tasks) > 0
}

// GetStats 获取任务统计
func (m *Manager) GetStats() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 键名与 Python 版 /api/system/async_task/stats 保持一致
	stats := map[string]int{
		"total":     0,
		"pending":   0,
		"running":   0,
		"completed": 0,
		"failed":    0,
		"stopped":   0,
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
		case TaskStatusStopped, TaskStatusCancelled:
			stats["stopped"]++
		}
		return true
	})

	return stats
}

// RetryTask 重试已失败或取消的任务
func (m *Manager) RetryTask(taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	taskVal, ok := m.tasks.Load(taskID)
	if !ok {
		return fmt.Errorf("任务不存在: %s", taskID)
	}

	task := taskVal.(*Task)

	// 只能重试失败或已终止的任务（与 Python 版 retry_task 对 stopped 生效一致）
	if task.Status != TaskStatusFailed &&
		task.Status != TaskStatusStopped &&
		task.Status != TaskStatusCancelled {
		return fmt.Errorf("只能重试失败或已终止的任务，当前状态: %s", task.Status)
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

	m.taskEvent("任务重试", task).Info("任务已重置为待执行：%s", task.Name)

	// 注意：重试需要原始的执行器，但当前设计中执行器未存储
	// 这里返回成功，但实际需要调用方重新提交（使用保存的metadata）
	return nil
}
