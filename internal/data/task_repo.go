// Package data 任务数据访问层
package data

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// TaskStatus 任务状态
type TaskStatus string

const (
	TaskStatusPending   TaskStatus = "pending"
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusCompleted TaskStatus = "completed"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusCancelled TaskStatus = "cancelled"
)

// TaskType 任务类型
type TaskType string

// Task 任务模型
type Task struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	Type        string                 `json:"type"`
	Status      string                 `json:"status"`
	Progress    int                    `json:"progress"`
	Message     string                 `json:"message"`
	Result      map[string]interface{} `json:"result"`
	Error       string                 `json:"error"`
	Metadata    map[string]interface{} `json:"metadata"`
	CreatedBy   string                 `json:"created_by"`
	CreatedAt   time.Time              `json:"created_at"`
	StartedAt   *time.Time             `json:"started_at"`
	CompletedAt *time.Time             `json:"completed_at"`
}

// TaskFilter 任务过滤器
type TaskFilter struct {
	Status    []TaskStatus `json:"status"`
	Type      []TaskType   `json:"type"`
	CreatedBy string       `json:"created_by"`
	Limit     int          `json:"limit"`
	Offset    int          `json:"offset"`
}

// SaveTask 保存任务
func (d *DB) SaveTask(
	id, name, taskType, status string,
	progress int,
	message, result, errorMsg, metadata, createdBy string,
	createdAt time.Time,
	startedAt, completedAt *time.Time,
) error {
	query := `
		INSERT INTO async_tasks (
			id, name, type, status, progress, message, 
			result, error, metadata, created_by, 
			created_at, started_at, completed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			status = excluded.status,
			progress = excluded.progress,
			message = excluded.message,
			result = excluded.result,
			error = excluded.error,
			metadata = excluded.metadata,
			started_at = excluded.started_at,
			completed_at = excluded.completed_at
	`

	_, err := d.db.Exec(
		query,
		id, name, taskType, status, progress, message,
		result, errorMsg, metadata, createdBy,
		createdAt, startedAt, completedAt,
	)
	return err
}

// GetTask 获取任务
func (d *DB) GetTask(taskID string) (*Task, error) {
	query := `
		SELECT id, name, type, status, progress, message,
		       result, error, metadata, created_by,
		       created_at, started_at, completed_at
		FROM async_tasks
		WHERE id = ?
	`

	var task Task
	var resultJSON, metadataJSON string
	var startedAt, completedAt sql.NullTime

	err := d.db.QueryRow(query, taskID).Scan(
		&task.ID, &task.Name, &task.Type, &task.Status,
		&task.Progress, &task.Message,
		&resultJSON, &task.Error, &metadataJSON, &task.CreatedBy,
		&task.CreatedAt, &startedAt, &completedAt,
	)

	if err != nil {
		return nil, err
	}

	// 解析JSON字段
	if resultJSON != "" {
		json.Unmarshal([]byte(resultJSON), &task.Result)
	}
	if metadataJSON != "" {
		json.Unmarshal([]byte(metadataJSON), &task.Metadata)
	}
	if startedAt.Valid {
		task.StartedAt = &startedAt.Time
	}
	if completedAt.Valid {
		task.CompletedAt = &completedAt.Time
	}

	return &task, nil
}

// ListTasks 列出任务
func (d *DB) ListTasks(filter TaskFilter) ([]*Task, int, error) {
	// 构建查询条件
	query := `
		SELECT id, name, type, status, progress, message,
		       result, error, metadata, created_by,
		       created_at, started_at, completed_at
		FROM async_tasks
		WHERE 1=1
	`
	countQuery := "SELECT COUNT(*) FROM async_tasks WHERE 1=1"
	args := []interface{}{}

	// 状态过滤
	if len(filter.Status) > 0 {
		placeholders := ""
		for i, status := range filter.Status {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, string(status))
		}
		condition := fmt.Sprintf(" AND status IN (%s)", placeholders)
		query += condition
		countQuery += condition
	}

	// 类型过滤
	if len(filter.Type) > 0 {
		placeholders := ""
		for i, taskType := range filter.Type {
			if i > 0 {
				placeholders += ","
			}
			placeholders += "?"
			args = append(args, string(taskType))
		}
		condition := fmt.Sprintf(" AND type IN (%s)", placeholders)
		query += condition
		countQuery += condition
	}

	// 创建用户过滤
	if filter.CreatedBy != "" {
		query += " AND created_by = ?"
		countQuery += " AND created_by = ?"
		args = append(args, filter.CreatedBy)
	}

	// 获取总数
	var total int
	err := d.db.QueryRow(countQuery, args...).Scan(&total)
	if err != nil {
		return nil, 0, err
	}

	// 排序和分页
	query += " ORDER BY created_at DESC"
	if filter.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, filter.Limit)
	}
	if filter.Offset > 0 {
		query += " OFFSET ?"
		args = append(args, filter.Offset)
	}

	// 执行查询
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	// 解析结果
	tasks := []*Task{}
	for rows.Next() {
		var task Task
		var resultJSON, metadataJSON string
		var startedAt, completedAt sql.NullTime

		err := rows.Scan(
			&task.ID, &task.Name, &task.Type, &task.Status,
			&task.Progress, &task.Message,
			&resultJSON, &task.Error, &metadataJSON, &task.CreatedBy,
			&task.CreatedAt, &startedAt, &completedAt,
		)
		if err != nil {
			return nil, 0, err
		}

		// 解析JSON字段
		if resultJSON != "" {
			json.Unmarshal([]byte(resultJSON), &task.Result)
		}
		if metadataJSON != "" {
			json.Unmarshal([]byte(metadataJSON), &task.Metadata)
		}
		if startedAt.Valid {
			task.StartedAt = &startedAt.Time
		}
		if completedAt.Valid {
			task.CompletedAt = &completedAt.Time
		}

		tasks = append(tasks, &task)
	}

	return tasks, total, nil
}

// DeleteTask 删除任务
func (d *DB) DeleteTask(taskID string) error {
	_, err := d.db.Exec("DELETE FROM async_tasks WHERE id = ?", taskID)
	return err
}

// DeleteCompletedTasks 删除已完成的任务（超过指定天数）
func (d *DB) DeleteCompletedTasks(days int) (int, error) {
	query := `
		DELETE FROM async_tasks 
		WHERE status IN ('completed', 'failed', 'cancelled')
		AND completed_at < datetime('now', '-' || ? || ' days')
	`
	result, err := d.db.Exec(query, days)
	if err != nil {
		return 0, err
	}
	affected, _ := result.RowsAffected()
	return int(affected), nil
}
