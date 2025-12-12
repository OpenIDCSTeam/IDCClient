package models

import (
	"encoding/json"
	"time"
)

// ZMessage 消息结构体
type ZMessage struct {
	Success bool        `json:"success"`
	Action  string      `json:"actions"`
	Message string      `json:"message"`
	Results interface{} `json:"results,omitempty"`
	Execute interface{} `json:"execute,omitempty"`
	Level   LogLevel    `json:"level,omitempty"`
	Time    time.Time   `json:"time"`
}

// LogLevel 日志级别
type LogLevel string

const (
	LogLevelInfo    LogLevel = "INFO"
	LogLevelWarning LogLevel = "WARNING"
	LogLevelError   LogLevel = "ERROR"
	LogLevelDebug   LogLevel = "DEBUG"
)

// LogEntry 日志条目
type LogEntry struct {
	ID        int       `json:"id" db:"id"`
	HSName    string    `json:"hs_name" db:"hs_name"`
	LogData   string    `json:"log_data" db:"log_data"`
	LogLevel  LogLevel  `json:"log_level" db:"log_level"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}

// TaskEntry 任务条目
type TaskEntry struct {
	ID        int                    `json:"id" db:"id"`
	HSName    string                 `json:"hs_name" db:"hs_name"`
	TaskData  map[string]interface{} `json:"task_data" db:"task_data"`
	Status    string                 `json:"status" db:"status"`
	CreatedAt time.Time              `json:"created_at" db:"created_at"`
	UpdatedAt *time.Time             `json:"updated_at,omitempty" db:"updated_at"`
}

// APIResponse API响应结构体
type APIResponse struct {
	Code int         `json:"code"`
	Msg  string      `json:"msg"`
	Data interface{} `json:"data"`
}

// SystemStats 系统统计
type SystemStats struct {
	HostCount       int `json:"host_count"`
	VMCount         int `json:"vm_count"`
	RunningVMCount  int `json:"running_vm_count"`
	StoppedVMCount  int `json:"stopped_vm_count"`
	ErrorVMCount    int `json:"error_vm_count"`
	TotalCPU        int64 `json:"total_cpu"`
	TotalMemory     int64 `json:"total_memory"`
	TotalDisk       int64 `json:"total_disk"`
	AllocatedIPs    int   `json:"allocated_ips"`
	TotalPorts      int   `json:"total_ports"`
	UsedPorts       int   `json:"used_ports"`
}

// HostInfo 主机信息
type HostInfo struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Addr     string   `json:"addr"`
	VMCount  int      `json:"vm_count"`
	Status   string   `json:"status"`
	Config   HSConfig `json:"config"`
	VMList   []string `json:"vm_list"`
	LastUpdated int64 `json:"last_updated"`
}

// VMInfo 虚拟机信息
type VMInfo struct {
	UUID         string            `json:"uuid"`
	Name         string            `json:"name"`
	OSName       string            `json:"os_name"`
	CPU          int               `json:"cpu"`
	Memory       int64             `json:"memory"`
	Disk         int64             `json:"disk"`
	Status       string            `json:"status"`
	Config       VMConfig          `json:"config"`
	StatusList   []HWStatus        `json:"status_list"`
	Networks     []NCConfig        `json:"networks"`
	PortForwards []PortData        `json:"port_forwards"`
	Proxies      []WebProxy        `json:"proxies"`
	VNCURL       string            `json:"vnc_url,omitempty"`
	LastSeen     *time.Time        `json:"last_seen,omitempty"`
}

// EngineConfig 引擎配置
type EngineConfig struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Enabled     bool              `json:"enabled"`
	Platform    []string          `json:"platform"`
	Arch        []string          `json:"arch"`
	Options     map[string]string `json:"options"`
	Message     string            `json:"message"`
	Imported    string            `json:"imported"`
}

// NewZMessage 创建新的消息
func NewZMessage(success bool, action, message string) *ZMessage {
	return &ZMessage{
		Success: success,
		Action:  action,
		Message: message,
		Time:    time.Now(),
		Level:   func() LogLevel {
			if success {
				return LogLevelInfo
			}
			return LogLevelError
		}(),
	}
}

// NewZMessageWithData 创建带数据的消息
func NewZMessageWithData(success bool, action, message string, results interface{}) *ZMessage {
	return &ZMessage{
		Success: success,
		Action:  action,
		Message: message,
		Results: results,
		Time:    time.Now(),
		Level:   func() LogLevel {
			if success {
				return LogLevelInfo
			}
			return LogLevelError
		}(),
	}
}

// ToJSON 将ZMessage转换为JSON
func (z *ZMessage) ToJSON() string {
	data, _ := json.Marshal(z)
	return string(data)
}

// NewAPIResponse 创建API响应
func NewAPIResponse(code int, msg string, data interface{}) *APIResponse {
	return &APIResponse{
		Code: code,
		Msg:  msg,
		Data: data,
	}
}

// NewSuccessResponse 创建成功响应
func NewSuccessResponse(data interface{}) *APIResponse {
	return NewAPIResponse(200, "success", data)
}

// NewErrorResponse 创建错误响应
func NewErrorResponse(code int, msg string) *APIResponse {
	return NewAPIResponse(code, msg, nil)
}

// ToJSON 将APIResponse转换为JSON
func (a *APIResponse) ToJSON() string {
	data, _ := json.Marshal(a)
	return string(data)
}