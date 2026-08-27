// Package task 异步任务系统
package task

import (
	"encoding/json"
	"time"
)

// TaskStatus 任务状态
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"    // 等待执行
	TaskStatusRunning   TaskStatus = "running"    // 执行中
	TaskStatusCompleted TaskStatus = "completed"  // 已完成
	TaskStatusFailed    TaskStatus = "failed"     // 失败
	TaskStatusCancelled TaskStatus = "cancelled"  // 已取消
)

// TaskType 任务类型
type TaskType string

const (
	TaskTypeVMCreate    TaskType = "vm.create"     // 创建虚拟机
	TaskTypeVMReinstall TaskType = "vm.reinstall"  // 重装虚拟机
	TaskTypeVMDelete    TaskType = "vm.delete"     // 删除虚拟机
	TaskTypeVMBackup    TaskType = "vm.backup"     // 备份虚拟机
	TaskTypeVMRestore   TaskType = "vm.restore"    // 还原虚拟机
	TaskTypeVMSnapshot  TaskType = "vm.snapshot"   // 创建快照
	TaskTypeVMClone     TaskType = "vm.clone"      // 克隆虚拟机
	TaskTypeVMUpdate    TaskType = "vm.update"     // 更新虚拟机配置
	TaskTypeHostScan    TaskType = "host.scan"     // 扫描主机
	TaskTypeBackupScan  TaskType = "backup.scan"   // 扫描备份
)

// Task 任务模型
type Task struct {
	ID          string                 `json:"id"`           // 任务ID (UUID)
	Name        string                 `json:"name"`         // 任务名称
	Type        TaskType               `json:"type"`         // 任务类型
	Status      TaskStatus             `json:"status"`       // 任务状态
	Progress    int                    `json:"progress"`     // 进度 (0-100)
	Message     string                 `json:"message"`      // 当前状态消息
	Result      map[string]interface{} `json:"result"`       // 执行结果 (JSON)
	Error       string                 `json:"error"`        // 错误信息
	Metadata    map[string]interface{} `json:"metadata"`     // 任务元数据 (JSON)
	CreatedBy   string                 `json:"created_by"`   // 创建用户
	CreatedAt   time.Time              `json:"created_at"`   // 创建时间
	StartedAt   *time.Time             `json:"started_at"`   // 开始时间
	CompletedAt *time.Time             `json:"completed_at"` // 完成时间
}

// NewTask 创建新任务
func NewTask(name string, taskType TaskType, createdBy string) *Task {
	return &Task{
		ID:        generateTaskID(),
		Name:      name,
		Type:      taskType,
		Status:    TaskStatusPending,
		Progress:  0,
		Message:   "等待执行",
		Result:    make(map[string]interface{}),
		Metadata:  make(map[string]interface{}),
		CreatedBy: createdBy,
		CreatedAt: time.Now(),
	}
}

// TaskFilter 任务查询过滤器
type TaskFilter struct {
	Status    []TaskStatus `json:"status"`     // 状态过滤
	Type      []TaskType   `json:"type"`       // 类型过滤
	CreatedBy string       `json:"created_by"` // 创建用户过滤
	Limit     int          `json:"limit"`      // 返回数量限制
	Offset    int          `json:"offset"`     // 分页偏移
}

// ToJSON 转换为JSON字符串
func (t *Task) ToJSON() (string, error) {
	data, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// FromJSON 从JSON字符串解析
func FromJSON(jsonStr string) (*Task, error) {
	var task Task
	if err := json.Unmarshal([]byte(jsonStr), &task); err != nil {
		return nil, err
	}
	return &task, nil
}

// IsTerminal 判断任务是否已终止（完成、失败或取消）
func (t *Task) IsTerminal() bool {
	return t.Status == TaskStatusCompleted ||
		t.Status == TaskStatusFailed ||
		t.Status == TaskStatusCancelled
}

// IsRunning 判断任务是否正在运行
func (t *Task) IsRunning() bool {
	return t.Status == TaskStatusRunning
}

// Duration 获取任务执行时长
func (t *Task) Duration() time.Duration {
	if t.StartedAt == nil {
		return 0
	}
	if t.CompletedAt != nil {
		return t.CompletedAt.Sub(*t.StartedAt)
	}
	return time.Since(*t.StartedAt)
}

// generateTaskID 生成任务ID
func generateTaskID() string {
	return time.Now().Format("20060102150405") + "-" + randomString(8)
}

// randomString 生成随机字符串
func randomString(length int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	result := make([]byte, length)
	for i := range result {
		result[i] = charset[time.Now().UnixNano()%int64(len(charset))]
	}
	return string(result)
}
