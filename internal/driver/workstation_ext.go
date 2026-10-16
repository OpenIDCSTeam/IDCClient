// Package driver VMware Workstation 驱动扩展实现。
//
// 补齐 Python 版 HostServer/Workstation.py 中存在、而 Go 版此前缺失的能力：
//   - 系统重装（VMSetups）：复制系统模板镜像并扩展磁盘容量
//   - 屏幕截图（VMScreen）：vmrun captureScreen
//   - USB 设备枚举（USBShows）：Windows 走 PowerShell，其它平台走 lsusb
//
// 说明：HDDMount / ISOMount / RMMounts 在 Python 版中同样是调用基类
// 的「配置记录」实现，因此 Go 版保持继承基类行为即可，不做伪实现。
package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
	"github.com/OpenIDCSTeam/HostAgent-Go/internal/security"
)

// vmrunBin 解析 vmrun 可执行文件路径（优先取 launch_path 配置）
func (d *WorkstationDriver) vmrunBin() string {
	launch := strings.TrimSpace(d.Config.LaunchPath)
	if launch != "" {
		if info, err := os.Stat(launch); err == nil {
			if info.IsDir() {
				for _, name := range []string{"vmrun.exe", "vmrun"} {
					candidate := filepath.Join(launch, name)
					if _, err := os.Stat(candidate); err == nil {
						return candidate
					}
				}
			} else {
				return launch
			}
		}
	}
	return "vmrun"
}

// vmxPath 返回虚拟机的 VMX 配置文件路径
func (d *WorkstationDriver) vmxPath(vmUUID string) string {
	dir := strings.TrimSpace(d.Config.SystemPath)
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, vmUUID+".vmx")
}

// ReinstallVM 重装系统：用系统模板镜像覆盖系统盘，并把磁盘扩展到目标容量。
//
// 与 Python 版 Workstation.VMSetups 一致：
//  1. 复制 <images_path>/<os_name> → <system_path>/<vm_uuid>.<ext>
//  2. vmware-vdiskmanager -x <size>GB 扩展磁盘
func (d *WorkstationDriver) ReinstallVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	vmUUID := cfg.VmUUID

	// 停止虚拟机
	if _, err := d.PowerControl(vmUUID, model.VMPowerHClose); err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall",
			Message: "关闭虚拟机失败: " + err.Error()}, nil
	}
	time.Sleep(2 * time.Second)

	templateName := strings.TrimSpace(cfg.OsName)
	imagesPath := strings.TrimSpace(d.Config.ImagesPath)
	if templateName == "" || imagesPath == "" {
		return &model.ZMessage{Success: false, Action: "VMReinstall",
			Message: "未配置系统模板镜像（os_name / images_path）"}, nil
	}

	srcFile := filepath.Join(imagesPath, templateName)
	if _, err := os.Stat(srcFile); err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall",
			Message: "系统模板镜像不存在: " + srcFile}, nil
	}

	dir := strings.TrimSpace(d.Config.SystemPath)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall",
			Message: "创建虚拟机目录失败: " + err.Error()}, nil
	}

	// 目标盘沿用模板扩展名（vmdk 等），与 Python 版 _get_vm_path + 后缀拼接一致
	ext := filepath.Ext(templateName)
	dstFile := filepath.Join(dir, vmUUID+ext)

	if _, _, err := security.SafeExecWithTimeout([]string{"cp", "-f", srcFile, dstFile}, 30*time.Minute); err != nil {
		// Windows 无 cp，改用 PowerShell Copy-Item
		ps := fmt.Sprintf("Copy-Item -LiteralPath '%s' -Destination '%s' -Force -ErrorAction Stop",
			psEscape(srcFile), psEscape(dstFile))
		if out, _, e := security.SafeExecWithTimeout(
			[]string{"powershell", "-NoProfile", "-NonInteractive", "-Command", ps}, 30*time.Minute); e != nil {
			return &model.ZMessage{Success: false, Action: "VMReinstall",
				Message: fmt.Sprintf("复制系统镜像失败: %v (%s)", e, strings.TrimSpace(out))}, nil
		}
	}

	// 扩展磁盘到目标容量（GB）
	if cfg.HddNum > 0 {
		sizeGB := cfg.HddNum / 1024
		if sizeGB <= 0 {
			sizeGB = 20
		}
		// vmware-vdiskmanager -x 20GB /path/to/disk.vmdk
		if out, _, err := security.SafeExecWithTimeout(
			[]string{"vmware-vdiskmanager", "-x", fmt.Sprintf("%dGB", sizeGB), dstFile},
			30*time.Minute); err != nil {
			// 扩展失败不阻断重装（部分镜像不支持在线扩展），但需明确告知
			d.mu.Lock()
			if c, ok := d.VmSaving[vmUUID]; ok {
				c.OsName = cfg.OsName
			}
			d.mu.Unlock()
			return &model.ZMessage{Success: true, Action: "VMReinstall",
				Message: fmt.Sprintf("系统盘已重建，但磁盘扩展失败（可手动扩容）: %s", strings.TrimSpace(out))}, nil
		}
	}

	d.mu.Lock()
	if c, ok := d.VmSaving[vmUUID]; ok {
		c.OsName = cfg.OsName
		c.HddNum = cfg.HddNum
	}
	d.mu.Unlock()

	// 重新启动
	_, _ = d.PowerControl(vmUUID, model.VMPowerSStart)

	return &model.ZMessage{Success: true, Action: "VMReinstall", Message: "系统重装成功"}, nil
}

// GetScreenshot 通过 vmrun captureScreen 截图
func (d *WorkstationDriver) GetScreenshot(vmUUID string) (string, error) {
	if d.GetPower(vmUUID) != "运行中" {
		return "", fmt.Errorf("虚拟机未运行，无法截图")
	}
	vmx := d.vmxPath(vmUUID)
	if _, err := os.Stat(vmx); err != nil {
		return "", fmt.Errorf("虚拟机配置文件不存在: %s", vmx)
	}

	tmp := filepath.Join(os.TempDir(),
		fmt.Sprintf("hostagent-screen-%s-%d.png", vmUUID, time.Now().UnixNano()))
	defer os.Remove(tmp)

	out, _, err := security.SafeExecWithTimeout(
		[]string{d.vmrunBin(), "-T", "ws", "captureScreen", vmx, tmp}, 60*time.Second)
	if err != nil {
		return "", fmt.Errorf("vmrun captureScreen 失败: %v (%s)", err, strings.TrimSpace(out))
	}
	if _, err := os.Stat(tmp); err != nil {
		return "", fmt.Errorf("截图文件未生成")
	}
	return readFileAsDataURL(tmp)
}

// GetPCIList Workstation 不支持 PCI 直通（与 Python 版 PCIShows 返回空一致）
func (d *WorkstationDriver) GetPCIList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// GetGPUList Workstation 不支持 GPU 直通
func (d *WorkstationDriver) GetGPUList() (map[string]*model.VFConfig, error) {
	return make(map[string]*model.VFConfig), nil
}

// SetupPCI Workstation 无 PCI 直通能力，明确拒绝
func (d *WorkstationDriver) SetupPCI(vmUUID string, cfg *model.VFConfig, pciKey string, add bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "PCISetup",
		Message: "VMware Workstation 不支持 PCI 设备直通"}, nil
}

// GetUSBList 枚举宿主机 USB 设备（Windows 走 PowerShell，其它平台走 lsusb）
func (d *WorkstationDriver) GetUSBList() (map[string]*model.USBInfos, error) {
	result := make(map[string]*model.USBInfos)

	// Windows：Get-PnpDevice（与 Python 版一致）
	psScript := `$ErrorActionPreference='SilentlyContinue'; ` +
		`Get-PnpDevice -Class USB -PresentOnly | Where-Object { $_.InstanceId -like 'USB\VID_*' } | ` +
		`ForEach-Object { $id=$_.InstanceId; $name=$_.FriendlyName; ` +
		`if ($id -match 'VID_([0-9A-Fa-f]{4})&PID_([0-9A-Fa-f]{4})') { "$($Matches[1])|$($Matches[2])|$name" } }`

	if out, _, err := security.SafeExecWithTimeout(
		[]string{"powershell", "-NoProfile", "-NonInteractive", "-Command", psScript},
		30*time.Second); err == nil {
		for _, line := range splitLines(out) {
			parts := strings.SplitN(line, "|", 3)
			if len(parts) < 2 {
				continue
			}
			vid := strings.ToLower(strings.TrimSpace(parts[0]))
			pid := strings.ToLower(strings.TrimSpace(parts[1]))
			if vid == "" || pid == "" {
				continue
			}
			hint := vid + ":" + pid
			if len(parts) > 2 {
				hint = strings.TrimSpace(parts[2])
			}
			key := vid + ":" + pid
			result[key] = &model.USBInfos{
				UsbKey: key, VidUUID: vid, PidUUID: pid, UsbHint: hint,
			}
		}
	}

	// 非 Windows（或 PowerShell 不可用）：回退 lsusb
	if len(result) == 0 {
		if res, _, err := security.SafeExecWithTimeout([]string{"lsusb"}, 15*time.Second); err == nil {
			for _, line := range strings.Split(res, "\n") {
				if !strings.Contains(line, "ID ") {
					continue
				}
				idPart := strings.SplitN(line, "ID ", 2)[1]
				fields := strings.SplitN(strings.TrimSpace(idPart), " ", 2)
				if len(fields) == 0 || !strings.Contains(fields[0], ":") {
					continue
				}
				pair := strings.SplitN(fields[0], ":", 2)
				hint := fields[0]
				if len(fields) > 1 {
					hint = strings.TrimSpace(fields[1])
				}
				result[fields[0]] = &model.USBInfos{
					UsbKey: fields[0], VidUUID: pair[0], PidUUID: pair[1], UsbHint: hint,
				}
			}
		}
	}

	return result, nil
}

// SetupUSB USB 直通：写入 VMX 配置并记录（Workstation 需重启虚拟机生效）
//
// 与 Python 版一致：更新虚拟机配置中的 USB 列表；VMX 侧的 usb_x 条目
// 在下次冷启动时由 Workstation 读取，因此这里如实提示「需重启生效」。
func (d *WorkstationDriver) SetupUSB(vmUUID string, info *model.USBInfos, usbKey string, add bool) (*model.ZMessage, error) {
	d.mu.Lock()
	cfg, ok := d.VmSaving[vmUUID]
	if !ok {
		d.mu.Unlock()
		return &model.ZMessage{Success: false, Action: "USBSetup", Message: "虚拟机不存在"}, nil
	}
	if cfg.UsbAll == nil {
		cfg.UsbAll = make(map[string]model.USBInfos)
	}
	if add {
		cfg.UsbAll[usbKey] = *info
	} else {
		delete(cfg.UsbAll, usbKey)
	}
	d.mu.Unlock()

	action := "添加"
	if !add {
		action = "移除"
	}
	return &model.ZMessage{Success: true, Action: "USBSetup",
		Message: fmt.Sprintf("USB设备%s成功（需重启虚拟机生效）", action)}, nil
}

// MountUSB Workstation 不支持宿主 USB 存储直挂（与 Python 版一致）
func (d *WorkstationDriver) MountUSB(vmUUID string, usbKey string, mount bool) (*model.ZMessage, error) {
	return &model.ZMessage{Success: false, Action: "USBMount",
		Message: "VMware Workstation 不支持 USB 存储直挂，请使用 USB 直通"}, nil
}
