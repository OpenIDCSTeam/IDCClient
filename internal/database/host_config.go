package database

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/models"
)

// HostConfigDAO 主机配置数据访问对象
type HostConfigDAO struct {
	db *Database
}

// NewHostConfigDAO 创建主机配置DAO
func NewHostConfigDAO(db *Database) *HostConfigDAO {
	return &HostConfigDAO{db: db}
}

// SetGlobalConfig 设置全局配置
func (h *HostConfigDAO) SetGlobalConfig(key, value string) error {
	query := `
		INSERT OR REPLACE INTO hs_global (id, data, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
	`
	_, err := h.db.Exec(query, key, value)
	return err
}

// GetGlobalConfig 获取全局配置
func (h *HostConfigDAO) GetGlobalConfig(key string) (string, error) {
	query := `SELECT data FROM hs_global WHERE id = ?`
	var value string
	err := h.db.QueryRow(query, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

// GetAllGlobalConfigs 获取所有全局配置
func (h *HostConfigDAO) GetAllGlobalConfigs() (map[string]string, error) {
	query := `SELECT id, data FROM hs_global`
	rows, err := h.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	configs := make(map[string]string)
	for rows.Next() {
		var id, data string
		if err := rows.Scan(&id, &data); err != nil {
			return nil, err
		}
		configs[id] = data
	}
	return configs, nil
}

// SetHostConfig 保存主机配置
func (h *HostConfigDAO) SetHostConfig(hsName string, config *models.HSConfig) error {
	systemMapsJSON, _ := json.Marshal(config.SystemMaps)
	publicAddrJSON, _ := json.Marshal(config.PublicAddr)
	extendDataJSON, _ := json.Marshal(config.ExtendData)
	serverDNSSJSON, _ := json.Marshal(config.ServerDNSS)
	ipAddrMapsJSON, _ := json.Marshal(config.IPAddrMaps)
	ipAddrDNSSJSON, _ := json.Marshal(config.IPAddrDNSS)

	query := `
		INSERT OR REPLACE INTO hs_config (
			hs_name, server_name, server_type, server_addr, server_user, server_pass,
			filter_name, images_path, system_path, backup_path, extern_path,
			launch_path, network_nat, network_pub, i_kuai_addr, i_kuai_user,
			i_kuai_pass, ports_start, ports_close, remote_port, system_maps,
			public_addr, extend_data, server_dnss, limits_nums, ipaddr_maps, ipaddr_dnss, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
	`

	_, err := h.db.Exec(query,
		hsName,
		config.ServerName,
		config.ServerType,
		config.ServerAddr,
		config.ServerUser,
		config.ServerPass,
		config.FilterName,
		config.ImagesPath,
		config.SystemPath,
		config.BackupPath,
		config.ExternPath,
		config.LaunchPath,
		config.NetworkNAT,
		config.NetworkPub,
		config.IKuaiAddr,
		config.IKuaiUser,
		config.IKuaiPass,
		config.PortsStart,
		config.PortsClose,
		config.RemotePort,
		string(systemMapsJSON),
		string(publicAddrJSON),
		string(extendDataJSON),
		string(serverDNSSJSON),
		config.LimitsNums,
		string(ipAddrMapsJSON),
		string(ipAddrDNSSJSON),
	)

	return err
}

// GetHostConfig 获取主机配置
func (h *HostConfigDAO) GetHostConfig(hsName string) (*models.HSConfig, error) {
	query := `
		SELECT server_name, server_type, server_addr, server_user, server_pass,
			   filter_name, images_path, system_path, backup_path, extern_path,
			   launch_path, network_nat, network_pub, i_kuai_addr, i_kuai_user,
			   i_kuai_pass, ports_start, ports_close, remote_port, system_maps,
			   public_addr, extend_data, server_dnss, limits_nums, ipaddr_maps, ipaddr_dnss
		FROM hs_config WHERE hs_name = ?
	`

	var config models.HSConfig
	var systemMapsJSON, publicAddrJSON, extendDataJSON, serverDNSSJSON, ipAddrMapsJSON, ipAddrDNSSJSON sql.NullString

	err := h.db.QueryRow(query, hsName).Scan(
		&config.ServerName,
		&config.ServerType,
		&config.ServerAddr,
		&config.ServerUser,
		&config.ServerPass,
		&config.FilterName,
		&config.ImagesPath,
		&config.SystemPath,
		&config.BackupPath,
		&config.ExternPath,
		&config.LaunchPath,
		&config.NetworkNAT,
		&config.NetworkPub,
		&config.IKuaiAddr,
		&config.IKuaiUser,
		&config.IKuaiPass,
		&config.PortsStart,
		&config.PortsClose,
		&config.RemotePort,
		&systemMapsJSON,
		&publicAddrJSON,
		&extendDataJSON,
		&serverDNSSJSON,
		&config.LimitsNums,
		&ipAddrMapsJSON,
		&ipAddrDNSSJSON,
	)

	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	// 解析JSON字段
	if systemMapsJSON.Valid {
		json.Unmarshal([]byte(systemMapsJSON.String), &config.SystemMaps)
	}
	if publicAddrJSON.Valid {
		json.Unmarshal([]byte(publicAddrJSON.String), &config.PublicAddr)
	}
	if extendDataJSON.Valid {
		json.Unmarshal([]byte(extendDataJSON.String), &config.ExtendData)
	}
	if serverDNSSJSON.Valid {
		json.Unmarshal([]byte(serverDNSSJSON.String), &config.ServerDNSS)
	}
	if ipAddrMapsJSON.Valid {
		json.Unmarshal([]byte(ipAddrMapsJSON.String), &config.IPAddrMaps)
	}
	if ipAddrDNSSJSON.Valid {
		json.Unmarshal([]byte(ipAddrDNSSJSON.String), &config.IPAddrDNSS)
	}

	config.ServerName = hsName
	return &config, nil
}

// GetAllHostConfigs 获取所有主机配置
func (h *HostConfigDAO) GetAllHostConfigs() ([]*models.HSConfig, error) {
	query := `
		SELECT hs_name, server_name, server_type, server_addr, server_user, server_pass,
			   filter_name, images_path, system_path, backup_path, extern_path,
			   launch_path, network_nat, network_pub, i_kuai_addr, i_kuai_user,
			   i_kuai_pass, ports_start, ports_close, remote_port, system_maps,
			   public_addr, extend_data, server_dnss, limits_nums, ipaddr_maps, ipaddr_dnss
		FROM hs_config
	`

	rows, err := h.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []*models.HSConfig
	for rows.Next() {
		var config models.HSConfig
		var hsName string
		var systemMapsJSON, publicAddrJSON, extendDataJSON, serverDNSSJSON, ipAddrMapsJSON, ipAddrDNSSJSON sql.NullString

		err := rows.Scan(
			&hsName,
			&config.ServerName,
			&config.ServerType,
			&config.ServerAddr,
			&config.ServerUser,
			&config.ServerPass,
			&config.FilterName,
			&config.ImagesPath,
			&config.SystemPath,
			&config.BackupPath,
			&config.ExternPath,
			&config.LaunchPath,
			&config.NetworkNAT,
			&config.NetworkPub,
			&config.IKuaiAddr,
			&config.IKuaiUser,
			&config.IKuaiPass,
			&config.PortsStart,
			&config.PortsClose,
			&config.RemotePort,
			&systemMapsJSON,
			&publicAddrJSON,
			&extendDataJSON,
			&serverDNSSJSON,
			&config.LimitsNums,
			&ipAddrMapsJSON,
			&ipAddrDNSSJSON,
		)

		if err != nil {
			return nil, err
		}

		// 解析JSON字段
		if systemMapsJSON.Valid {
			json.Unmarshal([]byte(systemMapsJSON.String), &config.SystemMaps)
		}
		if publicAddrJSON.Valid {
			json.Unmarshal([]byte(publicAddrJSON.String), &config.PublicAddr)
		}
		if extendDataJSON.Valid {
			json.Unmarshal([]byte(extendDataJSON.String), &config.ExtendData)
		}
		if serverDNSSJSON.Valid {
			json.Unmarshal([]byte(serverDNSSJSON.String), &config.ServerDNSS)
		}
		if ipAddrMapsJSON.Valid {
			json.Unmarshal([]byte(ipAddrMapsJSON.String), &config.IPAddrMaps)
		}
		if ipAddrDNSSJSON.Valid {
			json.Unmarshal([]byte(ipAddrDNSSJSON.String), &config.IPAddrDNSS)
		}

		config.ServerName = hsName
		configs = append(configs, &config)
	}

	return configs, nil
}

// DeleteHostConfig 删除主机配置
func (h *HostConfigDAO) DeleteHostConfig(hsName string) error {
	query := `DELETE FROM hs_config WHERE hs_name = ?`
	result, err := h.db.Exec(query, hsName)
	if err != nil {
		return err
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if rowsAffected == 0 {
		return fmt.Errorf("主机配置不存在: %s", hsName)
	}

	return nil
}

// HostConfigExists 检查主机配置是否存在
func (h *HostConfigDAO) HostConfigExists(hsName string) (bool, error) {
	query := `SELECT COUNT(*) FROM hs_config WHERE hs_name = ?`
	var count int
	err := h.db.QueryRow(query, hsName).Scan(&count)
	return count > 0, err
}

// UpdateHostConfig 更新主机配置
func (h *HostConfigDAO) UpdateHostConfig(hsName string, config *models.HSConfig) error {
	exists, err := h.HostConfigExists(hsName)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("主机配置不存在: %s", hsName)
	}
	return h.SetHostConfig(hsName, config)
}

// GetHostConfigCount 获取主机配置数量
func (h *HostConfigDAO) GetHostConfigCount() (int, error) {
	query := `SELECT COUNT(*) FROM hs_config`
	var count int
	err := h.db.QueryRow(query).Scan(&count)
	return count, err
}

// GetHostConfigsByType 根据类型获取主机配置
func (h *HostConfigDAO) GetHostConfigsByType(serverType string) ([]*models.HSConfig, error) {
	query := `
		SELECT hs_name, server_name, server_type, server_addr, server_user, server_pass,
			   filter_name, images_path, system_path, backup_path, extern_path,
			   launch_path, network_nat, network_pub, i_kuai_addr, i_kuai_user,
			   i_kuai_pass, ports_start, ports_close, remote_port, system_maps,
			   public_addr, extend_data, server_dnss, limits_nums, ipaddr_maps, ipaddr_dnss
		FROM hs_config WHERE server_type = ?
	`

	rows, err := h.db.Query(query, serverType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var configs []*models.HSConfig
	for rows.Next() {
		var config models.HSConfig
		var hsName string
		var systemMapsJSON, publicAddrJSON, extendDataJSON, serverDNSSJSON, ipAddrMapsJSON, ipAddrDNSSJSON sql.NullString

		err := rows.Scan(
			&hsName,
			&config.ServerName,
			&config.ServerType,
			&config.ServerAddr,
			&config.ServerUser,
			&config.ServerPass,
			&config.FilterName,
			&config.ImagesPath,
			&config.SystemPath,
			&config.BackupPath,
			&config.ExternPath,
			&config.LaunchPath,
			&config.NetworkNAT,
			&config.NetworkPub,
			&config.IKuaiAddr,
			&config.IKuaiUser,
			&config.IKuaiPass,
			&config.PortsStart,
			&config.PortsClose,
			&config.RemotePort,
			&systemMapsJSON,
			&publicAddrJSON,
			&extendDataJSON,
			&serverDNSSJSON,
			&config.LimitsNums,
			&ipAddrMapsJSON,
			&ipAddrDNSSJSON,
		)

		if err != nil {
			return nil, err
		}

		// 解析JSON字段
		if systemMapsJSON.Valid {
			json.Unmarshal([]byte(systemMapsJSON.String), &config.SystemMaps)
		}
		if publicAddrJSON.Valid {
			json.Unmarshal([]byte(publicAddrJSON.String), &config.PublicAddr)
		}
		if extendDataJSON.Valid {
			json.Unmarshal([]byte(extendDataJSON.String), &config.ExtendData)
		}
		if serverDNSSJSON.Valid {
			json.Unmarshal([]byte(serverDNSSJSON.String), &config.ServerDNSS)
		}
		if ipAddrMapsJSON.Valid {
			json.Unmarshal([]byte(ipAddrMapsJSON.String), &config.IPAddrMaps)
		}
		if ipAddrDNSSJSON.Valid {
			json.Unmarshal([]byte(ipAddrDNSSJSON.String), &config.IPAddrDNSS)
		}

		config.ServerName = hsName
		configs = append(configs, &config)
	}

	return configs, nil
}