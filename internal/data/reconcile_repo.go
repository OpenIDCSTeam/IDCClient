// Package data 外部资源补偿（outbox）与删除墓碑（tombstone）数据访问层
//
// 背景：虚拟机的删除会同时影响多个外部系统（平台侧虚拟机、路由器端口映射、
// Caddy 反向代理、用户配额）以及本地多张表。这些系统无法参与同一个事务，
// 进程在任意一步崩溃都会留下不一致状态。因此引入：
//
//   - tombstone：持久化「这台虚拟机正在被删除」的意图与各步骤完成情况，
//     使删除可以在重启后从断点继续，而不是停留在半删状态；
//   - outbox：把无法立即完成的外部调用（如路由器销账）持久化为待补偿任务，
//     由后台按退避策略重试，避免「外部残留但本地已无记录」。
package data

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// ============================================================
// 外部资源补偿任务（outbox）
// ============================================================

// 补偿动作类型
const (
	// OutboxActionNATRevoke 撤销路由器端口映射
	OutboxActionNATRevoke = "nat_revoke"
	// OutboxActionStaticUnbind 解除路由器静态绑定（DHCP 静态地址 + ARP）
	OutboxActionStaticUnbind = "static_unbind"
	// OutboxActionProxySync 重新下发反向代理配置
	OutboxActionProxySync = "proxy_sync"
	// OutboxActionProxyRevoke 撤销平台自有通道（如青州云）上的一条反向代理
	OutboxActionProxyRevoke = "proxy_revoke"
)

// 补偿任务状态
const (
	OutboxPending = "pending"
	OutboxDone    = "done"
	OutboxFailed  = "failed"
)

// OutboxEntry 外部资源补偿任务
type OutboxEntry struct {
	ID          int64                  `json:"id"`
	HsName      string                 `json:"hs_name"`
	VmUUID      string                 `json:"vm_uuid"`
	Action      string                 `json:"action"`
	Payload     map[string]interface{} `json:"payload"`
	Status      string                 `json:"status"`
	Attempts    int                    `json:"attempts"`
	LastError   string                 `json:"last_error"`
	NextRetryAt int64                  `json:"next_retry_at"`
	CreatedAt   int64                  `json:"created_at"`
	UpdatedAt   int64                  `json:"updated_at"`
}

// CreateOutboxEntry 登记一条待补偿的外部资源操作。
//
// 同一 (hs_name, vm_uuid, action, payload) 已存在未完成任务时不重复插入，
// 保证删除流程重入时不会堆积重复任务。
func (d *DB) CreateOutboxEntry(hsName, vmUUID, action string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("序列化补偿任务失败: %w", err)
	}
	now := time.Now().Unix()

	var existing int64
	query := `SELECT COUNT(*) FROM resource_outbox
		WHERE hs_name = ? AND vm_uuid = ? AND action = ? AND payload = ? AND status = ?`
	if err := d.db.QueryRow(query, hsName, vmUUID, action, string(data), OutboxPending).Scan(&existing); err != nil {
		return err
	}
	if existing > 0 {
		return nil
	}

	_, err = d.db.Exec(`INSERT INTO resource_outbox
		(hs_name, vm_uuid, action, payload, status, attempts, last_error, next_retry_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, '', ?, ?, ?)`,
		hsName, vmUUID, action, string(data), OutboxPending, now, now, now)
	return err
}

// ListPendingOutbox 列出到期待重试的补偿任务
func (d *DB) ListPendingOutbox(limit int) ([]*OutboxEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.db.Query(`SELECT id, hs_name, vm_uuid, action, payload, status,
		attempts, last_error, next_retry_at, created_at, updated_at
		FROM resource_outbox
		WHERE status = ? AND next_retry_at <= ?
		ORDER BY id LIMIT ?`, OutboxPending, time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*OutboxEntry
	for rows.Next() {
		e := &OutboxEntry{}
		var payload string
		if err := rows.Scan(&e.ID, &e.HsName, &e.VmUUID, &e.Action, &payload, &e.Status,
			&e.Attempts, &e.LastError, &e.NextRetryAt, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		if payload != "" {
			_ = json.Unmarshal([]byte(payload), &e.Payload)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ListOutbox 按状态列出补偿任务（status 为空表示全部），供运维排查使用
func (d *DB) ListOutbox(status string, limit int) ([]*OutboxEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT id, hs_name, vm_uuid, action, payload, status,
		attempts, last_error, next_retry_at, created_at, updated_at
		FROM resource_outbox WHERE 1=1`
	args := []interface{}{}
	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []*OutboxEntry
	for rows.Next() {
		e := &OutboxEntry{}
		var payload string
		if err := rows.Scan(&e.ID, &e.HsName, &e.VmUUID, &e.Action, &payload, &e.Status,
			&e.Attempts, &e.LastError, &e.NextRetryAt, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		if payload != "" {
			_ = json.Unmarshal([]byte(payload), &e.Payload)
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

// ResetOutboxRetry 立即重试一条补偿任务（清除退避并重置状态）
func (d *DB) ResetOutboxRetry(id int64) error {
	_, err := d.db.Exec(`UPDATE resource_outbox
		SET status = ?, next_retry_at = ?, updated_at = ? WHERE id = ?`,
		OutboxPending, time.Now().Unix(), time.Now().Unix(), id)
	return err
}

// DismissOutbox 人工忽略一条补偿任务（标记为最终失败并记录原因）
func (d *DB) DismissOutbox(id int64, reason string) error {
	if reason == "" {
		reason = "人工忽略"
	}
	_, err := d.db.Exec(`UPDATE resource_outbox
		SET status = ?, last_error = ?, updated_at = ? WHERE id = ?`,
		OutboxFailed, reason, time.Now().Unix(), id)
	return err
}

// MarkOutboxDone 标记补偿任务已完成
func (d *DB) MarkOutboxDone(id int64) error {
	_, err := d.db.Exec(`UPDATE resource_outbox
		SET status = ?, last_error = '', updated_at = ? WHERE id = ?`,
		OutboxDone, time.Now().Unix(), id)
	return err
}

// MarkOutboxRetry 记录一次失败并安排下次重试。
// 退避时间随尝试次数指数增长，上限 60 分钟，避免持续打爆外部依赖。
func (d *DB) MarkOutboxRetry(id int64, errMsg string, maxAttempts int) error {
	var attempts int
	if err := d.db.QueryRow("SELECT attempts FROM resource_outbox WHERE id = ?", id).Scan(&attempts); err != nil {
		return err
	}
	attempts++

	backoff := time.Duration(1<<uint(minInt(attempts, 6))) * time.Minute
	if backoff > 60*time.Minute {
		backoff = 60 * time.Minute
	}

	status := OutboxPending
	if maxAttempts > 0 && attempts >= maxAttempts {
		status = OutboxFailed
	}

	_, err := d.db.Exec(`UPDATE resource_outbox
		SET status = ?, attempts = ?, last_error = ?, next_retry_at = ?, updated_at = ?
		WHERE id = ?`,
		status, attempts, errMsg, time.Now().Add(backoff).Unix(), time.Now().Unix(), id)
	return err
}

// PurgeOutboxDone 清理已完成/最终失败的补偿任务
func (d *DB) PurgeOutboxDone(days int) (int64, error) {
	if days <= 0 {
		days = 7
	}
	cutoff := time.Now().AddDate(0, 0, -days).Unix()
	res, err := d.db.Exec("DELETE FROM resource_outbox WHERE status != ? AND updated_at < ?",
		OutboxPending, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountPendingOutbox 统计待处理补偿任务数量（供可观测性使用）
func (d *DB) CountPendingOutbox() (int, error) {
	var n int
	err := d.db.QueryRow("SELECT COUNT(*) FROM resource_outbox WHERE status = ?", OutboxPending).Scan(&n)
	return n, err
}

// ============================================================
// 虚拟机删除墓碑（tombstone）
// ============================================================

// 删除步骤标识
const (
	TombstoneStepNAT      = "nat_revoked"
	TombstoneStepStatic   = "static_unbound"
	TombstoneStepPlatform = "platform_deleted"
	TombstoneStepRecords  = "records_cleaned"
	TombstoneStepQuota    = "quota_released"
	TombstoneStepProxy    = "proxy_revoked"
)

// TombstonePending / TombstoneDone 墓碑状态
const (
	TombstonePending = "pending"
	TombstoneDone    = "done"
)

// Tombstone 记录一次虚拟机删除的意图与进度
type Tombstone struct {
	HsName    string          `json:"hs_name"`
	VmUUID    string          `json:"vm_uuid"`
	Status    string          `json:"status"`
	QuotaUser string          `json:"quota_user"`
	Steps     map[string]bool `json:"steps"`
	// VmConfig 保存删除前的虚拟机配置，供重启后继续计算并释放配额
	VmConfig  string `json:"vm_config"`
	LastError string `json:"last_error"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

// CreateTombstone 创建删除墓碑（幂等：已存在则保留原有进度）
func (d *DB) CreateTombstone(hsName, vmUUID, quotaUser, vmConfig string) error {
	now := time.Now().Unix()
	_, err := d.db.Exec(`INSERT INTO vm_tombstones
		(hs_name, vm_uuid, status, quota_user, steps, vm_config, last_error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, '', ?, ?)
		ON CONFLICT(hs_name, vm_uuid) DO UPDATE SET
			quota_user = excluded.quota_user,
			vm_config  = CASE WHEN excluded.vm_config != '' THEN excluded.vm_config ELSE vm_tombstones.vm_config END,
			updated_at = excluded.updated_at`,
		hsName, vmUUID, TombstonePending, quotaUser, "{}", vmConfig, now, now)
	return err
}

// GetTombstone 读取墓碑
func (d *DB) GetTombstone(hsName, vmUUID string) (*Tombstone, error) {
	return d.scanTombstone(d.db.QueryRow(`SELECT hs_name, vm_uuid, status, quota_user, steps,
		vm_config, last_error, created_at, updated_at
		FROM vm_tombstones WHERE hs_name = ? AND vm_uuid = ?`, hsName, vmUUID))
}

func (d *DB) scanTombstone(row *sql.Row) (*Tombstone, error) {
	t := &Tombstone{}
	var steps string
	err := row.Scan(&t.HsName, &t.VmUUID, &t.Status, &t.QuotaUser, &steps,
		&t.VmConfig, &t.LastError, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if steps != "" {
		_ = json.Unmarshal([]byte(steps), &t.Steps)
	}
	if t.Steps == nil {
		t.Steps = map[string]bool{}
	}
	return t, nil
}

// ListIncompleteTombstones 列出尚未完成的删除墓碑
func (d *DB) ListIncompleteTombstones(limit int) ([]*Tombstone, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.db.Query(`SELECT hs_name, vm_uuid, status, quota_user, steps,
		vm_config, last_error, created_at, updated_at
		FROM vm_tombstones WHERE status = ? ORDER BY id LIMIT ?`, TombstonePending, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []*Tombstone
	for rows.Next() {
		t := &Tombstone{}
		var steps string
		if err := rows.Scan(&t.HsName, &t.VmUUID, &t.Status, &t.QuotaUser, &steps,
			&t.VmConfig, &t.LastError, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		if steps != "" {
			_ = json.Unmarshal([]byte(steps), &t.Steps)
		}
		if t.Steps == nil {
			t.Steps = map[string]bool{}
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

// MarkTombstoneStep 标记某个删除步骤已完成
func (d *DB) MarkTombstoneStep(hsName, vmUUID, step string, lastErr string) error {
	t, err := d.GetTombstone(hsName, vmUUID)
	if err != nil || t == nil {
		return err
	}
	if t.Steps == nil {
		t.Steps = map[string]bool{}
	}
	t.Steps[step] = true

	data, _ := json.Marshal(t.Steps)
	_, err = d.db.Exec(`UPDATE vm_tombstones SET steps = ?, last_error = ?, updated_at = ?
		WHERE hs_name = ? AND vm_uuid = ?`, string(data), lastErr, time.Now().Unix(), hsName, vmUUID)
	return err
}

// SetTombstoneError 记录删除过程中的错误信息（不改变状态）
func (d *DB) SetTombstoneError(hsName, vmUUID, lastErr string) error {
	_, err := d.db.Exec(`UPDATE vm_tombstones SET last_error = ?, updated_at = ?
		WHERE hs_name = ? AND vm_uuid = ?`, lastErr, time.Now().Unix(), hsName, vmUUID)
	return err
}

// CompleteTombstone 标记删除流程全部完成
func (d *DB) CompleteTombstone(hsName, vmUUID string) error {
	_, err := d.db.Exec(`UPDATE vm_tombstones SET status = ?, updated_at = ?
		WHERE hs_name = ? AND vm_uuid = ?`, TombstoneDone, time.Now().Unix(), hsName, vmUUID)
	return err
}

// HasIncompleteTombstone 判断虚拟机是否处于删除过程中。
// 反向代理等配置生成逻辑据此排除正在删除的虚拟机，
// 避免已下线的代理被重新写回 Caddy 配置。
func (d *DB) HasIncompleteTombstone(hsName, vmUUID string) bool {
	var n int
	err := d.db.QueryRow(`SELECT COUNT(*) FROM vm_tombstones
		WHERE hs_name = ? AND vm_uuid = ? AND status = ?`,
		hsName, vmUUID, TombstonePending).Scan(&n)
	return err == nil && n > 0
}

// PurgeTombstones 清理已完成且超过保留期的墓碑
func (d *DB) PurgeTombstones(days int) (int64, error) {
	if days <= 0 {
		days = 30
	}
	cutoff := time.Now().AddDate(0, 0, -days).Unix()
	res, err := d.db.Exec("DELETE FROM vm_tombstones WHERE status = ? AND updated_at < ?",
		TombstoneDone, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// minInt 返回两个整数中的较小值
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
