package data

import (
	"database/sql"
	"errors"
	"fmt"
)

// RestoreHostConfigs 仅替换配置行，不删除主机、虚拟机或其他关联记录。
func (d *DB) RestoreHostConfigs(rows []*HSConfigRow, overwrite bool) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		if row == nil || row.HsName == "" || seen[row.HsName] {
			return errors.New("主机配置为空或名称重复")
		}
		seen[row.HsName] = true
		var serverType string
		err = tx.QueryRow("SELECT server_type FROM hs_config WHERE hs_name=?", row.HsName).Scan(&serverType)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			err = createHostRow(tx, row)
		case err != nil:
			return err
		case !overwrite:
			return fmt.Errorf("主机 %s 已存在，请确认覆盖", row.HsName)
		case serverType != row.ServerType:
			return fmt.Errorf("主机 %s 的引擎类型不一致，不能覆盖", row.HsName)
		default:
			err = updateHostRow(tx, row)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
