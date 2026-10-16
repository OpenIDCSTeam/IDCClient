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
	TaskStatusFailed    TaskStatus = "failed"    // 失败
	// TaskStatusStopped 与 Python 版 TaskManager.stop_task 一致：
	// 用户终止的任务状态是 stopped，而不是 cancelled。
	TaskStatusStopped TaskStatus = "stopped"
	// TaskStatusCancelled 兼容历史数据
	TaskStatusCancelled TaskStatus = "cancelled"
)

// TaskType 任务类型
type TaskType string

const (
	TaskTypeVMCreate    TaskType = "create_vm"    // 创建虚拟机
	TaskTypeVMReinstall TaskType = "reinstall_vm" // 重装虚拟机
	TaskTypeVMDelete    TaskType = "delete_vm"    // 删除虚拟机
	TaskTypeVMBackup    TaskType = "backup_vm"    // 备份虚拟机
	TaskTypeVMRestore   TaskType = "restore_vm"   // 还原虚拟机
	TaskTypeVMSnapshot  TaskType = "snapshot_vm"  // 创建快照
	TaskTypeVMClone     TaskType = "clone_vm"     // 克隆虚拟机
	TaskTypeVMUpdate    TaskType = "update_vm"    // 更新虚拟机配置
	TaskTypeVMPassword  TaskType = "password_vm"  // 修改密码
	TaskTypeVMPower     TaskType = "power_vm"     // 电源操作
	TaskTypeHostScan    TaskType = "scan_host"    // 扫描主机
	TaskTypeBackupScan  TaskType = "scan_backup"  // 扫描备份
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
	HsName      string                 `json:"hs_name"`      // 目标主机
	VmUUID      string                 `json:"vm_uuid"`      // 目标虚拟机
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
	HsName    string       `json:"hs_name"`    // 主机过滤
	VmUUID    string       `json:"vm_uuid"`    // 虚拟机过滤
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

// MarshalJSON 输出与 Python 版任务字典一致的字段。
//
// Python 任务字段：task_id, hs_name, vm_uuid, task_type, status,
// params, result, error_message, created_at, started_at, finished_at。
// 同时保留 Go 侧原有字段，避免已有调用方解析失败。
func (t *Task) MarshalJSON() ([]byte, error) {
	out := map[string]interface{}{
		"task_id":       t.ID,
		"id":            t.ID,
		"name":          t.Name,
		"task_type":     string(t.Type),
		"type":          string(t.Type),
		"status":        string(t.Status),
		"progress":      t.Progress,
		"message":       t.Message,
		"result":        t.Result,
		"error":         t.Error,
		"error_message": t.Error,
		"params":        t.Metadata,
		"metadata":      t.Metadata,
		"created_by":    t.CreatedBy,
		"created_at":    t.CreatedAt,
		"started_at":    t.StartedAt,
		"finished_at":   t.CompletedAt,
		"completed_at":  t.CompletedAt,
	}
	// 主机与虚拟机标识存放在 Metadata 中，提升为顶层字段供前端直接使用
	if t.Metadata != nil {
		if v, ok := t.Metadata["hs_name"].(string); ok {
			out["hs_name"] = v
		}
		if v, ok := t.Metadata["vm_uuid"].(string); ok {
			out["vm_uuid"] = v
		}
	}
	return json.Marshal(out)
}

// IsTerminal 判断任务是否已终止（完成、失败或已终止）
func (t *Task) IsTerminal() bool {
	return t.Status == TaskStatusCompleted ||
		t.Status == TaskStatusFailed ||
		t.Status == TaskStatusStopped ||
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
