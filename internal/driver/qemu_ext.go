// Package driver QEMU/KVM 驱动扩展实现。
//
// 补齐 Python 版 HostServer/QEMUService.py 中存在、而 Go 版此前缺失的能力：
//   - 数据盘删除（RMMounts）与移交（HDDTrans）—— 原为基类假成功
//   - PCI / GPU / USB 设备枚举与直通（PCIShows / USBShows / PCISetup / USBSetup）
//   - USB 存储挂载（USBSetup 的存储型设备）
//
// 全部通过 virsh 命令行完成，与 Python 版实现方式一致。
package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// ============================================================
// 设备 XML 解析辅助
// ============================================================

// xmlAttrOf 从 XML 文本中提取指定标签下某个属性的值。
//
// 仅做轻量扫描，不引入 XML 依赖：libvirt 的 nodedev-dumpxml 输出结构固定，
// 形如 <vendor id='0x8086'/>，取标签名后第一个 attr='value' 即可。
func xmlAttrOf(xml, tag, attr string) string {
	idx := strings.Index(xml, "<"+tag)
	if idx < 0 {
		return ""
	}
	seg := xml[idx:]
	if end := strings.Index(seg, ">"); end >= 0 {
		seg = seg[:end]
	}
	key := attr + "='"
	aidx := strings.Index(seg, key)
	if aidx < 0 {
		return ""
	}
	seg = seg[aidx+len(key):]
	if end := strings.Index(seg, "'"); end >= 0 {
		return seg[:end]
	}
	return ""
}

// xmlInnerOf 提取 <tag>...</tag> 之间的文本内容
func xmlInnerOf(xml, tag string) string {
	open := "<" + tag + ">"
	idx := strings.Index(xml, open)
	if idx < 0 {
		return ""
	}
	seg := xml[idx+len(open):]
	if end := strings.Index(seg, "</"+tag+">"); end >= 0 {
		return strings.TrimSpace(seg[:end])
	}
	return ""
}

// parsePCIAddress 解析 "0000:00:02.0" 形式的 PCI 地址
func parsePCIAddress(addr string) (domain, bus, slot, fn int, ok bool) {
	parts := strings.Split(strings.TrimSpace(addr), ":")
	if len(parts) != 3 {
		return 0, 0, 0, 0, false
	}
	dv, err := strconv.ParseInt(parts[0], 16, 32)
	if err != nil {
		return 0, 0, 0, 0, false
	}
	bv, err := strconv.ParseInt(parts[1], 16, 32)
	if err != nil {
		return 0, 0, 0, 0, false
	}
	rest := strings.Split(parts[2], ".")
	if len(rest) != 2 {
		return 0, 0, 0, 0, false
	}
	sv, err := strconv.ParseInt(rest[0], 16, 32)
	if err != nil {
		return 0, 0, 0, 0, false
	}
	fv, err := strconv.ParseInt(rest[1], 16, 32)
	if err != nil {
		return 0, 0, 0, 0, false
	}
	return int(dv), int(bv), int(sv), int(fv), true
}

// pciNodedevToAddress 把 nodedev 名称 pci_0000_00_02_0 转成 0000:00:02.0
func pciNodedevToAddress(name string) string {
	name = strings.TrimPrefix(name, "pci_")
	parts := strings.Split(name, "_")
	if len(parts) != 4 {
		return ""
	}
	return fmt.Sprintf("%s:%s:%s.%s", parts[0], parts[1], parts[2], parts[3])
}

// ============================================================
// 数据盘
// ============================================================

// qemuDiskTargets 解析 `virsh domblklist`，返回 target -> source 映射
func (d *QEMUDriver) qemuDiskTargets(vmUUID string) map[string]string {
	result := make(map[string]string)
	out, err := d.execVirsh("domblklist", vmUUID)
	if err != nil {
		return result
	}
	for _, line := range splitLines(out) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		target, source := fields[0], fields[1]
		if target == "Target" || strings.HasPrefix(source, "---") {
			continue
		}
		if source == "-" {
			continue
		}
		result[target] = source
	}
	return result
}

// locateHDD 定位数据盘在平台上的 target 与真实路径
func (d *QEMUDriver) locateHDD(vmUUID string, hdd *model.SDConfig) (target string, path string) {
	targets := d.qemuDiskTargets(vmUUID)

	// 1) 按记录路径匹配（最可靠）
	if hdd != nil && hdd.HddPath != "" {
		for t, src := range targets {
			if src == hdd.HddPath {
				return t, src
			}
		}
	}
	// 2) 按磁盘名匹配文件名
	if hdd != nil && hdd.HddName != "" {
		for t, src := range targets {
			if strings.Contains(filepath.Base(src), hdd.HddName) {
				return t, src
			}
		}
	}
	// 3) 回退到约定路径
	dir := d.Config.ExternPath
	if dir == "" {
		dir = d.diskDir()
	}
	name := ""
	if hdd != nil {
		name = hdd.HddName
	}
	if name == "" {
		return "", ""
	}
	return "", filepath.Join(dir, name+".qcow2")
}

// nextFreeVdLetter 返回下一个可用的 vdX 目标名（vdb 起，跳过已占用）
func (d *QEMUDriver) nextFreeVdLetter(vmUUID string) string {
	targets := d.qemuDiskTargets(vmUUID)
	for _, c := range "bcdefghij" {
		name := string([]rune{'v', 'd', c})
		if _, used := targets[name]; !used {
			return name
		}
	}
	return "vdb"
}

// DeleteHDD 删除数据盘：先卸载，再删除平台上的真实卷文件。
//
// 与 Python 版 QEMUService.RMMounts 对齐：
//   virsh detach-disk <vm> <target> --persistent
//   删除底层 qcow2 文件
//
// 这是破坏性操作，任一步失败都必须如实返回，不能只改内存。
func (d *QEMUDriver) DeleteHDD(vmUUID string, hddName string) (*model.ZMessage, error) {
	if strings.TrimSpace(hddName) == "" {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: "磁盘名称不能为空"}, nil
	}

	d.mu.RLock()
	cfg, ok := d.VmSaving[vmUUID]
	var hdd model.SDConfig
	if ok {
		hdd = cfg.HddAll[hddName]
	}
	d.mu.RUnlock()
	if !ok {
		return &model.ZMessage{Success: false, Action: "RMMounts", Message: "虚拟机不存在"}, nil
	}
	if hdd.HddName == "" {
		hdd.HddName = hddName
	}

	target, path := d.locateHDD(vmUUID, &hdd)

	if target != "" {
		if out, err := d.execVirsh("detach-disk", vmUUID, target, "--persistent"); err != nil {
			return &model.ZMessage{Success: false, Action: "RMMounts",
				Message: fmt.Sprintf("卸载数据盘失败: %v (%s)", err, strings.TrimSpace(out))}, nil
		}
	}

	// 删除真实卷文件（跳过网络/块设备路径）
	if path != "" && !strings.Contains(path, "://") && !strings.HasPrefix(path, "/dev/") {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return &model.ZMessage{Success: false, Action: "RMMounts",
				Message: "删除卷文件失败: " + err.Error()}, nil
		}
	}

	d.mu.Lock()
	delete(cfg.HddAll, hddName)
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "RMMounts", Message: "数据盘删除成功"}, nil
}

// TransferHDD 移交数据盘：从源虚拟机卸载，挂载到目标虚拟机。
//
// 与 Python 版 QEMUService.HDDTrans 对齐。挂载失败时回滚到源虚拟机，
// 避免出现「两边都没挂上」的中间态导致磁盘丢失。
func (d *QEMUDriver) TransferHDD(vmUUID string, hdd *model.SDConfig, targetVM string) (*model.ZMessage, error) {
	if hdd == nil || strings.TrimSpace(hdd.HddName) == "" {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "磁盘信息不完整"}, nil
	}
	if vmUUID == targetVM {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "源与目标虚拟机相同"}, nil
	}

	d.mu.RLock()
	srcCfg, srcOK := d.VmSaving[vmUUID]
	dstCfg, dstOK := d.VmSaving[targetVM]
	d.mu.RUnlock()
	if !srcOK {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "原始虚拟机不存在"}, nil
	}
	if !dstOK {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "目标虚拟机不存在"}, nil
	}

	target, path := d.locateHDD(vmUUID, hdd)
	if path == "" {
		return &model.ZMessage{Success: false, Action: "HDDTrans", Message: "无法定位磁盘路径"}, nil
	}

	// 1) 从源卸载
	if target != "" {
		if out, err := d.execVirsh("detach-disk", vmUUID, target, "--persistent"); err != nil {
			return &model.ZMessage{Success: false, Action: "HDDTrans",
				Message: fmt.Sprintf("源虚拟机卸载失败: %v (%s)", err, strings.TrimSpace(out))}, nil
		}
	}

	// 2) 挂到目标
	dstDev := d.nextFreeVdLetter(targetVM)
	if out, err := d.execVirsh("attach-disk", targetVM, path, dstDev,
		"--type", "disk", "--persistent"); err != nil {
		// 回滚：重新挂回源虚拟机
		rollbackDev := d.nextFreeVdLetter(vmUUID)
		_, _ = d.execVirsh("attach-disk", vmUUID, path, rollbackDev, "--type", "disk", "--persistent")
		return &model.ZMessage{Success: false, Action: "HDDTrans",
			Message: fmt.Sprintf("目标虚拟机挂载失败（已回滚）: %v (%s)", err, strings.TrimSpace(out))}, nil
	}

	// 3) 双方内存记录同步
	d.mu.Lock()
	delete(srcCfg.HddAll, hdd.HddName)
	if hdd.HddFlag == 0 {
		hdd.HddFlag = 1
	}
	hdd.HddPath = path
	if dstCfg.HddAll == nil {
		dstCfg.HddAll = make(map[string]model.SDConfig)
	}
	dstCfg.HddAll[hdd.HddName] = *hdd
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "HDDTrans", Message: "数据盘移交成功"}, nil
}

// ============================================================
// 设备直通枚举
// ============================================================

// GetPCIList 枚举宿主机可直通 PCI 设备（virsh nodedev-list --cap pci）
func (d *QEMUDriver) GetPCIList() (map[string]*model.VFConfig, error) {
	result := make(map[string]*model.VFConfig)
	out, err := d.execVirsh("nodedev-list", "--cap", "pci")
	if err != nil {
		return result, nil
	}
	for _, name := range splitLines(out) {
		xml, err := d.execVirsh("nodedev-dumpxml", name)
		if err != nil {
			continue
		}
		addr := pciNodedevToAddress(name)
		if addr == "" {
			continue
		}
		vendor := strings.TrimPrefix(xmlAttrOf(xml, "vendor", "id"), "0x")
		product := strings.TrimPrefix(xmlAttrOf(xml, "product", "id"), "0x")
		hint := strings.TrimSpace(vendor + ":" + product)
		if hint == ":" {
			hint = addr
		}
		result[name] = &model.VFConfig{
			PciKey:  name,
			GpuUUID: addr,
			GpuHint: hint,
		}
	}
	return result, nil
}

// GetGPUList 枚举宿主机 GPU（PCI 设备中筛选 VGA/3D 控制器）
func (d *QEMUDriver) GetGPUList() (map[string]*model.VFConfig, error) {
	all, err := d.GetPCIList()
	if err != nil {
		return all, err
	}
	result := make(map[string]*model.VFConfig)
	for key, cfg := range all {
		xml, err := d.execVirsh("nodedev-dumpxml", key)
		if err != nil {
			continue
		}
		cls := xmlAttrOf(xml, "class", "id")
		// 0x0300xx = Display controller（VGA / 3D / other display）
		if strings.HasPrefix(strings.TrimPrefix(cls, "0x"), "03") {
			result[key] = cfg
		}
	}
	return result, nil
}

// GetUSBList 枚举宿主机 USB 设备（virsh nodedev-list --cap usb）
func (d *QEMUDriver) GetUSBList() (map[string]*model.USBInfos, error) {
	result := make(map[string]*model.USBInfos)
	out, err := d.execVirsh("nodedev-list", "--cap", "usb")
	if err != nil {
		return result, nil
	}
	for _, name := range splitLines(out) {
		xml, err := d.execVirsh("nodedev-dumpxml", name)
		if err != nil {
			continue
		}
		vendor := strings.TrimPrefix(xmlAttrOf(xml, "vendor", "id"), "0x")
		product := strings.TrimPrefix(xmlAttrOf(xml, "product", "id"), "0x")
		// USB 根集线器没有 vendor/product，跳过
		if vendor == "" && product == "" {
			continue
		}
		result[name] = &model.USBInfos{
			UsbKey:  name,
			VidUUID: strings.ToLower(vendor),
			PidUUID: strings.ToLower(product),
			UsbHint: strings.TrimSpace(vendor + ":" + product),
		}
	}
	return result, nil
}

// ============================================================
// PCI 直通
// ============================================================

// SetupPCI PCI/GPU 直通（hostdev 挂载，需关机）
func (d *QEMUDriver) SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error) {
	if cfg == nil {
		return &model.ZMessage{Success: false, Action: "PCISetup", Message: "PCI设备信息为空"}, nil
	}
	if d.GetPower(vmUUID) == "运行中" {
		return &model.ZMessage{Success: false, Action: "PCISetup", Message: "PCI直通需要先关闭虚拟机"}, nil
	}

	addr := cfg.GpuUUID
	if addr == "" {
		addr = pciNodedevToAddress(pciKey)
	}
	dm, bus, slot, fn, ok := parsePCIAddress(addr)
	if !ok {
		return &model.ZMessage{Success: false, Action: "PCISetup",
			Message: "无法解析PCI地址: " + addr}, nil
	}

	hostdevXML := fmt.Sprintf(`<hostdev mode='subsystem' type='pci' managed='yes'>
  <source>
    <address domain='0x%04x' bus='0x%02x' slot='0x%02x' function='0x%x'/>
  </source>
</hostdev>`, dm, bus, slot, fn)

	if err := d.applyDeviceXML(vmUUID, "pci", hostdevXML, add); err != nil {
		return &model.ZMessage{Success: false, Action: "PCISetup", Message: err.Error()}, nil
	}

	d.mu.Lock()
	if vmCfg, exists := d.VmSaving[vmUUID]; exists {
		if vmCfg.PciAll == nil {
			vmCfg.PciAll = make(map[string]model.VFConfig)
		}
		if add {
			vmCfg.PciAll[pciKey] = *cfg
		} else {
			delete(vmCfg.PciAll, pciKey)
		}
	}
	d.mu.Unlock()

	action := "添加"
	if !add {
		action = "移除"
	}
	return &model.ZMessage{Success: true, Action: "PCISetup", Message: "PCI设备" + action + "成功"}, nil
}

// applyDeviceXML 把设备 XML 写到临时文件后 attach/detach
func (d *QEMUDriver) applyDeviceXML(vmUUID, kind, xml string, add bool) error {
	xmlPath := filepath.Join(os.TempDir(),
		fmt.Sprintf("hostagent-%s-%s-%d.xml", kind, vmUUID, time.Now().UnixNano()))
	if err := os.WriteFile(xmlPath, []byte(xml), 0644); err != nil {
		return fmt.Errorf("写入设备XML失败: %w", err)
	}
	defer os.Remove(xmlPath)

	action := "attach-device"
	if !add {
		action = "detach-device"
	}
	out, err := d.execVirsh(action, vmUUID, xmlPath, "--persistent")
	if err != nil {
		return fmt.Errorf("设备操作失败: %v (%s)", err, strings.TrimSpace(out))
	}
	return nil
}

// ============================================================
// USB 存储挂载
// ============================================================

// MountUSB 挂载 / 卸载 USB 存储设备（U 盘等块设备）
//
// 与 Python 版 QEMUService 一致：以 hostdev usb 形式挂载按 VID/PID 匹配的设备。
func (d *QEMUDriver) MountUSB(vmUUID string, usbKey string, mount bool) (*model.ZMessage, error) {
	if strings.TrimSpace(usbKey) == "" {
		return &model.ZMessage{Success: false, Action: "USBMount", Message: "USB设备标识不能为空"}, nil
	}

	d.mu.RLock()
	var info model.USBInfos
	if cfg, ok := d.VmSaving[vmUUID]; ok {
		info = cfg.UsbAll[usbKey]
	}
	d.mu.RUnlock()

	vid := strings.TrimPrefix(info.VidUUID, "0x")
	pid := strings.TrimPrefix(info.PidUUID, "0x")
	if vid == "" || pid == "" {
		return &model.ZMessage{Success: false, Action: "USBMount",
			Message: "USB设备缺少 VID/PID，无法挂载"}, nil
	}

	usbXML := fmt.Sprintf(`<hostdev mode='subsystem' type='usb' managed='yes'>
  <source>
    <vendor id='0x%s'/>
    <product id='0x%s'/>
  </source>
</hostdev>`, vid, pid)

	if err := d.applyDeviceXML(vmUUID, "usb", usbXML, mount); err != nil {
		return &model.ZMessage{Success: false, Action: "USBMount", Message: err.Error()}, nil
	}

	action := "挂载"
	if !mount {
		action = "卸载"
	}
	return &model.ZMessage{Success: true, Action: "USBMount", Message: "USB存储" + action + "成功"}, nil
}
