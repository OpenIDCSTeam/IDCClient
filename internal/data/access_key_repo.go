// Package data 虚拟机访问密钥数据访问层
//
// 访问密钥（AccessKey）是绑定到单台虚拟机的长期凭据：16 位大小写字母 + 数字，
// 可用于「登录页直接登录该虚拟机」与「URL 带密钥跳转」两种场景，
// 登录后的会话权限与临时凭据（temptoken）完全一致。
//
// 之所以单独建表而不是塞进 vm_saving 的 JSON 配置：
//   - 登录时需要「按密钥反查虚拟机」，JSON 里存无法建唯一索引，只能全表扫描；
//   - 配置会参与平台扫描 / 主机间同步合并，混在配置里存在被覆盖的风险；
//   - 密钥属于凭据，不应随虚拟机详情接口整体下发给所有者。
package data

import (
	"database/sql"
	"fmt"
	"time"
)

// VMAccessKeyRow 虚拟机访问密钥记录
type VMAccessKeyRow struct {
	AccessKey string
	HsName    string
	VmUUID    string
	CreatedBy string
	CreatedAt int64
	UpdatedAt int64
}

const vmAccessKeyColumns = `access_key, hs_name, vm_uuid, created_by, created_at, updated_at`

func scanVMAccessKeyRow(scanner interface {
	Scan(dest ...interface{}) error
}) (*VMAccessKeyRow, error) {
	var row VMAccessKeyRow
	if err := scanner.Scan(
		&row.AccessKey, &row.HsName, &row.VmUUID,
		&row.CreatedBy, &row.CreatedAt, &row.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// UpsertVMAccessKey 为虚拟机写入（或替换）访问密钥。
//
// access_key 带 UNIQUE 约束：同一台虚拟机重新生成密钥时先删除旧记录，
// 因此不会出现「一台虚拟机多个有效密钥」的歧义状态。
func (d *DB) UpsertVMAccessKey(hsName, vmUUID, accessKey, createdBy string) error {
	if hsName == "" || vmUUID == "" || accessKey == "" {
		return fmt.Errorf("访问密钥参数不完整")
	}
	now := time.Now().Unix()

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec("DELETE FROM vm_access_key WHERE hs_name = ? AND vm_uuid = ?", hsName, vmUUID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO vm_access_key (access_key, hs_name, vm_uuid, created_by, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		accessKey, hsName, vmUUID, createdBy, now, now,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// GetVMAccessKeyByVM 按主机 + 虚拟机查询访问密钥，不存在返回 (nil, nil)
func (d *DB) GetVMAccessKeyByVM(hsName, vmUUID string) (*VMAccessKeyRow, error) {
	query := `SELECT ` + vmAccessKeyColumns + ` FROM vm_access_key WHERE hs_name = ? AND vm_uuid = ?`
	return scanVMAccessKeyRow(d.db.QueryRow(query, hsName, vmUUID))
}

// GetVMAccessKeyByKey 按密钥反查虚拟机（登录时使用）
func (d *DB) GetVMAccessKeyByKey(accessKey string) (*VMAccessKeyRow, error) {
	if accessKey == "" {
		return nil, nil
	}
	query := `SELECT ` + vmAccessKeyColumns + ` FROM vm_access_key WHERE access_key = ?`
	return scanVMAccessKeyRow(d.db.QueryRow(query, accessKey))
}

// DeleteVMAccessKey 撤销虚拟机的访问密钥，返回是否确实删除了记录
func (d *DB) DeleteVMAccessKey(hsName, vmUUID string) (bool, error) {
	res, err := d.db.Exec("DELETE FROM vm_access_key WHERE hs_name = ? AND vm_uuid = ?", hsName, vmUUID)
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}
