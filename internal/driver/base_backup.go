// Package driver 基类通用备份实现
// 与 Python 版 BasicServer.VMBackup / Restores / RMSaves 语义一致：
// 关机 → 打包 system_path/<vm> → 备份到 backup_path → 开机 → 记录备份信息。
// 归档优先使用外部 7z（与 Python 一致，扩展名 .7z），未安装时回退标准库 tar.gz。
package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// powerForBackup 通过最外层驱动派发电源操作（模拟 Python 虚方法派发），
// 返回是否确实执行成功。备份/还原属于破坏性流程，
// 关机失败必须能被调用方感知，否则会在线打包或删除运行中的磁盘文件。
func (b *BaseDriver) powerForBackup(vmUUID string, on bool) bool {
	self := b.SelfDriver()
	if self == nil {
		return false
	}
	action := model.VMPowerSStart
	if !on {
		action = model.VMPowerHClose
	}
	msg, err := self.PowerControl(vmUUID, action)
	if err != nil {
		return false
	}
	if msg != nil && !msg.Success {
		return false
	}
	return true
}

// isVMRunning 判断虚拟机当前是否处于运行状态。
// 用于在备份/还原后恢复原有的电源状态，而不是无条件开机。
func (b *BaseDriver) isVMRunning(vmUUID string) bool {
	self := b.SelfDriver()
	if self == nil {
		return false
	}
	state, ok := PowerStatusMap[self.GetPower(vmUUID)]
	return ok && state == "运行中"
}

// systemDir 返回虚拟机的系统目录
func (b *BaseDriver) systemDir(vmUUID string) string {
	return filepath.Join(b.Config.SystemPath, vmUUID)
}

// CreateBackup 通用备份
func (b *BaseDriver) CreateBackup(vmUUID string, tips string) (*model.ZMessage, error) {
	if strings.TrimSpace(b.Config.SystemPath) == "" {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: "主机未配置系统路径"}, nil
	}
	backupDir := strings.TrimSpace(b.Config.BackupPath)
	if backupDir == "" {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: "主机未配置备份路径"}, nil
	}

	srcDir := b.systemDir(vmUUID)
	if _, err := os.Stat(srcDir); err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: "虚拟机目录不存在: " + srcDir}, nil
	}
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: "创建备份目录失败: " + err.Error()}, nil
	}

	bakName := vmUUID + "-" + time.Now().Format("20060102150405")
	dstBase := filepath.Join(backupDir, bakName)

	// 记录原电源状态：还原时只恢复到原来的状态，避免把关机中的虚拟机开机
	wasRunning := b.isVMRunning(vmUUID)
	if !b.powerForBackup(vmUUID, false) {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: "关闭虚拟机失败，已中止备份"}, nil
	}
	archivePath, err := archiveDir(srcDir, dstBase)
	if wasRunning {
		b.powerForBackup(vmUUID, true)
	}

	if err != nil {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: "备份失败: " + err.Error()}, nil
	}

	bakFile := filepath.Base(archivePath)
	b.mu.Lock()
	if cfg, ok := b.VmSaving[vmUUID]; ok {
		cfg.Backups = append(cfg.Backups, model.VMBackup{
			BackupName: bakFile, BackupTime: time.Now(), BackupTips: tips,
		})
	}
	b.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMBackup",
		Message: "备份成功: " + bakFile,
		Results: map[string]interface{}{"backup_name": bakFile}}, nil
}

// RestoreBackup 通用还原
//
// 安全不变量（原实现均不满足，会导致数据丢失或跨虚拟机覆盖）：
//  1. 备份必须归属于目标虚拟机（按 <vmUUID>- 前缀绑定）；
//  2. 先解压到隔离暂存目录并校验结构，成功后才替换目标目录；
//  3. 只有确认关机成功才执行破坏性操作；
//  4. 还原结束后恢复原有电源状态，而不是无条件开机。
func (b *BaseDriver) RestoreBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	if err := safeIdent(vmUUID, 128); err != nil {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "虚拟机标识无效: " + err.Error()}, nil
	}
	if strings.TrimSpace(b.Config.SystemPath) == "" {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "主机未配置系统路径"}, nil
	}
	if strings.TrimSpace(b.Config.BackupPath) == "" {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "主机未配置备份路径"}, nil
	}

	name, err := sanitizeFileName(backupName)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "备份名称无效: " + err.Error()}, nil
	}
	// 备份由 CreateBackup 以「<vmUUID>-<时间戳>」命名，
	// 前缀校验可防止用其它虚拟机的备份覆盖目标目录。
	if !strings.HasPrefix(name, vmUUID+"-") {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "备份文件与目标虚拟机不匹配: " + name}, nil
	}
	backupPath := filepath.Join(b.Config.BackupPath, name)
	if _, err := os.Stat(backupPath); err != nil {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "备份文件不存在: " + name}, nil
	}

	srcDir := b.systemDir(vmUUID)

	wasRunning := b.isVMRunning(vmUUID)
	if !b.powerForBackup(vmUUID, false) {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "关闭虚拟机失败，已中止还原（原数据未改动）"}, nil
	}
	restorePower := func() {
		if wasRunning {
			b.powerForBackup(vmUUID, true)
		}
	}

	// 1) 解压到隔离暂存目录，此阶段不触碰原数据
	tmpRoot := filepath.Join(b.Config.SystemPath,
		fmt.Sprintf(".restore-%s-%d", vmUUID, time.Now().UnixNano()))
	if err := os.MkdirAll(tmpRoot, 0755); err != nil {
		restorePower()
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "创建暂存目录失败: " + err.Error()}, nil
	}
	defer os.RemoveAll(tmpRoot)

	if err := extractArchive(backupPath, tmpRoot); err != nil {
		restorePower()
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "解压备份失败（原数据未改动）: " + err.Error()}, nil
	}

	// 2) 校验结构：归档必须包含唯一的虚拟机根目录
	entries, err := os.ReadDir(tmpRoot)
	if err != nil || len(entries) != 1 || !entries[0].IsDir() {
		restorePower()
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "备份结构异常：期望唯一的虚拟机根目录（原数据未改动）"}, nil
	}
	extractedRoot := filepath.Join(tmpRoot, entries[0].Name())

	// 3) 原子替换：先移走旧目录，再移入新目录，失败则回滚
	oldDir := fmt.Sprintf("%s.old-%d", srcDir, time.Now().UnixNano())
	movedOld := false
	if _, statErr := os.Stat(srcDir); statErr == nil {
		if err := os.Rename(srcDir, oldDir); err != nil {
			restorePower()
			return &model.ZMessage{Success: false, Action: "Restores",
				Message: "备份原目录失败（原数据未改动）: " + err.Error()}, nil
		}
		movedOld = true
	}

	if err := os.Rename(extractedRoot, srcDir); err != nil {
		if movedOld {
			_ = os.Rename(oldDir, srcDir) // 回滚
		}
		restorePower()
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "应用备份失败，已回滚原数据: " + err.Error()}, nil
	}

	// 替换成功后才清理旧目录
	if movedOld {
		_ = os.RemoveAll(oldDir)
	}
	restorePower()

	return &model.ZMessage{Success: true, Action: "Restores",
		Message: "还原成功: " + vmUUID}, nil
}

// DeleteBackup 通用删除备份
func (b *BaseDriver) DeleteBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	// 只允许删除归属于该虚拟机的备份文件，避免删除备份目录外的文件
	name, nameErr := sanitizeFileName(backupName)
	if nameErr != nil || (vmUUID != "" && !strings.HasPrefix(name, vmUUID+"-")) {
		return &model.ZMessage{Success: false, Action: "RMBackup",
			Message: "备份名称无效或与目标虚拟机不匹配"}, nil
	}
	path := filepath.Join(b.Config.BackupPath, name)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return &model.ZMessage{Success: false, Action: "RMBackup",
			Message: "删除备份文件失败: " + err.Error()}, nil
	}

	b.mu.Lock()
	if cfg, ok := b.VmSaving[vmUUID]; ok {
		kept := make([]model.VMBackup, 0, len(cfg.Backups))
		for _, bk := range cfg.Backups {
			if bk.BackupName != backupName {
				kept = append(kept, bk)
			}
		}
		cfg.Backups = kept
	}
	b.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "RMBackup", Message: "备份删除成功"}, nil
}

// ScanBackups 通用扫描：按备份目录中的归档文件重建各虚拟机的备份列表
func (b *BaseDriver) ScanBackups() (*model.ZMessage, error) {
	dir := strings.TrimSpace(b.Config.BackupPath)
	if dir == "" {
		return &model.ZMessage{Success: true, Action: "LDBackup",
			Message: "主机未配置备份路径"}, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "LDBackup",
			Message: "读取备份目录失败: " + err.Error()}, nil
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	// 清空现有列表后按文件重建
	for _, cfg := range b.VmSaving {
		cfg.Backups = []model.VMBackup{}
	}

	count := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isArchiveName(name) {
			continue
		}
		for vmUUID, cfg := range b.VmSaving {
			if !strings.HasPrefix(name, vmUUID+"-") {
				continue
			}
			bt := time.Now()
			if info, statErr := e.Info(); statErr == nil {
				bt = info.ModTime()
			}
			cfg.Backups = append(cfg.Backups, model.VMBackup{
				BackupName: name, BackupTime: bt,
			})
			count++
			break
		}
	}

	return &model.ZMessage{Success: true, Action: "LDBackup",
		Message: fmt.Sprintf("扫描完成，共发现 %d 个备份", count)}, nil
}
