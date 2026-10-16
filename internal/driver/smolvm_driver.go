// Package driver SmolVM microVM 驱动（Firecracker + Docker rootfs）
// 参考 Python 版 HostServer/SmolVM.py 对等实现。
package driver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// ============================================================
// SmolVM Driver
// ============================================================

// SmolVMDriver 轻量级 microVM 驱动
type SmolVMDriver struct {
	*BaseDriver
	sockRoot string
	once     sync.Once
	procMu   sync.Mutex
}

// NewSmolVMDriver 创建 SmolVM 驱动
func NewSmolVMDriver(cfg *model.HSConfig) *SmolVMDriver {
	return &SmolVMDriver{
		BaseDriver: NewBaseDriver(cfg),
		sockRoot:   "/run/smolvm",
	}
}

// ============================================================
// 工具函数：本地/远程命令执行
// ============================================================

// isRemote 判断是否为远程主机
func (d *SmolVMDriver) isRemote() bool {
	addr := d.Config.ServerAddr
	if addr == "" || addr == "localhost" || addr == "127.0.0.1" {
		return false
	}
	return true
}

// execHost 在宿主机上执行命令（本地直接 exec；远程通过 ssh）
func (d *SmolVMDriver) execHost(cmd string, timeout time.Duration) (bool, string, string) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if d.isRemote() {
		addr := strings.TrimPrefix(d.Config.ServerAddr, "ssh://")
		port := d.Config.ServerPort
		if port <= 0 {
			port = 22
		}
		user := d.Config.ServerUser
		if user == "" {
			user = "root"
		}
		// 使用 sshpass + ssh（生产环境建议改为基于 golang.org/x/crypto/ssh）
		target := fmt.Sprintf("%s@%s", user, addr)
		args := []string{
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-p", strconv.Itoa(port),
			target, cmd,
		}
		sshpass, _ := exec.LookPath("sshpass")
		var c *exec.Cmd
		if sshpass != "" && d.Config.ServerPass != "" {
			all := append([]string{"-p", d.Config.ServerPass, "ssh"}, args...)
			c = exec.CommandContext(ctx, "sshpass", all...)
		} else {
			c = exec.CommandContext(ctx, "ssh", args...)
		}
		var stdout, stderr bytes.Buffer
		c.Stdout = &stdout
		c.Stderr = &stderr
		err := c.Run()
		return err == nil, stdout.String(), stderr.String()
	}

	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	return err == nil, stdout.String(), stderr.String()
}

// ============================================================
// 路径 / 名称约定
// ============================================================

func (d *SmolVMDriver) vmDir(vmUUID string) string {
	base := d.Config.ExternPath
	if base == "" {
		base = "/var/lib/smolvm"
	}
	return filepath.ToSlash(filepath.Join(base, vmUUID))
}

func (d *SmolVMDriver) vmSock(vmUUID string) string {
	return filepath.ToSlash(filepath.Join(d.sockRoot, vmUUID+".sock"))
}

func (d *SmolVMDriver) vmPidFile(vmUUID string) string {
	return filepath.ToSlash(filepath.Join(d.sockRoot, vmUUID+".pid"))
}

func (d *SmolVMDriver) tapName(vmUUID string) string {
	clean := strings.ReplaceAll(vmUUID, "-", "")
	if len(clean) > 10 {
		clean = clean[:10]
	}
	name := "tap" + clean
	if len(name) > 15 {
		name = name[:15]
	}
	return name
}

func (d *SmolVMDriver) kernelPath() string {
	if d.Config.ExtendData != nil {
		if v, ok := d.Config.ExtendData["smolvm_kernel"].(string); ok && v != "" {
			return v
		}
	}
	base := d.Config.ImagesPath
	if base == "" {
		base = "/var/lib/smolvm/images"
	}
	return filepath.ToSlash(filepath.Join(base, "vmlinux-smolvm"))
}

func (d *SmolVMDriver) hypervisorBin() string {
	if d.Config.ExtendData != nil {
		if v, ok := d.Config.ExtendData["smolvm_hv_path"].(string); ok && v != "" {
			return v
		}
	}
	return "firecracker"
}

// ============================================================
// Firecracker HTTP over UDS 客户端
// ============================================================

// fcClient 返回一个指向指定 socket 的 *http.Client
func (d *SmolVMDriver) fcClient(sock string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				dialer := &net.Dialer{Timeout: 3 * time.Second}
				return dialer.DialContext(ctx, "unix", sock)
			},
		},
	}
}

// fcDo 执行一次 HTTP 请求到 firecracker socket
func (d *SmolVMDriver) fcDo(sock, method, path string, body interface{}) (int, map[string]interface{}, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, "http://unix"+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := d.fcClient(sock).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	result := map[string]interface{}{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &result)
	}
	return resp.StatusCode, result, nil
}

// waitSockReady 等待 socket 可连接
func (d *SmolVMDriver) waitSockReady(sock string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok, _, _ := d.execHost(fmt.Sprintf("test -S \"%s\" && echo YES || echo NO", sock), 3*time.Second); ok {
			if !d.isRemote() {
				c, err := net.DialTimeout("unix", sock, 500*time.Millisecond)
				if err == nil {
					_ = c.Close()
					return true
				}
			} else {
				// 远端：仅确认文件存在
				return true
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return false
}

// ============================================================
// 镜像与 rootfs 构建（通过宿主机命令完成）
// ============================================================

// ensureDockerImage 确保 Docker 镜像存在
func (d *SmolVMDriver) ensureDockerImage(image string) error {
	ok, out, _ := d.execHost(fmt.Sprintf(
		"docker image inspect %s >/dev/null 2>&1 && echo YES || echo NO", image),
		15*time.Second)
	if ok && strings.Contains(out, "YES") {
		return nil
	}
	ok, _, err := d.execHost(fmt.Sprintf("docker pull %s", image), 15*time.Minute)
	if !ok {
		return fmt.Errorf("docker pull 失败: %s", err)
	}
	return nil
}

// buildRootfs 构建 ext4 rootfs 镜像
func (d *SmolVMDriver) buildRootfs(image, outRootfs string, sizeMB int, rootPass string) error {
	if sizeMB <= 0 {
		sizeMB = 8 * 1024
	}

	// 处理 tar 文件离线加载
	if strings.HasSuffix(image, ".tar") || strings.HasSuffix(image, ".tar.gz") {
		tarPath := image
		if !filepath.IsAbs(tarPath) {
			tarPath = filepath.ToSlash(filepath.Join(d.Config.ImagesPath, image))
		}
		// 加载
		loadCmd := fmt.Sprintf("docker load -i \"%s\"", tarPath)
		if strings.HasSuffix(image, ".tar.gz") {
			loadCmd = fmt.Sprintf("gunzip -c \"%s\" | docker load", tarPath)
		}
		if ok, _, err := d.execHost(loadCmd, 15*time.Minute); !ok {
			return fmt.Errorf("docker load 失败: %s", err)
		}
		image = strings.TrimSuffix(strings.TrimSuffix(filepath.Base(image), ".tar.gz"), ".tar")
	} else if err := d.ensureDockerImage(image); err != nil {
		return err
	}

	outDir := filepath.ToSlash(filepath.Dir(outRootfs))
	workTag := fmt.Sprintf("%d", time.Now().UnixNano())
	workDir := fmt.Sprintf("%s/.build-%s", outDir, workTag)
	exportTar := fmt.Sprintf("%s/.export-%s.tar", outDir, workTag)
	containerName := fmt.Sprintf("smolvm-build-%s", workTag[len(workTag)-8:])

	cleanupFns := []func(){
		func() { d.execHost(fmt.Sprintf("docker rm -f %s >/dev/null 2>&1 || true", containerName), 30*time.Second) },
		func() { d.execHost(fmt.Sprintf("rm -f \"%s\"", exportTar), 10*time.Second) },
		func() { d.execHost(fmt.Sprintf("rm -rf \"%s\"", workDir), 30*time.Second) },
	}
	defer func() {
		for _, f := range cleanupFns {
			f()
		}
	}()

	d.execHost(fmt.Sprintf("mkdir -p \"%s\" \"%s\"", outDir, workDir), 10*time.Second)

	if ok, _, err := d.execHost(fmt.Sprintf("docker create --name %s %s /bin/sh", containerName, image), 2*time.Minute); !ok {
		return fmt.Errorf("docker create 失败: %s", err)
	}
	if ok, _, err := d.execHost(fmt.Sprintf("docker export %s -o \"%s\"", containerName, exportTar), 10*time.Minute); !ok {
		return fmt.Errorf("docker export 失败: %s", err)
	}
	if ok, _, err := d.execHost(fmt.Sprintf("tar -xf \"%s\" -C \"%s\"", exportTar, workDir), 10*time.Minute); !ok {
		return fmt.Errorf("解压 rootfs 失败: %s", err)
	}

	// 注入最小 init + SSHD 配置
	initScript := `#!/bin/sh
mount -t proc proc /proc 2>/dev/null
mount -t sysfs sys /sys 2>/dev/null
mount -t devtmpfs dev /dev 2>/dev/null || true
ip link set lo up 2>/dev/null || true
[ -x /sbin/udhcpc ] && udhcpc -i eth0 -q -f -n 2>/dev/null &
if [ -x /usr/sbin/sshd ]; then
    mkdir -p /var/run/sshd
    /usr/bin/ssh-keygen -A 2>/dev/null || true
    /usr/sbin/sshd -D &
fi
exec /bin/sh
`
	d.writeRemoteFile(filepath.ToSlash(filepath.Join(workDir, "init")), initScript, 0755)
	sshdConfig := "Port 22\nPermitRootLogin yes\nPasswordAuthentication yes\nPermitEmptyPasswords no\nUsePAM no\n"
	d.execHost(fmt.Sprintf("mkdir -p \"%s/etc/ssh\"", workDir), 5*time.Second)
	d.writeRemoteFile(filepath.ToSlash(filepath.Join(workDir, "etc/ssh/sshd_config")), sshdConfig, 0644)

	// 生成密码哈希并写入 shadow
	if rootPass != "" {
		ok, out, _ := d.execHost(fmt.Sprintf("openssl passwd -6 '%s' 2>/dev/null || openssl passwd -1 '%s'",
			shEscape(rootPass), shEscape(rootPass)), 10*time.Second)
		if ok {
			hash := strings.TrimSpace(out)
			lines := strings.Split(hash, "\n")
			if len(lines) > 0 {
				hash = lines[len(lines)-1]
			}
			shadow := filepath.ToSlash(filepath.Join(workDir, "etc/shadow"))
			escaped := strings.ReplaceAll(hash, "|", `\|`)
			d.execHost(fmt.Sprintf(
				"sh -c 'if [ -f \"%s\" ]; then sed -i -E \"s|^root:[^:]*:|root:%s:|\" \"%s\"; "+
					"else echo \"root:%s:0:0:99999:7:::\" > \"%s\"; fi'",
				shadow, escaped, shadow, escaped, shadow), 5*time.Second)
		}
	}

	// 构建 ext4 镜像
	d.execHost(fmt.Sprintf("truncate -s %dM \"%s\"", sizeMB, outRootfs), 15*time.Second)
	if ok, _, err := d.execHost(fmt.Sprintf("mkfs.ext4 -F -L rootfs -d \"%s\" \"%s\"", workDir, outRootfs),
		5*time.Minute); !ok {
		// 回退：mkfs + mount + cp
		mnt := outRootfs + ".mnt"
		d.execHost(fmt.Sprintf("mkfs.ext4 -F -L rootfs \"%s\" && mkdir -p \"%s\"", outRootfs, mnt), 2*time.Minute)
		if ok2, _, merr := d.execHost(fmt.Sprintf("mount -o loop \"%s\" \"%s\"", outRootfs, mnt), 10*time.Second); !ok2 {
			return fmt.Errorf("mkfs/mount 失败: %s %s", err, merr)
		}
		d.execHost(fmt.Sprintf("cp -a \"%s\"/. \"%s\"/", workDir, mnt), 20*time.Minute)
		d.execHost(fmt.Sprintf("sync && umount \"%s\" || umount -l \"%s\"; rmdir \"%s\" 2>/dev/null || true",
			mnt, mnt, mnt), 30*time.Second)
	}

	return nil
}

// writeRemoteFile 在宿主机（远程/本地）写入文本文件
func (d *SmolVMDriver) writeRemoteFile(path, content string, mode os.FileMode) {
	// 使用 here-doc
	// 为防止内容中出现 EOF_SMOLVM 自身，此处不做转义（约定内容安全）
	escaped := strings.ReplaceAll(content, "$", `\$`)
	cmd := fmt.Sprintf(
		"cat >\"%s\" <<'EOF_SMOLVM'\n%s\nEOF_SMOLVM\nchmod %o \"%s\"",
		path, escaped, mode, path)
	d.execHost(cmd, 10*time.Second)
}

// ============================================================
// 网络：tap 设备
// ============================================================

func (d *SmolVMDriver) bindTap(cfg *model.VMConfig, up bool) error {
	tap := d.tapName(cfg.VmUUID)
	if !up {
		d.execHost(fmt.Sprintf("ip link set %s down 2>/dev/null || true", tap), 5*time.Second)
		d.execHost(fmt.Sprintf("ip tuntap del dev %s mode tap 2>/dev/null || true", tap), 5*time.Second)
		return nil
	}
	d.execHost(fmt.Sprintf("ip tuntap add dev %s mode tap 2>/dev/null || true", tap), 5*time.Second)
	d.execHost(fmt.Sprintf("ip link set %s up", tap), 5*time.Second)

	bridge := d.Config.NetworkNat
	// 第一个网卡默认使用 NAT bridge；若有 NetworkPub 则覆盖（简化处理）
	_ = cfg
	if bridge == "" && d.Config.NetworkPub != "" {
		bridge = d.Config.NetworkPub
	}
	if bridge != "" {
		d.execHost(fmt.Sprintf(
			"ip link set %s master %s 2>/dev/null || brctl addif %s %s 2>/dev/null || true",
			tap, bridge, bridge, tap), 5*time.Second)
	}
	return nil
}

// ============================================================
// 核心接口实现
// ============================================================

// GetPower 获取虚拟机电源状态
func (d *SmolVMDriver) GetPower(vmUUID string) string {
	pid := d.readPid(vmUUID)
	if pid <= 0 || !d.pidAlive(pid) {
		return "已关机"
	}
	if !d.isRemote() {
		code, resp, err := d.fcDo(d.vmSock(vmUUID), "GET", "/", nil)
		if err == nil && code == 200 {
			state, _ := resp["state"].(string)
			switch strings.ToLower(state) {
			case "running":
				return "运行中"
			case "paused":
				return "已暂停"
			case "not started":
				return "已关机"
			case "restarting":
				return "重启中"
			}
		}
	}
	return "运行中"
}

func (d *SmolVMDriver) readPid(vmUUID string) int {
	ok, out, _ := d.execHost(fmt.Sprintf("cat \"%s\" 2>/dev/null || true", d.vmPidFile(vmUUID)), 5*time.Second)
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

func (d *SmolVMDriver) pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	ok, out, _ := d.execHost(fmt.Sprintf("kill -0 %d 2>/dev/null && echo YES || echo NO", pid),
		5*time.Second)
	return ok && strings.Contains(out, "YES")
}

func (d *SmolVMDriver) killPid(pid int, force bool) {
	if pid <= 0 {
		return
	}
	sig := "-15"
	if force {
		sig = "-9"
	}
	d.execHost(fmt.Sprintf("kill %s %d 2>/dev/null || true", sig, pid), 5*time.Second)
}

// CreateVM 创建 microVM
func (d *SmolVMDriver) CreateVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	d.ensureDirs()
	vmDir := d.vmDir(cfg.VmUUID)
	rootfs := filepath.ToSlash(filepath.Join(vmDir, "rootfs.ext4"))
	sock := d.vmSock(cfg.VmUUID)
	pidFile := d.vmPidFile(cfg.VmUUID)

	// 1) 构建 rootfs
	sizeMB := cfg.HddNum
	if sizeMB <= 0 {
		sizeMB = 8 * 1024
	} else {
		sizeMB = cfg.HddNum
	}
	d.execHost(fmt.Sprintf("mkdir -p \"%s\"", vmDir), 5*time.Second)
	if err := d.buildRootfs(cfg.OsName, rootfs, sizeMB, cfg.OsPass); err != nil {
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: fmt.Sprintf("rootfs 构建失败: %v", err)}, nil
	}

	// 2) tap 设备
	d.bindTap(cfg, true)

	// 3) 启动 firecracker
	d.execHost(fmt.Sprintf("rm -f \"%s\"", sock), 5*time.Second)
	hv := d.hypervisorBin()
	startCmd := fmt.Sprintf("setsid %s --api-sock \"%s\" >\"%s/fc.log\" 2>&1 & echo $! > \"%s\"",
		hv, sock, vmDir, pidFile)
	if ok, _, err := d.execHost(startCmd, 10*time.Second); !ok {
		d.bindTap(cfg, false)
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: fmt.Sprintf("启动 firecracker 失败: %s", err)}, nil
	}
	d.execHost(fmt.Sprintf("sleep 0.3; chmod 0600 \"%s\" 2>/dev/null || true", sock), 3*time.Second)

	// 4) FC API 装配
	if !d.waitSockReady(sock, 10*time.Second) {
		d.killPid(d.readPid(cfg.VmUUID), true)
		d.bindTap(cfg, false)
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: "firecracker socket 未就绪"}, nil
	}
	if err := d.assembleVM(cfg, sock, rootfs); err != nil {
		d.killPid(d.readPid(cfg.VmUUID), true)
		d.execHost(fmt.Sprintf("rm -f \"%s\" \"%s\"", sock, pidFile), 3*time.Second)
		d.bindTap(cfg, false)
		return &model.ZMessage{Success: false, Action: "VMCreate",
			Message: fmt.Sprintf("FC API 装配失败: %v", err)}, nil
	}

	// 5) 写 vm.json
	d.saveVMJSON(cfg)

	// 6) 同步到内存
	d.mu.Lock()
	d.VmSaving[cfg.VmUUID] = cfg
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMCreate",
		Message: fmt.Sprintf("microVM %s 创建成功", cfg.VmUUID)}, nil
}

// assembleVM 通过 FC API 装配虚拟机
func (d *SmolVMDriver) assembleVM(cfg *model.VMConfig, sock, rootfs string) error {
	bootArgs := "console=ttyS0 reboot=k panic=1 pci=off init=/sbin/init ip=dhcp"
	if code, _, err := d.fcDo(sock, "PUT", "/boot-source", map[string]interface{}{
		"kernel_image_path": d.kernelPath(), "boot_args": bootArgs}); err != nil || code >= 400 {
		return fmt.Errorf("/boot-source code=%d err=%v", code, err)
	}
	if code, _, err := d.fcDo(sock, "PUT", "/drives/rootfs", map[string]interface{}{
		"drive_id": "rootfs", "path_on_host": rootfs,
		"is_root_device": true, "is_read_only": false}); err != nil || code >= 400 {
		return fmt.Errorf("/drives/rootfs code=%d err=%v", code, err)
	}

	// 数据盘：Firecracker 以额外 drive 形式挂载（已挂载的才下发）
	for name, hdd := range cfg.HddAll {
		if hdd.HddFlag != 1 {
			continue
		}
		p := hdd.HddPath
		if p == "" {
			p = filepath.ToSlash(filepath.Join(d.vmDir(cfg.VmUUID), name+".ext4"))
		}
		if code, _, err := d.fcDo(sock, "PUT", "/drives/"+name, map[string]interface{}{
			"drive_id": name, "path_on_host": p,
			"is_root_device": false, "is_read_only": false}); err != nil || code >= 400 {
			return fmt.Errorf("/drives/%s code=%d err=%v", name, code, err)
		}
	}
	mac := ""
	for _, nic := range cfg.NicAll {
		mac = nic.MacAddr
		break
	}
	iface := map[string]interface{}{
		"iface_id": "eth0", "host_dev_name": d.tapName(cfg.VmUUID),
	}
	if mac != "" {
		iface["guest_mac"] = mac
	}
	if code, _, err := d.fcDo(sock, "PUT", "/network-interfaces/eth0", iface); err != nil || code >= 400 {
		return fmt.Errorf("/network-interfaces/eth0 code=%d err=%v", code, err)
	}
	vcpu := cfg.CpuNum
	if vcpu <= 0 {
		vcpu = 1
	}
	mem := cfg.MemNum
	if mem <= 0 {
		mem = 512
	}
	if code, _, err := d.fcDo(sock, "PUT", "/machine-config", map[string]interface{}{
		"vcpu_count": vcpu, "mem_size_mib": mem, "smt": false}); err != nil || code >= 400 {
		return fmt.Errorf("/machine-config code=%d err=%v", code, err)
	}
	if code, _, err := d.fcDo(sock, "PUT", "/actions", map[string]interface{}{
		"action_type": "InstanceStart"}); err != nil || code >= 400 {
		return fmt.Errorf("/actions code=%d err=%v", code, err)
	}
	return nil
}

// saveVMJSON 持久化 VM 配置
func (d *SmolVMDriver) saveVMJSON(cfg *model.VMConfig) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	path := filepath.ToSlash(filepath.Join(d.vmDir(cfg.VmUUID), "vm.json"))
	d.writeRemoteFile(path, string(data), 0644)
}

// loadVMJSON 读回 VM 配置
func (d *SmolVMDriver) loadVMJSON(vmUUID string) *model.VMConfig {
	path := filepath.ToSlash(filepath.Join(d.vmDir(vmUUID), "vm.json"))
	ok, out, _ := d.execHost(fmt.Sprintf("cat \"%s\" 2>/dev/null || true", path), 5*time.Second)
	if !ok || strings.TrimSpace(out) == "" {
		return nil
	}
	var cfg model.VMConfig
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		return nil
	}
	return &cfg
}

// ensureDirs 确保基础目录存在
func (d *SmolVMDriver) ensureDirs() {
	d.once.Do(func() {
		d.execHost(fmt.Sprintf("mkdir -p \"%s\" && chmod 0700 \"%s\"", d.sockRoot, d.sockRoot), 5*time.Second)
		if d.Config.ExternPath != "" {
			d.execHost(fmt.Sprintf("mkdir -p \"%s\"", d.Config.ExternPath), 5*time.Second)
		}
		if d.Config.BackupPath != "" {
			d.execHost(fmt.Sprintf("mkdir -p \"%s\"", d.Config.BackupPath), 5*time.Second)
		}
	})
}

// DeleteVM 删除 microVM
func (d *SmolVMDriver) DeleteVM(vmUUID string, removeBackup bool) (*model.ZMessage, error) {
	d.mu.RLock()
	cfg, ok := d.VmSaving[vmUUID]
	d.mu.RUnlock()
	if !ok {
		return &model.ZMessage{Success: false, Action: "VMDelete",
			Message: "microVM 不存在"}, nil
	}

	pid := d.readPid(vmUUID)
	if pid > 0 && d.pidAlive(pid) {
		d.killPid(pid, true)
	}
	d.execHost(fmt.Sprintf("rm -f \"%s\" \"%s\"", d.vmSock(vmUUID), d.vmPidFile(vmUUID)), 5*time.Second)
	d.bindTap(cfg, false)
	d.execHost(fmt.Sprintf("rm -rf \"%s\"", d.vmDir(vmUUID)), 30*time.Second)

	d.mu.Lock()
	delete(d.VmSaving, vmUUID)
	d.mu.Unlock()

	return &model.ZMessage{Success: true, Action: "VMDelete",
		Message: "microVM 已删除"}, nil
}

// PowerControl 电源操作
func (d *SmolVMDriver) PowerControl(vmUUID string, action model.VMPowers) (*model.ZMessage, error) {
	d.mu.RLock()
	cfg := d.VmSaving[vmUUID]
	d.mu.RUnlock()
	if cfg == nil {
		return &model.ZMessage{Success: false, Action: "VMPowers",
			Message: "microVM 不存在"}, nil
	}

	// 先设置中间状态
	if newState, ok := PowerActionMap[action]; ok {
		cfg.VmFlag = newState
	}

	sock := d.vmSock(vmUUID)
	pid := d.readPid(vmUUID)

	switch action {
	case model.VMPowerSStart:
		if pid > 0 && d.pidAlive(pid) {
			d.fcDo(sock, "PATCH", "/vm", map[string]interface{}{"state": "Resumed"})
		} else {
			d.ensureDirs()
			d.bindTap(cfg, true)
			d.execHost(fmt.Sprintf("rm -f \"%s\"", sock), 3*time.Second)
			startCmd := fmt.Sprintf("setsid %s --api-sock \"%s\" >\"%s/fc.log\" 2>&1 & echo $! > \"%s\"",
				d.hypervisorBin(), sock, d.vmDir(vmUUID), d.vmPidFile(vmUUID))
			d.execHost(startCmd, 10*time.Second)
			d.execHost(fmt.Sprintf("sleep 0.3; chmod 0600 \"%s\" 2>/dev/null || true", sock), 3*time.Second)
			if !d.waitSockReady(sock, 10*time.Second) {
				return &model.ZMessage{Success: false, Action: "VMPowers",
					Message: "socket 未就绪"}, nil
			}
			if err := d.assembleVM(cfg, sock, filepath.ToSlash(filepath.Join(d.vmDir(vmUUID), "rootfs.ext4"))); err != nil {
				return &model.ZMessage{Success: false, Action: "VMPowers",
					Message: fmt.Sprintf("启动失败: %v", err)}, nil
			}
		}
	case model.VMPowerHClose:
		if pid > 0 {
			d.killPid(pid, true)
		}
		d.execHost(fmt.Sprintf("rm -f \"%s\" \"%s\"", sock, d.vmPidFile(vmUUID)), 5*time.Second)
	case model.VMPowerSClose:
		d.fcDo(sock, "PUT", "/actions", map[string]interface{}{"action_type": "SendCtrlAltDel"})
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if pid > 0 && !d.pidAlive(pid) {
				break
			}
			time.Sleep(1 * time.Second)
		}
		if pid > 0 && d.pidAlive(pid) {
			d.killPid(pid, true)
		}
		d.execHost(fmt.Sprintf("rm -f \"%s\" \"%s\"", sock, d.vmPidFile(vmUUID)), 5*time.Second)
	case model.VMPowerSReset, model.VMPowerHReset:
		if action == model.VMPowerSReset {
			d.PowerControl(vmUUID, model.VMPowerSClose)
		} else {
			d.PowerControl(vmUUID, model.VMPowerHClose)
		}
		time.Sleep(1 * time.Second)
		d.PowerControl(vmUUID, model.VMPowerSStart)
	case model.VMPowerAPause:
		d.fcDo(sock, "PATCH", "/vm", map[string]interface{}{"state": "Paused"})
	case model.VMPowerAWaked:
		d.fcDo(sock, "PATCH", "/vm", map[string]interface{}{"state": "Resumed"})
	}

	return &model.ZMessage{Success: true, Action: "VMPowers",
		Message: "电源操作已执行"}, nil
}

// ScanVMs 扫描 extern_path 下的 microVM 目录
func (d *SmolVMDriver) ScanVMs() (*model.ZMessage, error) {
	base := d.Config.ExternPath
	if base == "" {
		return &model.ZMessage{Success: false, Action: "VMDetect",
			Message: "extern_path 未配置"}, nil
	}
	prefix := d.Config.FilterName
	ok, out, _ := d.execHost(fmt.Sprintf("ls -1 \"%s\" 2>/dev/null || true", base), 15*time.Second)
	if !ok {
		return &model.ZMessage{Success: true, Action: "VMDetect",
			Message: "扫描完成（目录不存在）"}, nil
	}

	scanner := bufio.NewScanner(strings.NewReader(out))
	scanned, added := 0, 0
	for scanner.Scan() {
		name := strings.TrimSpace(scanner.Text())
		if name == "" {
			continue
		}
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		scanned++

		d.mu.RLock()
		_, existed := d.VmSaving[name]
		d.mu.RUnlock()
		if existed {
			continue
		}

		vmCfg := d.loadVMJSON(name)
		if vmCfg == nil {
			vmCfg = model.NewVMConfig()
			vmCfg.VmUUID = name
		}
		d.mu.Lock()
		d.VmSaving[name] = vmCfg
		d.mu.Unlock()
		added++
	}
	return &model.ZMessage{Success: true, Action: "VMDetect",
		Message: fmt.Sprintf("扫描完成：共 %d 个，新增 %d 个", scanned, added)}, nil
}

// SetPassword 设置 root 密码（在线走 SSH，离线挂载 rootfs 改 shadow）
func (d *SmolVMDriver) SetPassword(vmUUID string, password string) (*model.ZMessage, error) {
	// 标识会参与宿主机目录与命令拼接，必须先校验
	if err := safeIdent(vmUUID, 128); err != nil {
		return &model.ZMessage{Success: false, Action: "Password",
			Message: "microVM 标识无效: " + err.Error()}, nil
	}
	d.mu.RLock()
	cfg, ok := d.VmSaving[vmUUID]
	d.mu.RUnlock()
	if !ok {
		return &model.ZMessage{Success: false, Action: "Password",
			Message: "microVM 不存在"}, nil
	}

	pid := d.readPid(vmUUID)
	if pid > 0 && d.pidAlive(pid) {
		// 在线改密（通过宿主机 ssh 到 VM IP）
		vmIP := ""
		for _, nic := range cfg.NicAll {
			if nic.IP4Addr != "" {
				vmIP = nic.IP4Addr
				break
			}
		}
		if vmIP == "" {
			return &model.ZMessage{Success: false, Action: "Password",
				Message: "microVM 未分配 IP"}, nil
		}
		// 口令进入本地 shell 命令串，必须转义；
		// IP 只接受合法地址，避免被拼成主机名或附加参数。
		if net.ParseIP(strings.TrimSpace(vmIP)) == nil {
			return &model.ZMessage{Success: false, Action: "Password",
				Message: "microVM 的 IP 地址无效"}, nil
		}
		cmd := fmt.Sprintf(
			"sshpass -p '%s' ssh -o StrictHostKeyChecking=no root@%s \"echo 'root:%s' | chpasswd\"",
			shEscape(password), vmIP, shEscape(password))
		if ok2, _, err := d.execHost(cmd, 30*time.Second); !ok2 {
			return &model.ZMessage{Success: false, Action: "Password",
				Message: fmt.Sprintf("在线改密失败: %s", err)}, nil
		}
		cfg.OsPass = password
		return &model.ZMessage{Success: true, Action: "Password",
			Message: "密码已更新（在线）"}, nil
	}

	// 离线改密
	rootfs := filepath.ToSlash(filepath.Join(d.vmDir(vmUUID), "rootfs.ext4"))
	mnt := filepath.ToSlash(filepath.Join(d.vmDir(vmUUID), ".mnt-passwd"))
	d.execHost(fmt.Sprintf("mkdir -p \"%s\"", mnt), 5*time.Second)
	if ok2, _, err := d.execHost(fmt.Sprintf("mount -o loop \"%s\" \"%s\"", rootfs, mnt), 10*time.Second); !ok2 {
		return &model.ZMessage{Success: false, Action: "Password",
			Message: fmt.Sprintf("挂载 rootfs 失败: %s", err)}, nil
	}
	defer func() {
		d.execHost(fmt.Sprintf("sync && umount \"%s\" || umount -l \"%s\"; rmdir \"%s\" 2>/dev/null || true",
			mnt, mnt, mnt), 10*time.Second)
	}()
	okp, out, _ := d.execHost(fmt.Sprintf(
		"openssl passwd -6 '%s' 2>/dev/null || openssl passwd -1 '%s'",
		shEscape(password), shEscape(password)), 10*time.Second)
	if !okp {
		return &model.ZMessage{Success: false, Action: "Password",
			Message: "openssl 生成密文失败"}, nil
	}
	hash := strings.TrimSpace(out)
	lines := strings.Split(hash, "\n")
	if len(lines) > 0 {
		hash = lines[len(lines)-1]
	}
	shadow := filepath.ToSlash(filepath.Join(mnt, "etc/shadow"))
	escaped := strings.ReplaceAll(hash, "|", `\|`)
	d.execHost(fmt.Sprintf(
		"sh -c 'if [ -f \"%s\" ]; then sed -i -E \"s|^root:[^:]*:|root:%s:|\" \"%s\"; "+
			"else echo \"root:%s:0:0:99999:7:::\" > \"%s\"; fi'",
		shadow, escaped, shadow, escaped, shadow), 10*time.Second)

	cfg.OsPass = password
	return &model.ZMessage{Success: true, Action: "Password",
		Message: "密码已更新（离线）"}, nil
}

// CreateBackup 备份（tar -czf extern_path/<vm>）
func (d *SmolVMDriver) CreateBackup(vmUUID string, tips string) (*model.ZMessage, error) {
	d.mu.RLock()
	cfg, ok := d.VmSaving[vmUUID]
	d.mu.RUnlock()
	if !ok {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: "microVM 不存在"}, nil
	}
	_ = cfg
	if d.Config.BackupPath == "" {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: "backup_path 未配置"}, nil
	}
	wasRunning := false
	pid := d.readPid(vmUUID)
	if pid > 0 && d.pidAlive(pid) {
		wasRunning = true
		d.PowerControl(vmUUID, model.VMPowerSClose)
	}
	bakFile := fmt.Sprintf("%s_%s.tar.gz", vmUUID, time.Now().Format("20060102_150405"))
	bakPath := filepath.ToSlash(filepath.Join(d.Config.BackupPath, bakFile))
	d.execHost(fmt.Sprintf("mkdir -p \"%s\"", d.Config.BackupPath), 5*time.Second)
	cmd := fmt.Sprintf("tar -C \"%s\" -czf \"%s\" \"%s\"", d.Config.ExternPath, bakPath, vmUUID)
	if ok2, _, err := d.execHost(cmd, 60*time.Minute); !ok2 {
		return &model.ZMessage{Success: false, Action: "VMBackup",
			Message: fmt.Sprintf("备份打包失败: %s", err)}, nil
	}
	if wasRunning {
		d.PowerControl(vmUUID, model.VMPowerSStart)
	}
	return &model.ZMessage{Success: true, Action: "VMBackup",
		Message: fmt.Sprintf("microVM 备份成功: %s", bakFile),
		Results: map[string]string{"backup_file": bakFile, "backup_path": bakPath, "tips": tips}}, nil
}

// RestoreBackup 恢复
func (d *SmolVMDriver) RestoreBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	bakPath := filepath.ToSlash(filepath.Join(d.Config.BackupPath, backupName))
	ok, out, _ := d.execHost(fmt.Sprintf("test -f \"%s\" && echo YES || echo NO", bakPath), 5*time.Second)
	if !ok || !strings.Contains(out, "YES") {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: fmt.Sprintf("备份不存在: %s", bakPath)}, nil
	}
	pid := d.readPid(vmUUID)
	if pid > 0 && d.pidAlive(pid) {
		d.killPid(pid, true)
	}
	d.execHost(fmt.Sprintf("rm -f \"%s\" \"%s\"", d.vmSock(vmUUID), d.vmPidFile(vmUUID)), 5*time.Second)
	d.execHost(fmt.Sprintf("rm -rf \"%s\"", d.vmDir(vmUUID)), 30*time.Second)
	if ok2, _, err := d.execHost(fmt.Sprintf("tar -C \"%s\" -xzf \"%s\"",
		d.Config.ExternPath, bakPath), 60*time.Minute); !ok2 {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: fmt.Sprintf("解压备份失败: %s", err)}, nil
	}

	cfg := d.loadVMJSON(vmUUID)
	if cfg == nil {
		return &model.ZMessage{Success: false, Action: "Restores",
			Message: "备份中未包含 vm.json"}, nil
	}
	d.mu.Lock()
	d.VmSaving[vmUUID] = cfg
	d.mu.Unlock()
	return d.PowerControl(vmUUID, model.VMPowerSStart)
}

// DeleteBackup 删除备份
func (d *SmolVMDriver) DeleteBackup(vmUUID string, backupName string) (*model.ZMessage, error) {
	// 只取文件名本身并绑定虚拟机前缀：
	// 原实现直接把请求值拼进 rm -f，可用 ../../ 删除备份目录之外的文件。
	name, err := sanitizeFileName(backupName)
	if err != nil || !strings.HasPrefix(name, vmUUID+"-") {
		return &model.ZMessage{Success: false, Action: "RMBackup",
			Message: "备份名称无效或与目标虚拟机不匹配"}, nil
	}
	if strings.TrimSpace(d.Config.BackupPath) == "" {
		return &model.ZMessage{Success: false, Action: "RMBackup",
			Message: "主机未配置备份路径"}, nil
	}
	path := filepath.ToSlash(filepath.Join(d.Config.BackupPath, name))
	d.execHost(fmt.Sprintf("rm -f \"%s\"", path), 5*time.Second)
	return &model.ZMessage{Success: true, Action: "RMBackup",
		Message: "备份已删除"}, nil
}

// Crontab 定时任务：同步所有 VM 的电源状态
func (d *SmolVMDriver) Crontab() bool {
	d.mu.RLock()
	snapshot := make(map[string]*model.VMConfig, len(d.VmSaving))
	for k, v := range d.VmSaving {
		snapshot[k] = v
	}
	d.mu.RUnlock()

	for uuid, cfg := range snapshot {
		state := d.GetPower(uuid)
		if newState, ok := PowerStatusMap[state]; ok {
			cfg.VmFlag = newState
		}
	}
	return true
}

// ReinstallVM 重装系统：停止VM，删除旧rootfs，重新构建，启动VM
func (d *SmolVMDriver) ReinstallVM(cfg *model.VMConfig) (*model.ZMessage, error) {
	vmUUID := cfg.VmUUID
	rootfs := filepath.ToSlash(filepath.Join(d.vmDir(vmUUID), "rootfs.ext4"))

	// 1) 停止虚拟机
	pid := d.readPid(vmUUID)
	if pid > 0 && d.pidAlive(pid) {
		d.PowerControl(vmUUID, model.VMPowerSClose)
		time.Sleep(2 * time.Second)
		// 强制停止
		d.killPid(pid, true)
		time.Sleep(1 * time.Second)
	}

	// 2) 删除旧 rootfs
	d.execHost(fmt.Sprintf("rm -f \"%s\"", rootfs), 5*time.Second)

	// 3) 重新构建 rootfs
	sizeMB := cfg.HddNum
	if sizeMB <= 0 {
		sizeMB = 8 * 1024
	}
	if err := d.buildRootfs(cfg.OsName, rootfs, sizeMB, cfg.OsPass); err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall",
			Message: fmt.Sprintf("rootfs 构建失败: %v", err)}, nil
	}

	// 4) 重新启动虚拟机
	d.execHost(fmt.Sprintf("rm -f \"%s\"", d.vmSock(vmUUID)), 5*time.Second)
	if _, err := d.PowerControl(vmUUID, model.VMPowerSStart); err != nil {
		return &model.ZMessage{Success: false, Action: "VMReinstall",
			Message: fmt.Sprintf("启动虚拟机失败: %v", err)}, nil
	}

	return &model.ZMessage{Success: true, Action: "VMReinstall",
		Message: "系统重装成功"}, nil
}
