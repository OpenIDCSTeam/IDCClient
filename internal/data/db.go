// Package data 数据访问层 - SQLite数据库封装
package data

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/crypto/bcrypt"
)

// DB 数据库连接封装
type DB struct {
	db *sql.DB
}

// NewDB 初始化数据库连接
// dataDir: 数据目录路径，如 "./DataSaving"
func NewDB(dataDir string) (*DB, error) {
	// 确保数据目录存在
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}

	dbPath := filepath.Join(dataDir, "hostmanage.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 配置连接池
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	// 启用WAL模式
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return nil, fmt.Errorf("启用WAL模式失败: %w", err)
	}

	d := &DB{db: db}

	// 初始化表结构
	if err := d.initSchema(); err != nil {
		return nil, fmt.Errorf("初始化数据库表结构失败: %w", err)
	}

	// 执行迁移
	if err := d.migrate(); err != nil {
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	return d, nil
}

// Close 关闭数据库连接
func (d *DB) Close() error {
	return d.db.Close()
}

// initSchema 初始化数据库表结构
func (d *DB) initSchema() error {
	schema := `
-- 全局配置表
CREATE TABLE IF NOT EXISTS hs_global (
    id   TEXT PRIMARY KEY,
    data TEXT NOT NULL
);

-- 主机配置表
CREATE TABLE IF NOT EXISTS hs_config (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    hs_name     TEXT NOT NULL UNIQUE,
    server_name TEXT      DEFAULT '',
    server_type TEXT NOT NULL DEFAULT '',
    server_addr TEXT NOT NULL DEFAULT '',
    server_user TEXT NOT NULL DEFAULT '',
    server_pass TEXT NOT NULL DEFAULT '',
    server_port INTEGER   DEFAULT 0,
    images_path TEXT      DEFAULT '',
    dvdrom_path TEXT      DEFAULT '',
    system_path TEXT      DEFAULT '',
    backup_path TEXT      DEFAULT '',
    extern_path TEXT      DEFAULT '',
    launch_path TEXT      DEFAULT '',
    network_nat TEXT      DEFAULT '',
    network_pub TEXT      DEFAULT '',
    filter_name TEXT      DEFAULT '',
    i_kuai_addr TEXT      DEFAULT '',
    i_kuai_user TEXT      DEFAULT '',
    i_kuai_pass TEXT      DEFAULT '',
    ports_start INTEGER   DEFAULT 0,
    ports_close INTEGER   DEFAULT 0,
    remote_port INTEGER   DEFAULT 0,
    system_maps TEXT      DEFAULT '[]',
    images_maps TEXT      DEFAULT '[]',
    public_addr TEXT      DEFAULT '[]',
    extend_data TEXT      DEFAULT '{}',
    limits_nums INTEGER   DEFAULT 0,
    ipaddr_maps TEXT      DEFAULT '{}',
    ipaddr_ddns TEXT      DEFAULT '["119.29.29.29", "223.5.5.5"]',
    enable_host INTEGER   DEFAULT 1,
    server_area TEXT      DEFAULT '',
    n_cpu_price REAL      DEFAULT 0,
    n_mem_price REAL      DEFAULT 0,
    n_hdd_price REAL      DEFAULT 0,
    n_net_price REAL      DEFAULT 0,
    server_plan TEXT      DEFAULT '{}',
    created_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- 主机状态表
CREATE TABLE IF NOT EXISTS hs_status (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    hs_name     TEXT NOT NULL,
    status_data TEXT NOT NULL,
    recorded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (hs_name) REFERENCES hs_config (hs_name) ON DELETE CASCADE
);

-- 虚拟机存储配置表
CREATE TABLE IF NOT EXISTS vm_saving (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    hs_name    TEXT NOT NULL,
    vm_uuid    TEXT NOT NULL,
    vm_config  TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (hs_name) REFERENCES hs_config (hs_name) ON DELETE CASCADE,
    UNIQUE (hs_name, vm_uuid)
);

-- 虚拟机状态表
CREATE TABLE IF NOT EXISTS vm_status (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    hs_name     TEXT NOT NULL,
    vm_uuid     TEXT NOT NULL,
    status_data TEXT NOT NULL,
    ac_status   TEXT,
    on_update   INTEGER,
    flu_usage   REAL DEFAULT 0,
    recorded_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (hs_name) REFERENCES hs_config (hs_name) ON DELETE CASCADE
);

-- 虚拟机任务表
CREATE TABLE IF NOT EXISTS vm_tasker (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    hs_name    TEXT NOT NULL,
    task_data  TEXT NOT NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (hs_name) REFERENCES hs_config (hs_name) ON DELETE CASCADE
);

-- 日志记录表
CREATE TABLE IF NOT EXISTS hs_logger (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    hs_name    TEXT,
    log_data   TEXT NOT NULL,
    log_level  TEXT      DEFAULT 'INFO',
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (hs_name) REFERENCES hs_config (hs_name) ON DELETE SET NULL
);

-- 用户表
CREATE TABLE IF NOT EXISTS web_users (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    username          TEXT NOT NULL UNIQUE,
    password          TEXT NOT NULL,
    email             TEXT NOT NULL UNIQUE,
    is_admin          INTEGER   DEFAULT 0,
    is_active         INTEGER   DEFAULT 1,
    email_verified    INTEGER   DEFAULT 0,
    verify_token      TEXT      DEFAULT '',
    reset_token       TEXT      DEFAULT '',
    can_create_vm     INTEGER   DEFAULT 0,
    can_delete_vm     INTEGER   DEFAULT 0,
    can_modify_vm     INTEGER   DEFAULT 0,
    quota_cpu         INTEGER   DEFAULT 0,
    quota_ram         INTEGER   DEFAULT 0,
    quota_ssd         INTEGER   DEFAULT 0,
    quota_gpu         INTEGER   DEFAULT 0,
    quota_nat_ports   INTEGER   DEFAULT 0,
    quota_web_proxy   INTEGER   DEFAULT 0,
    quota_nat_ips     INTEGER   DEFAULT 0,
    quota_pub_ips     INTEGER   DEFAULT 0,
    quota_bandwidth_up    INTEGER DEFAULT 0,
    quota_bandwidth_down  INTEGER DEFAULT 0,
    quota_traffic     INTEGER   DEFAULT 0,
    used_cpu          INTEGER   DEFAULT 0,
    used_ram          INTEGER   DEFAULT 0,
    used_ssd          INTEGER   DEFAULT 0,
    used_gpu          INTEGER   DEFAULT 0,
    used_nat_ports    INTEGER   DEFAULT 0,
    used_web_proxy    INTEGER   DEFAULT 0,
    used_nat_ips      INTEGER   DEFAULT 0,
    used_pub_ips      INTEGER   DEFAULT 0,
    used_bandwidth_up    INTEGER   DEFAULT 0,
    used_bandwidth_down  INTEGER   DEFAULT 0,
    used_traffic      INTEGER   DEFAULT 0,
    assigned_hosts    TEXT      DEFAULT '[]',
    created_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    last_login        TIMESTAMP DEFAULT NULL
);

-- 异步任务表
CREATE TABLE IF NOT EXISTS async_tasks (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    type          TEXT NOT NULL,
    status        TEXT NOT NULL,
    progress      INTEGER DEFAULT 0,
    message       TEXT DEFAULT '',
    result        TEXT DEFAULT '{}',
    error         TEXT DEFAULT '',
    metadata      TEXT DEFAULT '{}',
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMP NOT NULL,
    started_at    TIMESTAMP DEFAULT NULL,
    completed_at  TIMESTAMP DEFAULT NULL
);

-- 索引
CREATE INDEX IF NOT EXISTS idx_hs_config_name ON hs_config (hs_name);
CREATE INDEX IF NOT EXISTS idx_hs_status_name ON hs_status (hs_name);
CREATE INDEX IF NOT EXISTS idx_vm_saving_name ON vm_saving (hs_name);
CREATE INDEX IF NOT EXISTS idx_vm_saving_uuid ON vm_saving (vm_uuid);
CREATE INDEX IF NOT EXISTS idx_vm_status_name ON vm_status (hs_name);
CREATE INDEX IF NOT EXISTS idx_vm_status_uuid ON vm_status (vm_uuid);
CREATE INDEX IF NOT EXISTS idx_vm_status_name_uuid ON vm_status (hs_name, vm_uuid);
CREATE INDEX IF NOT EXISTS idx_vm_status_timestamp ON vm_status (on_update);
CREATE INDEX IF NOT EXISTS idx_vm_status_recorded ON vm_status (recorded_at);
CREATE INDEX IF NOT EXISTS idx_vm_tasker_name ON vm_tasker (hs_name);
CREATE INDEX IF NOT EXISTS idx_hs_logger_name ON hs_logger (hs_name);
CREATE INDEX IF NOT EXISTS idx_hs_logger_time ON hs_logger (created_at);
CREATE INDEX IF NOT EXISTS idx_web_users_username ON web_users (username);
CREATE INDEX IF NOT EXISTS idx_web_users_email ON web_users (email);
CREATE INDEX IF NOT EXISTS idx_async_tasks_status ON async_tasks (status);
CREATE INDEX IF NOT EXISTS idx_async_tasks_type ON async_tasks (type);
CREATE INDEX IF NOT EXISTS idx_async_tasks_created_by ON async_tasks (created_by);
CREATE INDEX IF NOT EXISTS idx_async_tasks_created_at ON async_tasks (created_at);
`
	_, err := d.db.Exec(schema)
	return err
}

// migrate 执行数据库迁移（添加新字段）
func (d *DB) migrate() error {
	migrations := []string{
		`ALTER TABLE hs_config ADD COLUMN server_area TEXT DEFAULT ''`,
		`ALTER TABLE hs_config ADD COLUMN server_plan TEXT DEFAULT '{}'`,
		`ALTER TABLE hs_config ADD COLUMN n_cpu_price REAL DEFAULT 0`,
		`ALTER TABLE hs_config ADD COLUMN n_mem_price REAL DEFAULT 0`,
		`ALTER TABLE hs_config ADD COLUMN n_hdd_price REAL DEFAULT 0`,
		`ALTER TABLE hs_config ADD COLUMN n_net_price REAL DEFAULT 0`,
	}
	for _, m := range migrations {
		// 忽略"duplicate column"错误（字段已存在）
		if _, err := d.db.Exec(m); err != nil {
			// SQLite返回"duplicate column name"时忽略
			if err.Error() != "" {
				// 继续，字段可能已存在
				continue
			}
		}
	}
	return nil
}

// ============================================================
// 全局配置操作
// ============================================================

// GetGlobal 获取全局配置
func (d *DB) GetGlobal(id string) (string, error) {
	var data string
	err := d.db.QueryRow("SELECT data FROM hs_global WHERE id = ?", id).Scan(&data)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return data, err
}

// SetGlobal 设置全局配置
func (d *DB) SetGlobal(id, data string) error {
	_, err := d.db.Exec(
		"INSERT OR REPLACE INTO hs_global (id, data) VALUES (?, ?)",
		id, data,
	)
	return err
}

// ============================================================
// 主机配置操作
// ============================================================

// HSConfigRow 主机配置数据库行
type HSConfigRow struct {
	ID         int    `db:"id"`
	HsName     string `db:"hs_name"`
	ServerName string `db:"server_name"`
	ServerType string `db:"server_type"`
	ServerAddr string `db:"server_addr"`
	ServerUser string `db:"server_user"`
	ServerPass string `db:"server_pass"`
	ServerPort int    `db:"server_port"`
	ImagesPath string `db:"images_path"`
	DvdromPath string `db:"dvdrom_path"`
	SystemPath string `db:"system_path"`
	BackupPath string `db:"backup_path"`
	ExternPath string `db:"extern_path"`
	LaunchPath string `db:"launch_path"`
	NetworkNat string `db:"network_nat"`
	NetworkPub string `db:"network_pub"`
	FilterName string `db:"filter_name"`
	IKuaiAddr  string `db:"i_kuai_addr"`
	IKuaiUser  string `db:"i_kuai_user"`
	IKuaiPass  string `db:"i_kuai_pass"`
	PortsStart int    `db:"ports_start"`
	PortsClose int    `db:"ports_close"`
	RemotePort int    `db:"remote_port"`
	SystemMaps string `db:"system_maps"`
	ImagesMaps string `db:"images_maps"`
	PublicAddr string `db:"public_addr"`
	ExtendData string `db:"extend_data"`
	LimitsNums int    `db:"limits_nums"`
	IPAddrMaps string `db:"ipaddr_maps"`
	IPAddrDDNS string `db:"ipaddr_ddns"`
	EnableHost int     `db:"enable_host"`
	ServerArea string  `db:"server_area"`
	NCpuPrice  float64 `db:"n_cpu_price"`
	NMemPrice  float64 `db:"n_mem_price"`
	NHddPrice  float64 `db:"n_hdd_price"`
	NNetPrice  float64 `db:"n_net_price"`
	ServerPlan string  `db:"server_plan"`
	CreatedAt  string  `db:"created_at"`
	UpdatedAt  string  `db:"updated_at"`
}

// GetAllHosts 获取所有主机配置
func (d *DB) GetAllHosts() ([]*HSConfigRow, error) {
	rows, err := d.db.Query(`SELECT id, hs_name, server_name, server_type, server_addr,
		server_user, server_pass, server_port, images_path, dvdrom_path, system_path,
		backup_path, extern_path, launch_path, network_nat, network_pub, filter_name,
		i_kuai_addr, i_kuai_user, i_kuai_pass, ports_start, ports_close, remote_port,
		system_maps, images_maps, public_addr, extend_data, limits_nums, ipaddr_maps,
		ipaddr_ddns, enable_host, server_area,
		COALESCE(n_cpu_price,0), COALESCE(n_mem_price,0), COALESCE(n_hdd_price,0), COALESCE(n_net_price,0),
		server_plan, created_at, updated_at
		FROM hs_config ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*HSConfigRow
	for rows.Next() {
		row := &HSConfigRow{}
		if err := rows.Scan(
			&row.ID, &row.HsName, &row.ServerName, &row.ServerType, &row.ServerAddr,
			&row.ServerUser, &row.ServerPass, &row.ServerPort, &row.ImagesPath, &row.DvdromPath,
			&row.SystemPath, &row.BackupPath, &row.ExternPath, &row.LaunchPath, &row.NetworkNat,
			&row.NetworkPub, &row.FilterName, &row.IKuaiAddr, &row.IKuaiUser, &row.IKuaiPass,
			&row.PortsStart, &row.PortsClose, &row.RemotePort, &row.SystemMaps, &row.ImagesMaps,
			&row.PublicAddr, &row.ExtendData, &row.LimitsNums, &row.IPAddrMaps, &row.IPAddrDDNS,
			&row.EnableHost, &row.ServerArea,
			&row.NCpuPrice, &row.NMemPrice, &row.NHddPrice, &row.NNetPrice,
			&row.ServerPlan, &row.CreatedAt, &row.UpdatedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// GetHost 获取单个主机配置
func (d *DB) GetHost(hsName string) (*HSConfigRow, error) {
	row := &HSConfigRow{}
	err := d.db.QueryRow(`SELECT id, hs_name, server_name, server_type, server_addr,
		server_user, server_pass, server_port, images_path, dvdrom_path, system_path,
		backup_path, extern_path, launch_path, network_nat, network_pub, filter_name,
		i_kuai_addr, i_kuai_user, i_kuai_pass, ports_start, ports_close, remote_port,
		system_maps, images_maps, public_addr, extend_data, limits_nums, ipaddr_maps,
		ipaddr_ddns, enable_host, server_area,
		COALESCE(n_cpu_price,0), COALESCE(n_mem_price,0), COALESCE(n_hdd_price,0), COALESCE(n_net_price,0),
		server_plan, created_at, updated_at
		FROM hs_config WHERE hs_name = ?`, hsName).Scan(
		&row.ID, &row.HsName, &row.ServerName, &row.ServerType, &row.ServerAddr,
		&row.ServerUser, &row.ServerPass, &row.ServerPort, &row.ImagesPath, &row.DvdromPath,
		&row.SystemPath, &row.BackupPath, &row.ExternPath, &row.LaunchPath, &row.NetworkNat,
		&row.NetworkPub, &row.FilterName, &row.IKuaiAddr, &row.IKuaiUser, &row.IKuaiPass,
		&row.PortsStart, &row.PortsClose, &row.RemotePort, &row.SystemMaps, &row.ImagesMaps,
		&row.PublicAddr, &row.ExtendData, &row.LimitsNums, &row.IPAddrMaps, &row.IPAddrDDNS,
		&row.EnableHost, &row.ServerArea,
		&row.NCpuPrice, &row.NMemPrice, &row.NHddPrice, &row.NNetPrice,
		&row.ServerPlan, &row.CreatedAt, &row.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return row, err
}

// CreateHost 创建主机配置
func (d *DB) CreateHost(row *HSConfigRow) error {
	_, err := d.db.Exec(`INSERT INTO hs_config (hs_name, server_name, server_type, server_addr,
		server_user, server_pass, server_port, images_path, dvdrom_path, system_path,
		backup_path, extern_path, launch_path, network_nat, network_pub, filter_name,
		i_kuai_addr, i_kuai_user, i_kuai_pass, ports_start, ports_close, remote_port,
		system_maps, images_maps, public_addr, extend_data, limits_nums, ipaddr_maps,
		ipaddr_ddns, enable_host, server_area,
		n_cpu_price, n_mem_price, n_hdd_price, n_net_price,
		server_plan)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		row.HsName, row.ServerName, row.ServerType, row.ServerAddr,
		row.ServerUser, row.ServerPass, row.ServerPort, row.ImagesPath, row.DvdromPath,
		row.SystemPath, row.BackupPath, row.ExternPath, row.LaunchPath, row.NetworkNat,
		row.NetworkPub, row.FilterName, row.IKuaiAddr, row.IKuaiUser, row.IKuaiPass,
		row.PortsStart, row.PortsClose, row.RemotePort, row.SystemMaps, row.ImagesMaps,
		row.PublicAddr, row.ExtendData, row.LimitsNums, row.IPAddrMaps, row.IPAddrDDNS,
		row.EnableHost, row.ServerArea,
		row.NCpuPrice, row.NMemPrice, row.NHddPrice, row.NNetPrice,
		row.ServerPlan,
	)
	return err
}

// UpdateHost 更新主机配置
func (d *DB) UpdateHost(row *HSConfigRow) error {
	_, err := d.db.Exec(`UPDATE hs_config SET server_name=?, server_type=?, server_addr=?,
		server_user=?, server_pass=?, server_port=?, images_path=?, dvdrom_path=?, system_path=?,
		backup_path=?, extern_path=?, launch_path=?, network_nat=?, network_pub=?, filter_name=?,
		i_kuai_addr=?, i_kuai_user=?, i_kuai_pass=?, ports_start=?, ports_close=?, remote_port=?,
		system_maps=?, images_maps=?, public_addr=?, extend_data=?, limits_nums=?, ipaddr_maps=?,
		ipaddr_ddns=?, enable_host=?, server_area=?,
		n_cpu_price=?, n_mem_price=?, n_hdd_price=?, n_net_price=?,
		server_plan=?, updated_at=CURRENT_TIMESTAMP
		WHERE hs_name=?`,
		row.ServerName, row.ServerType, row.ServerAddr,
		row.ServerUser, row.ServerPass, row.ServerPort, row.ImagesPath, row.DvdromPath,
		row.SystemPath, row.BackupPath, row.ExternPath, row.LaunchPath, row.NetworkNat,
		row.NetworkPub, row.FilterName, row.IKuaiAddr, row.IKuaiUser, row.IKuaiPass,
		row.PortsStart, row.PortsClose, row.RemotePort, row.SystemMaps, row.ImagesMaps,
		row.PublicAddr, row.ExtendData, row.LimitsNums, row.IPAddrMaps, row.IPAddrDDNS,
		row.EnableHost, row.ServerArea,
		row.NCpuPrice, row.NMemPrice, row.NHddPrice, row.NNetPrice,
		row.ServerPlan, row.HsName,
	)
	return err
}

// DeleteHost 删除主机配置
func (d *DB) DeleteHost(hsName string) error {
	_, err := d.db.Exec("DELETE FROM hs_config WHERE hs_name = ?", hsName)
	return err
}

// SetHostEnable 设置主机启用状态
func (d *DB) SetHostEnable(hsName string, enable bool) error {
	v := 0
	if enable {
		v = 1
	}
	_, err := d.db.Exec("UPDATE hs_config SET enable_host=?, updated_at=CURRENT_TIMESTAMP WHERE hs_name=?", v, hsName)
	return err
}

// ============================================================
// 虚拟机配置操作
// ============================================================

// GetVMSaving 获取主机下所有虚拟机配置（返回JSON字符串map）
func (d *DB) GetVMSaving(hsName string) (map[string]string, error) {
	rows, err := d.db.Query("SELECT vm_uuid, vm_config FROM vm_saving WHERE hs_name = ?", hsName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]string)
	for rows.Next() {
		var uuid, config string
		if err := rows.Scan(&uuid, &config); err != nil {
			return nil, err
		}
		result[uuid] = config
	}
	return result, rows.Err()
}

// GetVMConfig 获取单个虚拟机配置
func (d *DB) GetVMConfig(hsName, vmUUID string) (string, error) {
	var config string
	err := d.db.QueryRow("SELECT vm_config FROM vm_saving WHERE hs_name=? AND vm_uuid=?", hsName, vmUUID).Scan(&config)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return config, err
}

// UpsertVMConfig 创建或更新虚拟机配置
func (d *DB) UpsertVMConfig(hsName, vmUUID string, config interface{}) error {
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("序列化虚拟机配置失败: %w", err)
	}
	_, err = d.db.Exec(`INSERT INTO vm_saving (hs_name, vm_uuid, vm_config, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(hs_name, vm_uuid) DO UPDATE SET vm_config=excluded.vm_config, updated_at=CURRENT_TIMESTAMP`,
		hsName, vmUUID, string(data),
	)
	return err
}

// DeleteVMConfig 删除虚拟机配置
func (d *DB) DeleteVMConfig(hsName, vmUUID string) error {
	_, err := d.db.Exec("DELETE FROM vm_saving WHERE hs_name=? AND vm_uuid=?", hsName, vmUUID)
	return err
}

// SetAllVMConfigs 批量保存主机下所有虚拟机配置
func (d *DB) SetAllVMConfigs(hsName string, configs map[string]interface{}) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	for vmUUID, config := range configs {
		data, err := json.Marshal(config)
		if err != nil {
			return fmt.Errorf("序列化虚拟机 %s 配置失败: %w", vmUUID, err)
		}
		_, err = tx.Exec(`INSERT INTO vm_saving (hs_name, vm_uuid, vm_config, updated_at)
			VALUES (?, ?, ?, CURRENT_TIMESTAMP)
			ON CONFLICT(hs_name, vm_uuid) DO UPDATE SET vm_config=excluded.vm_config, updated_at=CURRENT_TIMESTAMP`,
			hsName, vmUUID, string(data),
		)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ============================================================
// 虚拟机状态操作
// ============================================================

// AddVMStatus 添加虚拟机状态记录
func (d *DB) AddVMStatus(hsName, vmUUID string, statusData interface{}, acStatus string, onUpdate int64, fluUsage float64) error {
	data, err := json.Marshal(statusData)
	if err != nil {
		return fmt.Errorf("序列化状态数据失败: %w", err)
	}
	_, err = d.db.Exec(`INSERT INTO vm_status (hs_name, vm_uuid, status_data, ac_status, on_update, flu_usage)
		VALUES (?, ?, ?, ?, ?, ?)`,
		hsName, vmUUID, string(data), acStatus, onUpdate, fluUsage,
	)
	return err
}

// GetVMStatus 获取虚拟机状态（支持时间范围过滤）
func (d *DB) GetVMStatus(hsName, vmUUID string, startTS, endTS int64) ([]string, error) {
	query := "SELECT status_data FROM vm_status WHERE hs_name=?"
	args := []interface{}{hsName}
	if vmUUID != "" {
		query += " AND vm_uuid=?"
		args = append(args, vmUUID)
	}
	if startTS > 0 {
		query += " AND on_update>=?"
		args = append(args, startTS)
	}
	if endTS > 0 {
		query += " AND on_update<=?"
		args = append(args, endTS)
	}
	query += " ORDER BY on_update DESC LIMIT 100"

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []string
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		result = append(result, data)
	}
	return result, rows.Err()
}

// ============================================================
// 主机状态操作
// ============================================================

// AddHSStatus 添加主机状态记录
func (d *DB) AddHSStatus(hsName string, statusData interface{}) error {
	data, err := json.Marshal(statusData)
	if err != nil {
		return fmt.Errorf("序列化主机状态失败: %w", err)
	}
	_, err = d.db.Exec("INSERT INTO hs_status (hs_name, status_data) VALUES (?, ?)", hsName, string(data))
	return err
}

// GetHSStatus 获取主机最新状态
func (d *DB) GetHSStatus(hsName string, limit int) ([]string, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := d.db.Query("SELECT status_data FROM hs_status WHERE hs_name=? ORDER BY recorded_at DESC LIMIT ?", hsName, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []string
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		result = append(result, data)
	}
	return result, rows.Err()
}

// ============================================================
// 日志操作
// ============================================================

// AddLogger 添加日志记录
func (d *DB) AddLogger(hsName string, logData interface{}, logLevel string) error {
	data, err := json.Marshal(logData)
	if err != nil {
		return fmt.Errorf("序列化日志数据失败: %w", err)
	}
	_, err = d.db.Exec("INSERT INTO hs_logger (hs_name, log_data, log_level) VALUES (?, ?, ?)",
		hsName, string(data), logLevel,
	)
	return err
}

// GetLoggers 获取日志列表
func (d *DB) GetLoggers(hsName, logLevel string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 100
	}
	query := "SELECT id, hs_name, log_data, log_level, created_at FROM hs_logger WHERE 1=1"
	args := []interface{}{}
	if hsName != "" {
		query += " AND hs_name=?"
		args = append(args, hsName)
	}
	if logLevel != "" {
		query += " AND log_level=?"
		args = append(args, logLevel)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var id int
		var hn, ld, ll, ca string
		if err := rows.Scan(&id, &hn, &ld, &ll, &ca); err != nil {
			return nil, err
		}
		var logDataObj interface{}
		_ = json.Unmarshal([]byte(ld), &logDataObj)
		result = append(result, map[string]interface{}{
			"id": id, "hs_name": hn, "log_data": logDataObj, "log_level": ll, "created_at": ca,
		})
	}
	return result, rows.Err()
}

// ClearLoggers 清空日志
func (d *DB) ClearLoggers(hsName string) error {
	if hsName != "" {
		_, err := d.db.Exec("DELETE FROM hs_logger WHERE hs_name=?", hsName)
		return err
	}
	_, err := d.db.Exec("DELETE FROM hs_logger")
	return err
}

// DeleteOldLoggers 删除N天前的日志
func (d *DB) DeleteOldLoggers(hsName string, days int) (int64, error) {
	var res sql.Result
	var err error
	if hsName != "" {
		res, err = d.db.Exec("DELETE FROM hs_logger WHERE hs_name=? AND created_at < datetime('now', ?)",
			hsName, fmt.Sprintf("-%d days", days))
	} else {
		res, err = d.db.Exec("DELETE FROM hs_logger WHERE created_at < datetime('now', ?)",
			fmt.Sprintf("-%d days", days))
	}
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ============================================================
// 任务操作
// ============================================================

// AddTask 添加任务记录
func (d *DB) AddTask(hsName string, taskData interface{}) error {
	data, err := json.Marshal(taskData)
	if err != nil {
		return fmt.Errorf("序列化任务数据失败: %w", err)
	}
	_, err = d.db.Exec("INSERT INTO vm_tasker (hs_name, task_data) VALUES (?, ?)", hsName, string(data))
	return err
}

// GetTasks 获取任务列表
func (d *DB) GetTasks(hsName string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 50
	}
	query := "SELECT id, hs_name, task_data, created_at FROM vm_tasker"
	args := []interface{}{}
	if hsName != "" {
		query += " WHERE hs_name=?"
		args = append(args, hsName)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var id int
		var hn, td, ca string
		if err := rows.Scan(&id, &hn, &td, &ca); err != nil {
			return nil, err
		}
		var taskDataObj interface{}
		_ = json.Unmarshal([]byte(td), &taskDataObj)
		result = append(result, map[string]interface{}{
			"id": id, "hs_name": hn, "task_data": taskDataObj, "created_at": ca,
		})
	}
	return result, rows.Err()
}

// ============================================================
// 用户操作
// ============================================================

// GetUserByUsername 按用户名查询用户
func (d *DB) GetUserByUsername(username string) (map[string]interface{}, error) {
	return d.getUserBy("username", username)
}

// GetUserByEmail 按邮箱查询用户
func (d *DB) GetUserByEmail(email string) (map[string]interface{}, error) {
	return d.getUserBy("email", email)
}

// GetUserByID 按ID查询用户
func (d *DB) GetUserByID(id int) (map[string]interface{}, error) {
	return d.getUserBy("id", id)
}

func (d *DB) getUserBy(field string, value interface{}) (map[string]interface{}, error) {
	query := fmt.Sprintf(`SELECT id, username, password, email, is_admin, is_active,
		email_verified, verify_token, reset_token, can_create_vm, can_delete_vm, can_modify_vm,
		quota_cpu, quota_ram, quota_ssd, quota_gpu, quota_nat_ports, quota_web_proxy,
		quota_nat_ips, quota_pub_ips, quota_bandwidth_up, quota_bandwidth_down, quota_traffic,
		used_cpu, used_ram, used_ssd, used_gpu, used_nat_ports, used_web_proxy,
		used_nat_ips, used_pub_ips, used_bandwidth_up, used_bandwidth_down, used_traffic,
		assigned_hosts, created_at, updated_at, COALESCE(last_login,'') as last_login
		FROM web_users WHERE %s = ?`, field)

	row := d.db.QueryRow(query, value)
	return d.scanUser(row)
}

func (d *DB) scanUser(row *sql.Row) (map[string]interface{}, error) {
	var (
		id, isAdmin, isActive, emailVerified int
		canCreateVM, canDeleteVM, canModifyVM int
		quotaCPU, quotaRAM, quotaSSD, quotaGPU int
		quotaNatPorts, quotaWebProxy, quotaNatIPs, quotaPubIPs int
		quotaBandwidthUp, quotaBandwidthDown, quotaTraffic int
		usedCPU, usedRAM, usedSSD, usedGPU int
		usedNatPorts, usedWebProxy, usedNatIPs, usedPubIPs int
		usedBandwidthUp, usedBandwidthDown, usedTraffic int
		username, password, email, verifyToken, resetToken string
		assignedHosts, createdAt, updatedAt, lastLogin string
	)
	err := row.Scan(
		&id, &username, &password, &email, &isAdmin, &isActive,
		&emailVerified, &verifyToken, &resetToken, &canCreateVM, &canDeleteVM, &canModifyVM,
		&quotaCPU, &quotaRAM, &quotaSSD, &quotaGPU, &quotaNatPorts, &quotaWebProxy,
		&quotaNatIPs, &quotaPubIPs, &quotaBandwidthUp, &quotaBandwidthDown, &quotaTraffic,
		&usedCPU, &usedRAM, &usedSSD, &usedGPU, &usedNatPorts, &usedWebProxy,
		&usedNatIPs, &usedPubIPs, &usedBandwidthUp, &usedBandwidthDown, &usedTraffic,
		&assignedHosts, &createdAt, &updatedAt, &lastLogin,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var hosts []string
	_ = json.Unmarshal([]byte(assignedHosts), &hosts)
	if hosts == nil {
		hosts = []string{}
	}

	return map[string]interface{}{
		"id": id, "username": username, "password": password, "email": email,
		"is_admin": isAdmin == 1, "is_active": isActive == 1,
		"email_verified": emailVerified == 1, "verify_token": verifyToken, "reset_token": resetToken,
		"can_create_vm": canCreateVM == 1, "can_delete_vm": canDeleteVM == 1, "can_modify_vm": canModifyVM == 1,
		"quota_cpu": quotaCPU, "quota_ram": quotaRAM, "quota_ssd": quotaSSD, "quota_gpu": quotaGPU,
		"quota_nat_ports": quotaNatPorts, "quota_web_proxy": quotaWebProxy,
		"quota_nat_ips": quotaNatIPs, "quota_pub_ips": quotaPubIPs,
		"quota_bandwidth_up": quotaBandwidthUp, "quota_bandwidth_down": quotaBandwidthDown, "quota_traffic": quotaTraffic,
		"used_cpu": usedCPU, "used_ram": usedRAM, "used_ssd": usedSSD, "used_gpu": usedGPU,
		"used_nat_ports": usedNatPorts, "used_web_proxy": usedWebProxy,
		"used_nat_ips": usedNatIPs, "used_pub_ips": usedPubIPs,
		"used_bandwidth_up": usedBandwidthUp, "used_bandwidth_down": usedBandwidthDown, "used_traffic": usedTraffic,
		"assigned_hosts": hosts, "created_at": createdAt, "updated_at": updatedAt, "last_login": lastLogin,
	}, nil
}

// GetAllUsers 获取所有用户
func (d *DB) GetAllUsers() ([]map[string]interface{}, error) {
	rows, err := d.db.Query(`SELECT id, username, password, email, is_admin, is_active,
		email_verified, verify_token, reset_token, can_create_vm, can_delete_vm, can_modify_vm,
		quota_cpu, quota_ram, quota_ssd, quota_gpu, quota_nat_ports, quota_web_proxy,
		quota_nat_ips, quota_pub_ips, quota_bandwidth_up, quota_bandwidth_down, quota_traffic,
		used_cpu, used_ram, used_ssd, used_gpu, used_nat_ports, used_web_proxy,
		used_nat_ips, used_pub_ips, used_bandwidth_up, used_bandwidth_down, used_traffic,
		assigned_hosts, created_at, updated_at, COALESCE(last_login,'') as last_login
		FROM web_users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []map[string]interface{}
	for rows.Next() {
		var (
			id, isAdmin, isActive, emailVerified int
			canCreateVM, canDeleteVM, canModifyVM int
			quotaCPU, quotaRAM, quotaSSD, quotaGPU int
			quotaNatPorts, quotaWebProxy, quotaNatIPs, quotaPubIPs int
			quotaBandwidthUp, quotaBandwidthDown, quotaTraffic int
			usedCPU, usedRAM, usedSSD, usedGPU int
			usedNatPorts, usedWebProxy, usedNatIPs, usedPubIPs int
			usedBandwidthUp, usedBandwidthDown, usedTraffic int
			username, password, email, verifyToken, resetToken string
			assignedHosts, createdAt, updatedAt, lastLogin string
		)
		if err := rows.Scan(
			&id, &username, &password, &email, &isAdmin, &isActive,
			&emailVerified, &verifyToken, &resetToken, &canCreateVM, &canDeleteVM, &canModifyVM,
			&quotaCPU, &quotaRAM, &quotaSSD, &quotaGPU, &quotaNatPorts, &quotaWebProxy,
			&quotaNatIPs, &quotaPubIPs, &quotaBandwidthUp, &quotaBandwidthDown, &quotaTraffic,
			&usedCPU, &usedRAM, &usedSSD, &usedGPU, &usedNatPorts, &usedWebProxy,
			&usedNatIPs, &usedPubIPs, &usedBandwidthUp, &usedBandwidthDown, &usedTraffic,
			&assignedHosts, &createdAt, &updatedAt, &lastLogin,
		); err != nil {
			return nil, err
		}
		var hosts []string
		_ = json.Unmarshal([]byte(assignedHosts), &hosts)
		if hosts == nil {
			hosts = []string{}
		}
		result = append(result, map[string]interface{}{
			"id": id, "username": username, "password": password, "email": email,
			"is_admin": isAdmin == 1, "is_active": isActive == 1,
			"email_verified": emailVerified == 1, "verify_token": verifyToken, "reset_token": resetToken,
			"can_create_vm": canCreateVM == 1, "can_delete_vm": canDeleteVM == 1, "can_modify_vm": canModifyVM == 1,
			"quota_cpu": quotaCPU, "quota_ram": quotaRAM, "quota_ssd": quotaSSD, "quota_gpu": quotaGPU,
			"quota_nat_ports": quotaNatPorts, "quota_web_proxy": quotaWebProxy,
			"quota_nat_ips": quotaNatIPs, "quota_pub_ips": quotaPubIPs,
			"quota_bandwidth_up": quotaBandwidthUp, "quota_bandwidth_down": quotaBandwidthDown, "quota_traffic": quotaTraffic,
			"used_cpu": usedCPU, "used_ram": usedRAM, "used_ssd": usedSSD, "used_gpu": usedGPU,
			"used_nat_ports": usedNatPorts, "used_web_proxy": usedWebProxy,
			"used_nat_ips": usedNatIPs, "used_pub_ips": usedPubIPs,
			"used_bandwidth_up": usedBandwidthUp, "used_bandwidth_down": usedBandwidthDown, "used_traffic": usedTraffic,
			"assigned_hosts": hosts, "created_at": createdAt, "updated_at": updatedAt, "last_login": lastLogin,
		})
	}
	return result, rows.Err()
}

// CreateUser 创建用户
func (d *DB) CreateUser(username, password, email string, isAdmin bool) (int64, error) {
	adminVal := 0
	if isAdmin {
		adminVal = 1
	}
	hostsJSON, _ := json.Marshal([]string{})
	res, err := d.db.Exec(`INSERT INTO web_users (username, password, email, is_admin, assigned_hosts)
		VALUES (?, ?, ?, ?, ?)`, username, password, email, adminVal, string(hostsJSON))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateUser 更新用户信息（通用字段更新）
func (d *DB) UpdateUser(id int, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	
	// 字段白名单 - 防止SQL注入
	allowedFields := map[string]bool{
		"username":              true,
		"email":                 true,
		"password":              true,
		"is_admin":              true,
		"is_active":             true,
		"email_verified":        true,
		"verify_token":          true,
		"reset_token":           true,
		"reset_token_expires":   true,
		"can_create_vm":         true,
		"can_delete_vm":         true,
		"can_modify_vm":         true,
		"quota_cpu":             true,
		"quota_ram":             true,
		"quota_ssd":             true,
		"quota_gpu":             true,
		"quota_nat_ports":       true,
		"quota_web_proxy":       true,
		"quota_nat_ips":         true,
		"quota_pub_ips":         true,
		"quota_bandwidth_up":    true,
		"quota_bandwidth_down":  true,
		"quota_traffic":         true,
		"used_cpu":              true,
		"used_ram":              true,
		"used_ssd":              true,
		"used_gpu":              true,
		"assigned_hosts":        true,
	}
	
	// 验证所有字段名
	for k := range fields {
		if !allowedFields[k] {
			return fmt.Errorf("不允许更新字段: %s", k)
		}
	}
	
	query := "UPDATE web_users SET "
	args := []interface{}{}
	i := 0
	for k, v := range fields {
		if i > 0 {
			query += ", "
		}
		query += k + "=?"
		args = append(args, v)
		i++
	}
	query += ", updated_at=CURRENT_TIMESTAMP WHERE id=?"
	args = append(args, id)
	_, err := d.db.Exec(query, args...)
	return err
}

// DeleteUser 删除用户
func (d *DB) DeleteUser(id int) error {
	_, err := d.db.Exec("DELETE FROM web_users WHERE id=?", id)
	return err
}

// UpdateUserLastLogin 更新最后登录时间
func (d *DB) UpdateUserLastLogin(username string) error {
	_, err := d.db.Exec("UPDATE web_users SET last_login=CURRENT_TIMESTAMP WHERE username=?", username)
	return err
}

// GetUserByVerifyToken 按验证Token查询用户
func (d *DB) GetUserByVerifyToken(token string) (map[string]interface{}, error) {
	return d.getUserBy("verify_token", token)
}

// GetUserByResetToken 按重置Token查询用户
func (d *DB) GetUserByResetToken(token string) (map[string]interface{}, error) {
	return d.getUserBy("reset_token", token)
}

// InitDefaultAdmin 若不存在任何管理员用户，则以 bearerToken 为初始密码创建默认 admin 账号
func (d *DB) InitDefaultAdmin(bearerToken string) error {
	var count int
	err := d.db.QueryRow("SELECT COUNT(*) FROM web_users WHERE is_admin = 1").Scan(&count)
	if err != nil {
		return fmt.Errorf("查询管理员用户失败: %w", err)
	}
	if count > 0 {
		return nil // 已存在管理员，跳过
	}

	// 用 bcrypt 对 bearerToken 做哈希作为初始密码
	hashBytes, err := bcrypt.GenerateFromPassword([]byte(bearerToken), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("密码哈希失败: %w", err)
	}

	hostsJSON, _ := json.Marshal([]string{})
	_, err = d.db.Exec(`INSERT INTO web_users
		(username, password, email, is_admin, is_active, email_verified,
		 can_create_vm, can_modify_vm, can_delete_vm,
		 quota_cpu, quota_ram, quota_ssd, quota_gpu,
		 quota_nat_ports, quota_web_proxy, quota_nat_ips, quota_pub_ips,
		 quota_bandwidth_up, quota_bandwidth_down, quota_traffic,
		 assigned_hosts)
		VALUES (?, ?, ?, 1, 1, 1, 1, 1, 1, 9999, 9999, 9999, 9999, 9999, 9999, 9999, 9999, 9999, 9999, 9999, ?)`,
		"admin", string(hashBytes), "admin@localhost", string(hostsJSON))
	if err != nil {
		return fmt.Errorf("创建默认管理员失败: %w", err)
	}
	return nil
}

// ============================================================
// 配额和清理服务辅助方法
// ============================================================

// CountUserNATRules 统计用户已用NAT端口数
func (d *DB) CountUserNATRules(userID int) (int, error) {
	var count int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM nat_rules WHERE user_id=?`, userID).Scan(&count)
	return count, err
}

// CountUserWebProxies 统计用户已用Web代理数
func (d *DB) CountUserWebProxies(userID int) (int, error) {
	var count int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM web_proxies WHERE user_id=?`, userID).Scan(&count)
	return count, err
}

// DeleteVMStatus 删除虚拟机状态缓存
func (d *DB) DeleteVMStatus(hsName, vmUUID string) error {
	_, err := d.db.Exec("DELETE FROM vm_status WHERE hs_name=? AND vm_uuid=?", hsName, vmUUID)
	return err
}

// DeleteNATRule 删除NAT规则记录
func (d *DB) DeleteNATRule(hsName, vmUUID string, wanPort int) error {
	_, err := d.db.Exec("DELETE FROM nat_rules WHERE hs_name=? AND vm_uuid=? AND wan_port=?", hsName, vmUUID, wanPort)
	return err
}

// DeleteWebProxy 删除Web代理记录
func (d *DB) DeleteWebProxy(hsName, vmUUID string, webName string) error {
	_, err := d.db.Exec("DELETE FROM web_proxies WHERE hs_name=? AND vm_uuid=? AND web_name=?", hsName, vmUUID, webName)
	return err
}

// GetHostByName 根据主机名获取主机配置
func (d *DB) GetHostByName(hsName string) (map[string]interface{}, error) {
	var configJSON string
	err := d.db.QueryRow("SELECT hs_config FROM hs_saving WHERE hs_name=?", hsName).Scan(&configJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	
	var config map[string]interface{}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
		return nil, fmt.Errorf("解析主机配置失败: %w", err)
	}
	return config, nil
}

// GetVMsByHost 获取主机下的所有虚拟机配置
func (d *DB) GetVMsByHost(hsName string) ([]map[string]interface{}, error) {
	rows, err := d.db.Query("SELECT vm_config FROM vm_saving WHERE hs_name=?", hsName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	
	var result []map[string]interface{}
	for rows.Next() {
		var configJSON string
		if err := rows.Scan(&configJSON); err != nil {
			return nil, err
		}
		var config map[string]interface{}
		if err := json.Unmarshal([]byte(configJSON), &config); err != nil {
			continue
		}
		result = append(result, config)
	}
	return result, rows.Err()
}

// GetNATRulesByHost 获取主机下的所有NAT规则
func (d *DB) GetNATRulesByHost(hsName string) ([]map[string]interface{}, error) {
	rows, err := d.db.Query("SELECT vm_uuid, wan_port, lan_addr, lan_port FROM nat_rules WHERE hs_name=?", hsName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	
	var result []map[string]interface{}
	for rows.Next() {
		var vmUUID, lanAddr string
		var wanPort, lanPort int
		if err := rows.Scan(&vmUUID, &wanPort, &lanAddr, &lanPort); err != nil {
			return nil, err
		}
		result = append(result, map[string]interface{}{
			"vm_uuid":  vmUUID,
			"wan_port": wanPort,
			"lan_addr": lanAddr,
			"lan_port": lanPort,
		})
	}
	return result, rows.Err()
}

// GetWebProxiesByHost 获取主机下的所有Web代理
func (d *DB) GetWebProxiesByHost(hsName string) ([]map[string]interface{}, error) {
	rows, err := d.db.Query("SELECT vm_uuid, web_name, web_domain, web_host, web_port FROM web_proxies WHERE hs_name=?", hsName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	
	var result []map[string]interface{}
	for rows.Next() {
		var vmUUID, webName, webDomain, webHost string
		var webPort int
		if err := rows.Scan(&vmUUID, &webName, &webDomain, &webHost, &webPort); err != nil {
			return nil, err
		}
		result = append(result, map[string]interface{}{
			"vm_uuid":    vmUUID,
			"web_name":   webName,
			"web_domain": webDomain,
			"web_host":   webHost,
			"web_port":   webPort,
		})
	}
	return result, rows.Err()
}


