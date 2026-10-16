// Package driver Proxmox VE 驱动扩展实现。
//
// 补齐 Python 版 HostServer/ProxmoxQemu.py 中存在、而 Go 版此前缺失/降级的能力：
//   - 数据盘删除（RMMounts）：卸载 → 移除 unused 引用 → 删除存储卷
//   - 数据盘移交（HDDTrans）：qm move-disk 到目标虚拟机
//   - PCI 设备枚举（lspci -nn）与 USB 设备枚举（lsusb）
//   - 备份还原（Restores）：POST /nodes/{node}/qemu 触发恢复任务并等待
package driver

import (
	"context"
	"fmt"
	"strings"
	"time"

	proxmox "github.com/luthermonson/go-proxmox"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// ============================================================
// 磁盘位工具
// ============================================================

// pveDiskBusKeys 返回 PVE 所有磁盘总线位（含 unused）
func pveDiskBusKeys() []string {
	buses := []string{"scsi", "virtio", "sata", "ide"}
	keys := make([]string, 0, 64)
	for _, bus := range buses {
		for i := 0; i <= 30; i++ {
			keys = append(keys, fmt.Sprintf("%s%d", bus, i))
		}
	}
	unused := make([]string, 0, 32)
	for i := 0; i <= 30; i++ {
		unused = append(unused, fmt.Sprintf("unused%d", i))
	}
	// unused 优先，便于卸载后的定位
	return append(unused, keys...)
}

// pveUnusedKeys 返回 unused 位键
func pveUnusedKeys() []string {
	out := make([]string, 0, 32)
	for i := 0; i <= 30; i++ {
		out = append(out, fmt.Sprintf("unused%d", i))
	}
	return out
}

// pveVMConfigMap 读取虚拟机配置（go-proxmox 会自动展开 data 字段）
func (d *ProxmoxDriver) pveVMConfigMap(vmid int64) (map[string]string, error) {
	client, err := d.apiConn()
	if err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", d.nodeName(), vmid)
	if err := client.Get(context.Background(), path, &raw); err != nil {
		return nil, fmt.Errorf("读取虚拟机配置失败: %w", err)
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out, nil
}

// pveFindDiskKey 在配置中定位与数据盘匹配的设备位，返回 (键, 完整值)
//
// 匹配顺序：显式记录的设备位 → 卷标识（HddPath）→ 磁盘名。
// 优先显式设备位可避免同名磁盘互相误判。
func pveFindDiskKey(cfg map[string]string, hdd *model.SDConfig) (string, string) {
	if hdd == nil || len(cfg) == 0 {
		return "", ""
	}
	if hdd.HddScsi != "" {
		if v, ok := cfg[hdd.HddScsi]; ok && v != "" {
			return hdd.HddScsi, v
		}
	}
	if hdd.HddPath != "" {
		for _, k := range pveDiskBusKeys() {
			v, ok := cfg[k]
			if !ok || v == "" {
				continue
			}
			if strings.Contains(v, hdd.HddPath) {
				return k, v
			}
		}
	}
	if hdd.HddName != "" {
		for _, k := range pveDiskBusKeys() {
			v, ok := cfg[k]
			if !ok || v == "" {
				continue
			}
			if strings.Contains(v, hdd.HddName) {
				return k, v
			}
		}
	}
	return "", ""
}

// pveVolumeOf 从磁盘配置值中取出卷标识（"local-lvm:vm-100-disk-1"）
func pveVolumeOf(value string) string {
	if idx := strings.Index(value, ","); idx >= 0 {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
}

// ============================================================
// 数据盘删除
// ============================================================

// DeleteHDD 删除数据盘（与 Python 版 ProxmoxQemu.RMMounts 一致）。
//
// 流程：定位设备位 → 关闭虚拟机 → 卸载（变 unused）→ 移除 unused 引用 →
// 删除存储卷 → 恢复原电源状态。任一步失败都如实返回，不再假成功。
func (d *ProxmoxDriver) DeleteHDD(vmUUID string, hddName string) (*model.ZMessage, error) {
	if strings.TrimSpace(hddName) == "" {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: "磁盘名称不能为空"}, nil
	}

	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: err.Error()}, nil
	}
	vmid, err := d.getVMID(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: err.Error()}, nil
	}

	d.mu.RLock()
	var hdd model.SDConfig
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		hdd = cfg.HddAll[hddName]
	}
	d.mu.RUnlock()
	if hdd.HddName == "" {
		hdd.HddName = hddName
	}

	cfgMap, err := d.pveVMConfigMap(vmid)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: err.Error()}, nil
	}
	key, value := pveFindDiskKey(cfgMap, &hdd)
	if key == "" {
		return &model.ZMessage{Success: false, Action: "RMMounts",
			Message: "未找到数据盘对应的设备，已拒绝删除以避免误删系统盘"}, nil
	}
	volid := pveVolumeOf(value)

	ctx := context.Background()
	wasRunning := vm.IsRunning()
	if wasRunning {
		if task, err := vm.Stop(ctx); err == nil && task != nil {
			_ = task.WaitFor(ctx, 120)
		}
		time.Sleep(2 * time.Second)
	}

	// 1) 卸载设备（若非 unused 状态）
	if !strings.HasPrefix(key, "unused") {
		task, err := vm.Config(ctx, proxmox.VirtualMachineOption{Name: "delete", Value: key})
		if err != nil {
			if wasRunning {
				_, _ = vm.Start(ctx)
			}
			return &model.ZMessage{Success: false, Action: "RMMounts",
				Message: "卸载数据盘失败: " + err.Error()}, nil
		}
		if task != nil {
			_ = task.WaitFor(ctx, 120)
		}
		time.Sleep(1 * time.Second)
	}

	// 2) 移除 unused 引用，使 PVE 彻底释放该卷
	cfgMap2, _ := d.pveVMConfigMap(vmid)
	unusedKey := ""
	for _, k := range pveUnusedKeys() {
		if v, ok := cfgMap2[k]; ok && v != "" && (volid == "" || strings.Contains(v, volid)) {
			unusedKey = k
			break
		}
	}
	if unusedKey == "" {
		for _, k := range pveUnusedKeys() {
			if v, ok := cfgMap2[k]; ok && v != "" {
				unusedKey = k
				break
			}
		}
	}
	if unusedKey != "" {
		if task, err := vm.Config(ctx, proxmox.VirtualMachineOption{Name: "delete", Value: unusedKey}); err == nil && task != nil {
			_ = task.WaitFor(ctx, 120)
		}
	}

	// 3) 删除存储卷（真实释放磁盘空间）
	if volid != "" {
		if idx := strings.Index(volid, ":"); idx > 0 {
			storageName := volid[:idx]
			if node, err := d.getNode(); err == nil {
				if st, err := node.Storage(ctx, storageName); err == nil {
					if task, err := st.DeleteContent(ctx, volid); err != nil {
						if wasRunning {
							_, _ = vm.Start(ctx)
						}
						return &model.ZMessage{Success: false, Action: "RMMounts",
							Message: "删除存储卷失败: " + err.Error()}, nil
					} else if task != nil {
						_ = task.WaitFor(ctx, 600)
					}
				}
			}
		}
	}

	if wasRunning {
		if task, err := vm.Start(ctx); err == nil && task != nil {
			_ = task.WaitFor(ctx, 120)
		}
	}

	d.mu.Lock()
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		delete(cfg.HddAll, hddName)
	}
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "RMMounts", Message: "数据盘删除成功"}, nil
}

// ============================================================
// 数据盘移交
// ============================================================

// TransferHDD 移交数据盘（与 Python 版 ProxmoxQemu.HDDTrans 一致）：
// 通过 `qm move-disk <src> <disk> --target-vmid <dst> --target-disk <scsiN>` 迁移，
// 迁移后在目标上卸载使其处于 unused 状态，等待后续挂载。
func (d *ProxmoxDriver) TransferHDD(vmUUID string, hdd *model.SDConfig, targetVM string) (*model.ZMessage, error) {
	if hdd == nil || strings.TrimSpace(hdd.HddName) == "" {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "磁盘信息不完整"}, nil
	}
	if vmUUID == targetVM {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "源与目标虚拟机相同"}, nil
	}

	srcVmid, err := d.getVMID(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "获取源虚拟机失败: " + err.Error()}, nil
	}
	dstVmid, err := d.getVMID(targetVM)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "获取目标虚拟机失败: " + err.Error()}, nil
	}

	srcVM, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: err.Error()}, nil
	}
	dstVM, err := d.getVM(targetVM)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: err.Error()}, nil
	}

	ctx := context.Background()

	// 移交前两侧都必须关机
	srcWasRunning := srcVM.IsRunning()
	dstWasRunning := dstVM.IsRunning()
	if srcWasRunning {
		if task, err := srcVM.Stop(ctx); err == nil && task != nil {
			_ = task.WaitFor(ctx, 120)
		}
	}
	if dstWasRunning {
		if task, err := dstVM.Stop(ctx); err == nil && task != nil {
			_ = task.WaitFor(ctx, 120)
		}
	}
	time.Sleep(2 * time.Second)

	restorePower := func() {
		if srcWasRunning {
			if task, err := srcVM.Start(ctx); err == nil && task != nil {
				_ = task.WaitFor(ctx, 120)
			}
		}
		if dstWasRunning {
			if task, err := dstVM.Start(ctx); err == nil && task != nil {
				_ = task.WaitFor(ctx, 120)
			}
		}
	}

	// 1) 定位源磁盘（优先 unused，其次已挂载总线位）
	cfgMap, err := d.pveVMConfigMap(srcVmid)
	if err != nil {
		restorePower()
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: err.Error()}, nil
	}
	key, value := pveFindDiskKey(cfgMap, hdd)
	if key == "" {
		restorePower()
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "未找到待移交的磁盘设备"}, nil
	}

	// 若仍挂载则先卸载为 unused
	if !strings.HasPrefix(key, "unused") {
		if task, err := srcVM.Config(ctx, proxmox.VirtualMachineOption{Name: "delete", Value: key}); err == nil && task != nil {
			_ = task.WaitFor(ctx, 120)
		}
		time.Sleep(1 * time.Second)
		cfgMap, _ = d.pveVMConfigMap(srcVmid)
		if newKey, newVal := pveFindDiskKey(cfgMap, hdd); newKey != "" {
			key, value = newKey, newVal
		}
	}
	_ = value

	// 2) 找到目标虚拟机空闲的 scsi 位
	dstCfg, _ := d.pveVMConfigMap(dstVmid)
	scsiNum := 1
	for {
		if _, used := dstCfg[fmt.Sprintf("scsi%d", scsiNum)]; !used || scsiNum > 30 {
			break
		}
		scsiNum++
	}
	targetDisk := fmt.Sprintf("scsi%d", scsiNum)

	// 3) qm move-disk（PVE 官方跨虚拟机迁移方式）
	cmd := fmt.Sprintf("qm move-disk %d %s --target-vmid %d --target-disk %s",
		srcVmid, key, dstVmid, targetDisk)
	res, err := d.runOnNode(cmd, 30*time.Minute)
	if err != nil || res == nil || res.ExitCode != 0 {
		restorePower()
		detail := ""
		if res != nil {
			detail = strings.TrimSpace(res.Stderr)
			if detail == "" {
				detail = strings.TrimSpace(res.Stdout)
			}
		}
		if err != nil {
			detail = err.Error()
		}
		return &model.ZMessage{Success: false, Action: "HDDTrans",
			Message: "移动磁盘失败: " + detail}, nil
	}

	// 4) 目标上立即卸载，使其保持 unused（与 Python 行为一致）
	if task, err := dstVM.Config(ctx, proxmox.VirtualMachineOption{Name: "delete", Value: targetDisk}); err == nil && task != nil {
		_ = task.WaitFor(ctx, 120)
	}

	// 5) 读取目标上的新卷标识
	newVal := ""
	if dstCfgNew, err := d.pveVMConfigMap(dstVmid); err == nil {
		for _, k := range pveUnusedKeys() {
			if v, ok := dstCfgNew[k]; ok && v != "" {
				newVal = pveVolumeOf(v)
				break
			}
		}
	}

	// 6) 同步双方内存记录
	d.mu.Lock()
	if src, ok := d.VmSaving[vmUUID]; ok {
		delete(src.HddAll, hdd.HddName)
	}
	if dst, ok := d.VmSaving[targetVM]; ok {
		if dst.HddAll == nil {
			dst.HddAll = make(map[string]model.SDConfig)
		}
		moved := *hdd
		moved.HddFlag = 0
		moved.HddScsi = ""
		moved.HddPath = newVal
		dst.HddAll[hdd.HddName] = moved
	}
	d.mu.Unlock()

	restorePower()

	return &model.ZMessage{Success: true, Action: "HDDTrans",
		Message: "磁盘移交成功",
		Results: map[string]interface{}{
			"source_vmid": srcVmid,
			"target_vmid": dstVmid,
			"source_disk": key,
			"target_disk": targetDisk,
		}}, nil
}

// ============================================================
// 设备枚举（SSH lspci / lsusb，与 Python 版一致）
// ============================================================

// GetPCIList 通过 SSH 执行 lspci -nn 枚举 PCI 设备
func (d *ProxmoxDriver) GetPCIList() (map[string]*model.VFConfig, error) {
	result := make(map[string]*model.VFConfig)

	res, err := d.runOnNode("lspci -nn", 30*time.Second)
	if err != nil || res == nil || res.ExitCode != 0 {
		return result, nil
	}

	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) < 2 {
			continue
		}
		pciID := strings.TrimSpace(parts[0]) // 形如 01:00.0
		name := strings.TrimSpace(parts[1])
		if pciID == "" {
			continue
		}
		result[pciID] = &model.VFConfig{
			PciKey:  pciID,
			GpuUUID: pciID,
			GpuHint: name,
		}
	}
	return result, nil
}

// GetGPUList 枚举显卡（lspci 输出中筛选 VGA / 3D 控制器）
func (d *ProxmoxDriver) GetGPUList() (map[string]*model.VFConfig, error) {
	all, err := d.GetPCIList()
	if err != nil {
		return all, err
	}
	result := make(map[string]*model.VFConfig)
	for key, cfg := range all {
		lower := strings.ToLower(cfg.GpuHint)
		if strings.Contains(lower, "vga") || strings.Contains(lower, "3d controller") ||
			strings.Contains(lower, "display controller") {
			result[key] = cfg
		}
	}
	return result, nil
}

// GetUSBList 通过 SSH 执行 lsusb 枚举 USB 设备
func (d *ProxmoxDriver) GetUSBList() (map[string]*model.USBInfos, error) {
	result := make(map[string]*model.USBInfos)

	res, err := d.runOnNode("lsusb", 30*time.Second)
	if err != nil || res == nil || res.ExitCode != 0 {
		return result, nil
	}

	for _, line := range strings.Split(res.Stdout, "\n") {
		if !strings.Contains(line, "ID ") {
			continue
		}
		idPart := strings.SplitN(line, "ID ", 2)[1]
		fields := strings.SplitN(strings.TrimSpace(idPart), " ", 2)
		if len(fields) == 0 {
			continue
		}
		vidPid := fields[0]
		if !strings.Contains(vidPid, ":") {
			continue
		}
		hint := vidPid
		if len(fields) > 1 {
			hint = strings.TrimSpace(fields[1])
		}
		pair := strings.SplitN(vidPid, ":", 2)
		result[vidPid] = &model.USBInfos{
			UsbKey:  vidPid,
			VidUUID: pair[0],
			PidUUID: pair[1],
			UsbHint: hint,
		}
	}
	return result, nil
}

// ============================================================
// 备份还原
// ============================================================

// RestoreBackup 从备份恢复虚拟机（与 Python 版 ProxmoxQemu.Restores 一致）：
// 关机 → 解析备份卷 → POST /nodes/{node}/qemu（archive + force）→ 等待任务完成 → 恢复电源。
func (d *ProxmoxDriver) RestoreBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	if strings.TrimSpace(backupName) == "" {
		return &model.ZMessage{Success: false, Action: "Restores", Message: "备份名称不能为空"}, nil
	}

	vm, err := d.getVM(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "Restores", Message: err.Error()}, nil
	}
	vmid, err := d.getVMID(vmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "Restores", Message: err.Error()}, nil
	}

	ctx := context.Background()

	// 解析备份卷标识（优先精确匹配，其次模糊匹配，与 Python 一致）
	storageName := strings.TrimSpace(d.Config.BackupPath)
	if storageName == "" {
		storageName = "local"
	}
	archive := fmt.Sprintf("%s:backup/%s", storageName, backupName)
	if node, err := d.getNode(); err == nil {
		if st, err := node.Storage(ctx, storageName); err == nil {
			if contents, err := st.GetContent(ctx); err == nil {
				matched := ""
				for _, c := range contents {
					if c.Volid == archive || strings.HasSuffix(c.Volid, "/"+backupName) {
						matched = c.Volid
						break
					}
				}
				if matched == "" {
					for _, c := range contents {
						if strings.Contains(c.Volid, backupName) {
							matched = c.Volid
							break
						}
					}
				}
				if matched != "" {
					archive = matched
				}
			}
		}
	}

	wasRunning := vm.IsRunning()
	if wasRunning {
		if task, err := vm.Stop(ctx); err == nil && task != nil {
			_ = task.WaitFor(ctx, 180)
		}
		time.Sleep(3 * time.Second)
	}

	client, err := d.apiConn()
	if err != nil {
		if wasRunning {
			_, _ = vm.Start(ctx)
		}
		return &model.ZMessage{Success: false, Action: "Restores", Message: err.Error()}, nil
	}

	body := map[string]interface{}{
		"vmid":    vmid,
		"archive": archive,
		"force":   1,
	}
	var taskID string
	if err := client.Post(ctx, fmt.Sprintf("/nodes/%s/qemu", d.nodeName()), body, &taskID); err != nil {
		if wasRunning {
			_, _ = vm.Start(ctx)
		}
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "恢复任务创建失败: " + err.Error()}, nil
	}

	// 等待恢复任务结束（最多 1 小时，与 Python 一致）
	if err := d.waitPVETask(taskID, time.Hour); err != nil {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "恢复失败: " + err.Error()}, nil
	}

	if wasRunning {
		if task, err := vm.Start(ctx); err == nil && task != nil {
			_ = task.WaitFor(ctx, 180)
		}
	}

	return &model.ZMessage{Success: true, Action: "Restores", Message: "虚拟机恢复成功: " + vmUUID}, nil
}

// waitPVETask 轮询 PVE 任务直至 stopped
func (d *ProxmoxDriver) waitPVETask(taskID string, timeout time.Duration) error {
	if strings.TrimSpace(taskID) == "" {
		return nil
	}
	client, err := d.apiConn()
	if err != nil {
		return err
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var status struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		path := fmt.Sprintf("/nodes/%s/tasks/%s/status", d.nodeName(), taskID)
		if err := client.Get(context.Background(), path, &status); err == nil {
			if status.Status == "stopped" {
				if status.ExitStatus != "" && status.ExitStatus != "OK" {
					return fmt.Errorf("任务返回失败状态: %s", status.ExitStatus)
				}
				return nil
			}
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("任务执行超时（%s）", timeout)
}
