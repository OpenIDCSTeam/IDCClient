package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/models"
)

// VMConfigDAO 虚拟机配置数据访问对象
type VMConfigDAO struct {
	db *Database
}

// NewVMConfigDAO 创建虚拟机配置DAO
func NewVMConfigDAO(db *Database) *VMConfigDAO {
	return &VMConfigDAO{db: db}
}

// SetVMConfig 保存虚拟机配置
func (v *VMConfigDAO) SetVMConfig(hsName string, vmConfigs map[string]*models.VMConfig) error {
	tx, err := v.db.Begin()
	if err != nil {
		return fmt.Errorf("开始事务失败: %w", err)
	}
	defer tx.Rollback()

	// 删除旧的配置
	result, err := tx.Exec("DELETE FROM vm_saving WHERE hs_name = ?", hsName)
	if err != nil {
		return fmt.Errorf("删除旧虚拟机配置失败: %w", err)
	}

	// 检查删除操作
	if rowsAffected, _ := result.RowsAffected(); rowsAffected > 0 {
		v.db.logger.Debugf("删除了 %d 条旧的虚拟机配置", rowsAffected)
	}

	// 插入新配置
	query := `
		INSERT INTO vm_saving (hs_name, vm_uuid, vm_config, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
	`

	count := 0
	for vmUUID, vmConfig := range vmConfigs {
		configJSON, err := json.Marshal(vmConfig)
		if err != nil {
			return fmt.Errorf("序列化虚拟机配置失败 %s: %w", vmUUID, err)
		}

		result, err := tx.Exec(query, hsName, vmUUID, string(configJSON))
		if err != nil {
			return fmt.Errorf("保存虚拟机配置失败 %s: %w", vmUUID, err)
		}

		if rowsAffected, _ := result.RowsAffected(); rowsAffected == 0 {
			return fmt.Errorf("保存虚拟机配置失败 %s: 没有行被影响", vmUUID)
		}
		count++
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}

	v.db.logger.Debugf("成功保存了 %d 个虚拟机配置", count)
	return nil
}

// GetVMConfigs 获取主机下的所有虚拟机配置
func (v *VMConfigDAO) GetVMConfigs(hsName string) (map[string]*models.VMConfig, error) {
	query := `SELECT vm_uuid, vm_config FROM vm_saving WHERE hs_name = ?`
	rows, err := v.db.Query(query, hsName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	vmConfigs := make(map[string]*models.VMConfig)
	for rows.Next() {
		var vmUUID, configJSON string
		if err := rows.Scan(&vmUUID, &configJSON); err != nil {
			return nil, err
		}

		var vmConfig models.VMConfig
		if err := json.Unmarshal([]byte(configJSON), &vmConfig); err != nil {
			return nil, fmt.Errorf("反序列化虚拟机配置失败: %w", err)
		}

		vmConfigs[vmUUID] = &vmConfig
	}

	return vmConfigs, nil
}

// GetVMConfig 获取单个虚拟机配置
func (v *VMConfigDAO) GetVMConfig(hsName, vmUUID string) (*models.VMConfig, error) {
	query := `SELECT vm_config FROM vm_saving WHERE hs_name = ? AND vm_uuid = ?`
	var configJSON string
	err := v.db.QueryRow(query, hsName, vmUUID).Scan(&configJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var vmConfig models.VMConfig
	if err := json.Unmarshal([]byte(configJSON), &vmConfig); err != nil {
		return nil, fmt.Errorf("反序列化虚拟机配置失败: %w", err)
	}

	return &vmConfig, nil
}

// DeleteVMConfig 删除虚拟机配置
func (v *VMConfigDAO) DeleteVMConfig(hsName, vmUUID string) error {
	query := `DELETE FROM vm_saving WHERE hs_name = ? AND vm_uuid = ?`
	result, err := v.db.Exec(query, hsName, vmUUID)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected == 0 {
		return fmt.Errorf("虚拟机配置不存在: %s/%s", hsName, vmUUID)
	}

	return nil
}

// DeleteAllVMConfigs 删除主机下的所有虚拟机配置
func (v *VMConfigDAO) DeleteAllVMConfigs(hsName string) error {
	query := `DELETE FROM vm_saving WHERE hs_name = ?`
	_, err := v.db.Exec(query, hsName)
	return err
}

// VMConfigExists 检查虚拟机配置是否存在
func (v *VMConfigDAO) VMConfigExists(hsName, vmUUID string) (bool, error) {
	query := `SELECT COUNT(*) FROM vm_saving WHERE hs_name = ? AND vm_uuid = ?`
	var count int
	err := v.db.QueryRow(query, hsName, vmUUID).Scan(&count)
	return count > 0, err
}

// GetVMConfigCount 获取主机下的虚拟机配置数量
func (v *VMConfigDAO) GetVMConfigCount(hsName string) (int, error) {
	query := `SELECT COUNT(*) FROM vm_saving WHERE hs_name = ?`
	var count int
	err := v.db.QueryRow(query, hsName).Scan(&count)
	return count, err
}

// GetAllVMConfigs 获取所有虚拟机配置
func (v *VMConfigDAO) GetAllVMConfigs() (map[string]map[string]*models.VMConfig, error) {
	query := `SELECT hs_name, vm_uuid, vm_config FROM vm_saving`
	rows, err := v.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]map[string]*models.VMConfig)
	for rows.Next() {
		var hsName, vmUUID, configJSON string
		if err := rows.Scan(&hsName, &vmUUID, &configJSON); err != nil {
			return nil, err
		}

		if _, exists := result[hsName]; !exists {
			result[hsName] = make(map[string]*models.VMConfig)
		}

		var vmConfig models.VMConfig
		if err := json.Unmarshal([]byte(configJSON), &vmConfig); err != nil {
			return nil, fmt.Errorf("反序列化虚拟机配置失败: %w", err)
		}

		result[hsName][vmUUID] = &vmConfig
	}

	return result, nil
}

// UpdateVMConfig 更新虚拟机配置（只更新配置，不更新时间戳）
func (v *VMConfigDAO) UpdateVMConfig(hsName, vmUUID string, vmConfig *models.VMConfig) error {
	exists, err := v.VMConfigExists(hsName, vmUUID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("虚拟机配置不存在: %s/%s", hsName, vmUUID)
	}

	configJSON, err := json.Marshal(vmConfig)
	if err != nil {
		return fmt.Errorf("序列化虚拟机配置失败: %w", err)
	}

	// 只更新配置信息，不更新时间戳（根据要求：vm_saving表更新时不得覆写created_at和updated_at）
	query := `
		UPDATE vm_saving 
		SET vm_config = ? 
		WHERE hs_name = ? AND vm_uuid = ?
	`

	_, err = v.db.Exec(query, string(configJSON), hsName, vmUUID)
	return err
}

// AddVMConfig 添加虚拟机配置
func (v *VMConfigDAO) AddVMConfig(hsName, vmUUID string, vmConfig *models.VMConfig) error {
	exists, err := v.VMConfigExists(hsName, vmUUID)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("虚拟机配置已存在: %s/%s", hsName, vmUUID)
	}

	configJSON, err := json.Marshal(vmConfig)
	if err != nil {
		return fmt.Errorf("序列化虚拟机配置失败: %w", err)
	}

	query := `
		INSERT INTO vm_saving (hs_name, vm_uuid, vm_config)
		VALUES (?, ?, ?)
	`

	_, err = v.db.Exec(query, hsName, vmUUID, string(configJSON))
	return err
}

// GetVMConfigsByStatus 根据状态获取虚拟机配置（这个需要关联状态表）
func (v *VMConfigDAO) GetVMConfigsByStatus(hsName string, status string) (map[string]*models.VMConfig, error) {
	// 这里需要关联vm_status表，暂时返回所有配置
	return v.GetVMConfigs(hsName)
}

// CopyVMConfigs 复制虚拟机配置
func (v *VMConfigDAO) CopyVMConfigs(srcHSName, dstHSName string) error {
	vmConfigs, err := v.GetVMConfigs(srcHSName)
	if err != nil {
		return err
	}

	return v.SetVMConfig(dstHSName, vmConfigs)
}

// UpdateVMStatusTimestamp 更新虚拟机状态时间戳（仅在状态上报时调用）
func (v *VMConfigDAO) UpdateVMStatusTimestamp(hsName, vmUUID string) error {
	exists, err := v.VMConfigExists(hsName, vmUUID)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("虚拟机配置不存在: %s/%s", hsName, vmUUID)
	}

	// 只更新updated_at时间戳，不修改配置内容
	query := `
		UPDATE vm_saving 
		SET updated_at = CURRENT_TIMESTAMP 
		WHERE hs_name = ? AND vm_uuid = ?
	`

	_, err = v.db.Exec(query, hsName, vmUUID)
	return err
}

// GetVMConfigWithTimestamp 获取带时间戳的虚拟机配置
func (v *VMConfigDAO) GetVMConfigWithTimestamp(hsName, vmUUID string) (*models.VMConfig, *time.Time, *time.Time, error) {
	query := `
		SELECT vm_config, created_at, updated_at 
		FROM vm_saving 
		WHERE hs_name = ? AND vm_uuid = ?
	`
	var configJSON string
	var createdAt, updatedAt time.Time
	err := v.db.QueryRow(query, hsName, vmUUID).Scan(&configJSON, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}

	var vmConfig models.VMConfig
	if err := json.Unmarshal([]byte(configJSON), &vmConfig); err != nil {
		return nil, nil, nil, fmt.Errorf("反序列化虚拟机配置失败: %w", err)
	}

	return &vmConfig, &createdAt, &updatedAt, nil
}