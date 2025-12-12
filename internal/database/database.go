package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/OpenIDCSTeam/OpenIDCS-Client-Go/internal/config"
	_ "modernc.org/sqlite"
	"github.com/sirupsen/logrus"
)

// Database 数据库结构体
type Database struct {
	db     *sql.DB
	config *config.DatabaseConfig
	logger *logrus.Logger
}

// NewDatabase 创建新的数据库实例
func NewDatabase(cfg *config.DatabaseConfig, logger *logrus.Logger) (*Database, error) {
	// 确保数据库目录存在
	dbDir := filepath.Dir(cfg.Path)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return nil, fmt.Errorf("创建数据库目录失败: %w", err)
	}

	// 打开数据库连接
	db, err := sql.Open("sqlite", cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 设置连接池参数
	db.SetMaxOpenConns(cfg.MaxConn)
	db.SetMaxIdleConns(cfg.IdleConn)
	db.SetConnMaxLifetime(time.Hour)

	// 测试连接
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("数据库连接测试失败: %w", err)
	}

	database := &Database{
		db:     db,
		config: cfg,
		logger: logger,
	}

	// 初始化数据库表结构
	if err := database.initTables(); err != nil {
		return nil, fmt.Errorf("初始化数据库表失败: %w", err)
	}

	logger.Info("数据库初始化成功")
	return database, nil
}

// Close 关闭数据库连接
func (d *Database) Close() error {
	if d.db != nil {
		return d.db.Close()
	}
	return nil
}

// GetDB 获取数据库连接
func (d *Database) GetDB() *sql.DB {
	return d.db
}

// initTables 初始化数据库表结构
func (d *Database) initTables() error {
	// 创建全局配置表
	globalSQL := `
	CREATE TABLE IF NOT EXISTS hs_global (
		id TEXT PRIMARY KEY,
		data TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`

	// 创建主机配置表
	hsConfigSQL := `
	CREATE TABLE IF NOT EXISTS hs_config (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		hs_name TEXT UNIQUE NOT NULL,
		server_name TEXT NOT NULL,
		server_type TEXT NOT NULL,
		server_addr TEXT NOT NULL,
		server_user TEXT NOT NULL,
		server_pass TEXT NOT NULL,
		filter_name TEXT DEFAULT '',
		images_path TEXT DEFAULT '',
		system_path TEXT DEFAULT '',
		backup_path TEXT DEFAULT '',
		extern_path TEXT DEFAULT '',
		launch_path TEXT DEFAULT '',
		network_nat TEXT DEFAULT '',
		network_pub TEXT DEFAULT '',
		i_kuai_addr TEXT DEFAULT '',
		i_kuai_user TEXT DEFAULT '',
		i_kuai_pass TEXT DEFAULT '',
		ports_start INTEGER DEFAULT 5900,
		ports_close INTEGER DEFAULT 5999,
		remote_port INTEGER DEFAULT 0,
		system_maps TEXT DEFAULT '{}',
		public_addr TEXT DEFAULT '[]',
		extend_data TEXT DEFAULT '{}',
		server_dnss TEXT DEFAULT '[]',
		limits_nums INTEGER DEFAULT 0,
		ipaddr_maps TEXT DEFAULT '{}',
		ipaddr_dnss TEXT DEFAULT '["119.29.29.29", "223.5.5.5"]',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`

	// 创建主机状态表
	hsStatusSQL := `
	CREATE TABLE IF NOT EXISTS hs_status (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		hs_name TEXT NOT NULL,
		status_data TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`

	// 创建虚拟机配置表
	vmSavingSQL := `
	CREATE TABLE IF NOT EXISTS vm_saving (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		hs_name TEXT NOT NULL,
		vm_uuid TEXT NOT NULL,
		vm_config TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(hs_name, vm_uuid)
	);`

	// 创建虚拟机状态表
	vmStatusSQL := `
	CREATE TABLE IF NOT EXISTS vm_status (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		hs_name TEXT NOT NULL,
		vm_uuid TEXT NOT NULL,
		status_data TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`

	// 创建虚拟机任务表
	vmTaskerSQL := `
	CREATE TABLE IF NOT EXISTS vm_tasker (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		hs_name TEXT NOT NULL,
		task_data TEXT NOT NULL,
		status TEXT DEFAULT 'pending',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`

	// 创建全局代理配置表
	webProxySQL := `
	CREATE TABLE IF NOT EXISTS web_proxy (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		lan_port INTEGER NOT NULL,
		lan_addr TEXT NOT NULL,
		web_addr TEXT UNIQUE NOT NULL,
		web_tips TEXT DEFAULT '',
		is_https INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`

	// 创建日志表
	hsLoggerSQL := `
	CREATE TABLE IF NOT EXISTS hs_logger (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		hs_name TEXT,
		log_data TEXT NOT NULL,
		log_level TEXT DEFAULT 'INFO',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`

	// 执行表创建SQL
	sqls := []string{
		globalSQL, hsConfigSQL, hsStatusSQL,
		vmSavingSQL, vmStatusSQL, vmTaskerSQL,
		webProxySQL, hsLoggerSQL,
	}

	for _, sql := range sqls {
		if _, err := d.db.Exec(sql); err != nil {
			return fmt.Errorf("创建表失败: %w", err)
		}
	}

	// 创建索引
	indexes := []string{
		"CREATE INDEX IF NOT EXISTS idx_hs_config_name ON hs_config(hs_name);",
		"CREATE INDEX IF NOT EXISTS idx_hs_status_name ON hs_status(hs_name);",
		"CREATE INDEX IF NOT EXISTS idx_vm_saving_name ON vm_saving(hs_name);",
		"CREATE INDEX IF NOT EXISTS idx_vm_status_name ON vm_status(hs_name);",
		"CREATE INDEX IF NOT EXISTS idx_vm_status_uuid ON vm_status(hs_name, vm_uuid);",
		"CREATE INDEX IF NOT EXISTS idx_vm_tasker_name ON vm_tasker(hs_name);",
		"CREATE INDEX IF NOT EXISTS idx_hs_logger_name ON hs_logger(hs_name);",
		"CREATE INDEX IF NOT EXISTS idx_hs_logger_created ON hs_logger(created_at);",
	}

	for _, indexSQL := range indexes {
		if _, err := d.db.Exec(indexSQL); err != nil {
			d.logger.Warnf("创建索引失败: %v", err)
		}
	}

	d.logger.Info("数据库表结构初始化完成")
	return nil
}

// Begin 开始事务
func (d *Database) Begin() (*sql.Tx, error) {
	return d.db.Begin()
}

// Exec 执行SQL语句
func (d *Database) Exec(query string, args ...interface{}) (sql.Result, error) {
	return d.db.Exec(query, args...)
}

// Query 查询多行
func (d *Database) Query(query string, args ...interface{}) (*sql.Rows, error) {
	return d.db.Query(query, args...)
}

// QueryRow 查询单行
func (d *Database) QueryRow(query string, args ...interface{}) *sql.Row {
	return d.db.QueryRow(query, args...)
}

// GetStats 获取数据库统计信息
func (d *Database) GetStats() map[string]int64 {
	stats := make(map[string]int64)
	
	tables := []string{"hs_global", "hs_config", "hs_status", "vm_saving", "vm_status", "vm_tasker", "web_proxy", "hs_logger"}
	
	for _, table := range tables {
		var count int64
		err := d.db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM %s", table)).Scan(&count)
		if err != nil {
			d.logger.Warnf("获取表 %s 统计信息失败: %v", table, err)
			stats[table] = 0
		} else {
			stats[table] = count
		}
	}
	
	return stats
}

// Backup 备份数据库
func (d *Database) Backup(backupPath string) error {
	// 确保备份目录存在
	backupDir := filepath.Dir(backupPath)
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return fmt.Errorf("创建备份目录失败: %w", err)
	}

	// 使用SQLite的备份API
	src, err := sql.Open("sqlite", d.config.Path)
	if err != nil {
		return fmt.Errorf("打开源数据库失败: %w", err)
	}
	defer src.Close()

	dst, err := sql.Open("sqlite", backupPath)
	if err != nil {
		return fmt.Errorf("创建备份数据库失败: %w", err)
	}
	defer dst.Close()

	// 执行备份
	_, err = dst.Exec(fmt.Sprintf("ATTACH DATABASE '%s' AS backup", d.config.Path))
	if err != nil {
		return fmt.Errorf("附加源数据库失败: %w", err)
	}

	_, err = dst.Exec(`
		BEGIN IMMEDIATE;
		DELETE FROM main.hs_global;
		INSERT INTO main.hs_global SELECT * FROM backup.hs_global;
		DELETE FROM main.hs_config;
		INSERT INTO main.hs_config SELECT * FROM backup.hs_config;
		DELETE FROM main.hs_status;
		INSERT INTO main.hs_status SELECT * FROM backup.hs_status;
		DELETE FROM main.vm_saving;
		INSERT INTO main.vm_saving SELECT * FROM backup.vm_saving;
		DELETE FROM main.vm_status;
		INSERT INTO main.vm_status SELECT * FROM backup.vm_status;
		DELETE FROM main.vm_tasker;
		INSERT INTO main.vm_tasker SELECT * FROM backup.vm_tasker;
		DELETE FROM main.web_proxy;
		INSERT INTO main.web_proxy SELECT * FROM backup.web_proxy;
		DELETE FROM main.hs_logger;
		INSERT INTO main.hs_logger SELECT * FROM backup.hs_logger;
		COMMIT;
		DETACH DATABASE backup;
	`)

	if err != nil {
		return fmt.Errorf("执行备份失败: %w", err)
	}

	d.logger.Infof("数据库备份成功: %s", backupPath)
	return nil
}

// Cleanup 清理过期数据
func (d *Database) Cleanup(days int) error {
	cutoffTime := time.Now().AddDate(0, 0, -days).Format(time.RFC3339)
	
	// 清理过期状态数据
	tables := []string{"hs_status", "vm_status", "hs_logger"}
	
	for _, table := range tables {
		result, err := d.db.Exec(
			fmt.Sprintf("DELETE FROM %s WHERE created_at < ?", table),
			cutoffTime,
		)
		if err != nil {
			d.logger.Warnf("清理表 %s 失败: %v", table, err)
		} else {
			if rowsAffected, _ := result.RowsAffected(); rowsAffected > 0 {
				d.logger.Infof("清理表 %s: %d 行", table, rowsAffected)
			}
		}
	}
	
	return nil
}