package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/models"
)

// StatusDAO 状态数据访问对象
type StatusDAO struct {
	db *Database
}

// NewStatusDAO 创建状态DAO
func NewStatusDAO(db *Database) *StatusDAO {
	return &StatusDAO{db: db}
}

// AddHostStatus 添加主机状态
func (s *StatusDAO) AddHostStatus(hsName string, status *models.HSStatus) error {
	statusJSON, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("序列化主机状态失败: %w", err)
	}

	// 先清理旧状态（保留最近100条）
	cleanupQuery := `
		DELETE FROM hs_status 
		WHERE hs_name = ? AND id NOT IN (
			SELECT id FROM hs_status 
			WHERE hs_name = ? 
			ORDER BY created_at DESC 
			LIMIT 100
		)
	`
	_, err = s.db.Exec(cleanupQuery, hsName, hsName)
	if err != nil {
		return fmt.Errorf("清理旧状态失败: %w", err)
	}

	// 插入新状态
	query := `INSERT INTO hs_status (hs_name, status_data) VALUES (?, ?)`
	_, err = s.db.Exec(query, hsName, string(statusJSON))
	return err
}

// GetHostStatus 获取主机状态
func (s *StatusDAO) GetHostStatus(hsName string) ([]*models.HSStatus, error) {
	query := `SELECT status_data FROM hs_status WHERE hs_name = ? ORDER BY created_at DESC`
	rows, err := s.db.Query(query, hsName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var statuses []*models.HSStatus
	for rows.Next() {
		var statusJSON string
		if err := rows.Scan(&statusJSON); err != nil {
			return nil, err
		}

		var status models.HSStatus
		if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
			return nil, fmt.Errorf("反序列化主机状态失败: %w", err)
		}

		statuses = append(statuses, &status)
	}

	return statuses, nil
}

// GetLatestHostStatus 获取最新的主机状态
func (s *StatusDAO) GetLatestHostStatus(hsName string) (*models.HSStatus, error) {
	query := `SELECT status_data FROM hs_status WHERE hs_name = ? ORDER BY created_at DESC LIMIT 1`
	var statusJSON string
	err := s.db.QueryRow(query, hsName).Scan(&statusJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var status models.HSStatus
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		return nil, fmt.Errorf("反序列化主机状态失败: %w", err)
	}

	return &status, nil
}

// AddVMStatus 添加虚拟机状态
func (s *StatusDAO) AddVMStatus(hsName, vmUUID string, status *models.HWStatus) error {
	// 获取现有状态
	existingStatuses, err := s.GetVMStatuses(hsName)
	if err != nil {
		return err
	}

	// 如果该虚拟机已有状态，累加流量
	if vmStatuses, exists := existingStatuses[vmUUID]; exists && len(vmStatuses) > 0 {
		lastStatus := vmStatuses[len(vmStatuses)-1]
		status.FluUsage += lastStatus.FluUsage
	}

	statusJSON, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("序列化虚拟机状态失败: %w", err)
	}

	// 先清理旧状态（保留每个虚拟机最近100条）
	cleanupQuery := `
		DELETE FROM vm_status 
		WHERE hs_name = ? AND vm_uuid = ? AND id NOT IN (
			SELECT id FROM vm_status 
			WHERE hs_name = ? AND vm_uuid = ? 
			ORDER BY created_at DESC 
			LIMIT 100
		)
	`
	_, err = s.db.Exec(cleanupQuery, hsName, vmUUID, hsName, vmUUID)
	if err != nil {
		return fmt.Errorf("清理旧状态失败: %w", err)
	}

	// 插入新状态
	query := `INSERT INTO vm_status (hs_name, vm_uuid, status_data) VALUES (?, ?, ?)`
	_, err = s.db.Exec(query, hsName, vmUUID, string(statusJSON))
	return err
}

// GetVMStatuses 获取主机下所有虚拟机状态
func (s *StatusDAO) GetVMStatuses(hsName string) (map[string][]*models.HWStatus, error) {
	query := `SELECT vm_uuid, status_data FROM vm_status WHERE hs_name = ? ORDER BY created_at DESC`
	rows, err := s.db.Query(query, hsName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	statuses := make(map[string][]*models.HWStatus)
	for rows.Next() {
		var vmUUID, statusJSON string
		if err := rows.Scan(&vmUUID, &statusJSON); err != nil {
			return nil, err
		}

		var status models.HWStatus
		if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
			return nil, fmt.Errorf("反序列化虚拟机状态失败: %w", err)
		}

		if _, exists := statuses[vmUUID]; !exists {
			statuses[vmUUID] = make([]*models.HWStatus, 0)
		}
		statuses[vmUUID] = append(statuses[vmUUID], &status)
	}

	return statuses, nil
}

// GetVMStatus 获取虚拟机状态
func (s *StatusDAO) GetVMStatus(hsName, vmUUID string) ([]*models.HWStatus, error) {
	query := `SELECT status_data FROM vm_status WHERE hs_name = ? AND vm_uuid = ? ORDER BY created_at DESC`
	rows, err := s.db.Query(query, hsName, vmUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var statuses []*models.HWStatus
	for rows.Next() {
		var statusJSON string
		if err := rows.Scan(&statusJSON); err != nil {
			return nil, err
		}

		var status models.HWStatus
		if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
			return nil, fmt.Errorf("反序列化虚拟机状态失败: %w", err)
		}

		statuses = append(statuses, &status)
	}

	return statuses, nil
}

// GetLatestVMStatus 获取最新的虚拟机状态
func (s *StatusDAO) GetLatestVMStatus(hsName, vmUUID string) (*models.HWStatus, error) {
	query := `SELECT status_data FROM vm_status WHERE hs_name = ? AND vm_uuid = ? ORDER BY created_at DESC LIMIT 1`
	var statusJSON string
	err := s.db.QueryRow(query, hsName, vmUUID).Scan(&statusJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var status models.HWStatus
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		return nil, fmt.Errorf("反序列化虚拟机状态失败: %w", err)
	}

	return &status, nil
}

// CleanOldStatus 清理旧状态数据
func (s *StatusDAO) CleanOldStatus(days int) error {
	cutoffTime := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)

	// 清理旧主机状态
	_, err := s.db.Exec("DELETE FROM hs_status WHERE created_at < ?", cutoffTime)
	if err != nil {
		return fmt.Errorf("清理主机状态失败: %w", err)
	}

	// 清理旧虚拟机状态
	_, err = s.db.Exec("DELETE FROM vm_status WHERE created_at < ?", cutoffTime)
	if err != nil {
		return fmt.Errorf("清理虚拟机状态失败: %w", err)
	}

	return nil
}

// LogDAO 日志数据访问对象
type LogDAO struct {
	db *Database
}

// NewLogDAO 创建日志DAO
func NewLogDAO(db *Database) *LogDAO {
	return &LogDAO{db: db}
}

// AddLog 添加日志
func (l *LogDAO) AddLog(hsName string, log *models.ZMessage) error {
	logJSON, err := json.Marshal(log)
	if err != nil {
		return fmt.Errorf("序列化日志失败: %w", err)
	}

	query := `INSERT INTO hs_logger (hs_name, log_data, log_level) VALUES (?, ?, ?)`
	_, err = l.db.Exec(query, hsName, string(logJSON), log.Level)
	return err
}

// GetLogs 获取日志
func (l *LogDAO) GetLogs(hsName string, limit int) ([]*models.ZMessage, error) {
	query := `SELECT log_data, created_at FROM hs_logger WHERE ? = '' OR hs_name = ? ORDER BY created_at DESC LIMIT ?`
	rows, err := l.db.Query(query, hsName, hsName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []*models.ZMessage
	for rows.Next() {
		var logJSON string
		var createdAt time.Time
		if err := rows.Scan(&logJSON, &createdAt); err != nil {
			return nil, err
		}

		var log models.ZMessage
		if err := json.Unmarshal([]byte(logJSON), &log); err != nil {
			return nil, fmt.Errorf("反序列化日志失败: %w", err)
		}

		logs = append(logs, &log)
	}

	return logs, nil
}

// GetAllLogs 获取所有日志
func (l *LogDAO) GetAllLogs(limit int) ([]*models.ZMessage, error) {
	query := `SELECT log_data, created_at FROM hs_logger ORDER BY created_at DESC LIMIT ?`
	rows, err := l.db.Query(query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []*models.ZMessage
	for rows.Next() {
		var logJSON string
		var createdAt time.Time
		if err := rows.Scan(&logJSON, &createdAt); err != nil {
			return nil, err
		}

		var log models.ZMessage
		if err := json.Unmarshal([]byte(logJSON), &log); err != nil {
			return nil, fmt.Errorf("反序列化日志失败: %w", err)
		}

		logs = append(logs, &log)
	}

	return logs, nil
}

// CleanOldLogs 清理旧日志
func (l *LogDAO) CleanOldLogs(days int) error {
	cutoffTime := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	_, err := l.db.Exec("DELETE FROM hs_logger WHERE created_at < ?", cutoffTime)
	return err
}

// GetLogCount 获取日志数量
func (l *LogDAO) GetLogCount(hsName string) (int, error) {
	query := `SELECT COUNT(*) FROM hs_logger WHERE ? = '' OR hs_name = ?`
	var count int
	err := l.db.QueryRow(query, hsName, hsName).Scan(&count)
	return count, err
}