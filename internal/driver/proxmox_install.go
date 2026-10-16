package driver

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	proxmox "github.com/luthermonson/go-proxmox"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/model"
)

// ============================================================
// PVE 系统镜像安装（对应 Python ProxmoxQemu.VMSetups）
//
// 这是虚拟机能够启动的前提：PVE 的 qemu.create 只创建配置与空磁盘，
// 必须把系统模板镜像导入/复制为系统盘并挂载，虚拟机才有可启动的系统。
// ============================================================

const (
	// importdisk/cp 大镜像可能耗时较久
	pveInstallTimeout = 20 * time.Minute
	pveCmdTimeout     = 2 * time.Minute
)

// nodeShellResult 节点命令执行结果
type nodeShellResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// runOnNode 在 PVE 节点上执行命令（本机直接执行，远程走 SSH）
func (d *ProxmoxDriver) runOnNode(cmd string, timeout time.Duration) (*nodeShellResult, error) {
	if timeout <= 0 {
		timeout = pveCmdTimeout
	}
	if isLocalHost(d.Config.ServerAddr) {
		return d.runLocalShell(cmd, timeout)
	}

	res, err := RunSSHCommand(sshConfigFromHS(d.Config), cmd)
	if err != nil {
		return nil, err
	}
	return &nodeShellResult{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode}, nil
}

// runLocalShell 在宿主机本地执行 shell 命令
func (d *ProxmoxDriver) runLocalShell(cmd string, timeout time.Duration) (*nodeShellResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.CommandContext(ctx, "cmd", "/C", cmd)
	} else {
		c = exec.CommandContext(ctx, "sh", "-c", cmd)
	}

	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr

	err := c.Run()
	res := &nodeShellResult{
		Stdout: strings.TrimSpace(stdout.String()),
		Stderr: strings.TrimSpace(stderr.String()),
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res.ExitCode = ee.ExitCode()
			return res, nil
		}
		if ctx.Err() == context.DeadlineExceeded {
			return res, fmt.Errorf("命令执行超时（%s）", timeout)
		}
		return res, err
	}
	return res, nil
}

// nodeFileExists 判断节点上文件是否存在
func (d *ProxmoxDriver) nodeFileExists(remotePath string) bool {
	cmd := fmt.Sprintf("test -f %s && echo exists || echo not_exists", shellQuote(remotePath))
	res, err := d.runOnNode(cmd, 30*time.Second)
	if err != nil || res == nil {
		return false
	}
	return strings.TrimSpace(res.Stdout) == "exists"
}

// nodeStoragePath 查询 PVE 存储的物理路径（对应 Python 查询 storage 配置的 path 字段）
func (d *ProxmoxDriver) nodeStoragePath(storage string) string {
	client, err := d.apiConn()
	if err != nil {
		return defaultStoragePath(storage)
	}
	ctx := context.Background()

	// 1) 节点级存储信息：/nodes/{node}/storage/{storage} 通常带 path
	var nodeResp struct {
		Path string `json:"path"`
	}
	if err := client.Get(ctx, fmt.Sprintf("/nodes/%s/storage/%s", d.nodeName(), storage), &nodeResp); err == nil {
		if p := strings.TrimSpace(nodeResp.Path); p != "" {
			return p
		}
	}

	// 2) 回退：集群级存储配置 /storage/{storage}
	var globalResp struct {
		Path string `json:"path"`
	}
	if err := client.Get(ctx, fmt.Sprintf("/storage/%s", storage), &globalResp); err == nil {
		if p := strings.TrimSpace(globalResp.Path); p != "" {
			return p
		}
	}

	return defaultStoragePath(storage)
}

// defaultStoragePath 未从 API 取到路径时的默认回退（与 Python 一致）
func defaultStoragePath(storage string) string {
	if storage == "local" || storage == "" {
		return "/var/lib/vz"
	}
	return "/mnt/pve/" + storage
}

// nextDiskIndex 计算下一个可用的磁盘编号
//
// EFI 磁盘（efidisk0）会占用 disk-0，因此系统盘通常从 disk-1 开始。
func (d *ProxmoxDriver) nextDiskIndex(vm *proxmox.VirtualMachine) int {
	maxIdx := -1
	for _, s := range vmConfigStrings(vm) {
		for _, m := range diskIndexRe.FindAllStringSubmatch(s, -1) {
			if len(m) > 1 {
				if n, err := strconv.Atoi(m[1]); err == nil && n > maxIdx {
					maxIdx = n
				}
			}
		}
	}
	return maxIdx + 1
}

var diskIndexRe = regexp.MustCompile(`disk-(\d+)`)

// vmConfigStrings 汇总虚拟机配置中所有可能含磁盘引用的字符串
func vmConfigStrings(vm *proxmox.VirtualMachine) []string {
	if vm == nil || vm.VirtualMachineConfig == nil {
		return nil
	}
	vc := vm.VirtualMachineConfig

	out := []string{
		vc.IDE0, vc.IDE1, vc.IDE2, vc.IDE3,
		vc.SCSI0, vc.SCSI1, vc.SCSI2, vc.SCSI3, vc.SCSI4, vc.SCSI5,
		vc.SATA0, vc.SATA1, vc.SATA2, vc.SATA3, vc.SATA4, vc.SATA5,
		vc.VirtIO0, vc.VirtIO1, vc.VirtIO2, vc.VirtIO3,
		vc.EFIDisk0,
	}
	out = append(out, vc.Unused0, vc.Unused1, vc.Unused2, vc.Unused3, vc.Unused4,
		vc.Unused5, vc.Unused6, vc.Unused7, vc.Unused8, vc.Unused9)

	filtered := make([]string, 0, len(out))
	for _, s := range out {
		if strings.TrimSpace(s) != "" {
			filtered = append(filtered, s)
		}
	}
	return filtered
}

// findUnusedDisk 查找 importdisk/cp 之后处于 unused 状态的磁盘（返回 storage:volume 形式）
func (d *ProxmoxDriver) findUnusedDisk(vm *proxmox.VirtualMachine, storage string) string {
	if vm == nil || vm.VirtualMachineConfig == nil {
		return ""
	}
	vc := vm.VirtualMachineConfig
	unuseds := []string{
		vc.Unused0, vc.Unused1, vc.Unused2, vc.Unused3, vc.Unused4,
		vc.Unused5, vc.Unused6, vc.Unused7, vc.Unused8, vc.Unused9,
	}

	// 优先返回指定存储上的磁盘
	for _, u := range unuseds {
		if u == "" {
			continue
		}
		if storage != "" && strings.HasPrefix(u, storage+":") {
			return strings.SplitN(u, ",", 2)[0]
		}
	}
	// 回退：任意 unused 磁盘
	for _, u := range unuseds {
		if u != "" {
			return strings.SplitN(u, ",", 2)[0]
		}
	}
	return ""
}

// ============================================================
// 网卡配置 / 启动顺序（对应 Python net_conf / _proxmox_boot_order）
// ============================================================

// netConf 构建 PVE 网卡配置（net0/net1...）
//
// 必须携带 macaddr：DHCP 静态绑定与 ARP 绑定都以 MAC 为键，
// 若下发给 PVE 的网卡不带 MAC，平台侧会随机生成一个，
// 与路由器上绑定的 MAC 不一致，虚拟机将拿不到预期 IP。
func (d *ProxmoxDriver) netConf(cfg *model.VMConfig) map[string]string {
	result := make(map[string]string)
	if cfg == nil || len(cfg.NicAll) == 0 {
		return result
	}

	names := make([]string, 0, len(cfg.NicAll))
	for name := range cfg.NicAll {
		names = append(names, name)
	}
	sort.Strings(names)

	idx := 0
	for _, name := range names {
		nic := cfg.NicAll[name]

		bridge := ""
		switch nic.EffectiveNicType() {
		case "pub":
			bridge = strings.TrimSpace(d.Config.NetworkPub)
		default:
			bridge = strings.TrimSpace(d.Config.NetworkNat)
		}
		if bridge == "" {
			// 未配置对应网桥时回退默认网桥，避免虚拟机完全没有网卡
			bridge = "vmbr0"
		}

		conf := fmt.Sprintf("e1000e,bridge=%s", bridge)
		if mac := strings.TrimSpace(nic.MacAddr); mac != "" {
			conf += ",macaddr=" + mac
		}
		result[fmt.Sprintf("net%d", idx)] = conf
		idx++
	}
	return result
}

// proxmoxBootOrder 生成 PVE 启动顺序（如 order=ide0;scsi1;ide2）
func (d *ProxmoxDriver) proxmoxBootOrder(cfg *model.VMConfig) string {
	if cfg == nil || len(cfg.EfiAll) == 0 {
		return "order=ide0;ide2"
	}

	// efi_name -> PVE 设备名
	deviceMap := make(map[string]string)
	deviceMap[cfg.VmUUID] = "ide0" // 系统盘

	hddNames := make([]string, 0, len(cfg.HddAll))
	for name := range cfg.HddAll {
		hddNames = append(hddNames, name)
	}
	sort.Strings(hddNames)

	scsiIdx := 1
	for _, name := range hddNames {
		if cfg.HddAll[name].HddFlag == 0 {
			continue
		}
		deviceMap[cfg.VmUUID+"-"+name] = fmt.Sprintf("scsi%d", scsiIdx)
		scsiIdx++
	}

	isoNames := make([]string, 0, len(cfg.IsoAll))
	for name := range cfg.IsoAll {
		isoNames = append(isoNames, name)
	}
	sort.Strings(isoNames)

	ideIdx := 2
	for _, name := range isoNames {
		deviceMap[name] = fmt.Sprintf("ide%d", ideIdx)
		ideIdx++
	}

	ordered := make([]string, 0, len(deviceMap))
	seen := make(map[string]bool, len(deviceMap))
	for _, efi := range cfg.EfiAll {
		if dev, ok := deviceMap[efi.EfiName]; ok && !seen[dev] {
			ordered = append(ordered, dev)
			seen[dev] = true
		}
	}
	// 未在 efi_all 中的设备追加到末尾（按设备名排序保证稳定）
	rest := make([]string, 0, len(deviceMap))
	for _, dev := range deviceMap {
		if !seen[dev] {
			rest = append(rest, dev)
		}
	}
	sort.Strings(rest)
	ordered = append(ordered, rest...)

	if len(ordered) == 0 {
		return "order=ide0;ide2"
	}
	return "order=" + strings.Join(ordered, ";")
}

// buildEfiList 构建默认启动项顺序（系统盘 -> 数据盘 -> 光盘）
func buildEfiList(cfg *model.VMConfig) []model.BootOpts {
	if cfg == nil {
		return nil
	}

	validHDD := map[string]bool{cfg.VmUUID: true}
	hddNames := make([]string, 0, len(cfg.HddAll))
	for name := range cfg.HddAll {
		hddNames = append(hddNames, name)
	}
	sort.Strings(hddNames)
	for _, name := range hddNames {
		if cfg.HddAll[name].HddFlag != 0 {
			validHDD[cfg.VmUUID+"-"+name] = true
		}
	}

	validISO := make(map[string]bool, len(cfg.IsoAll))
	for name := range cfg.IsoAll {
		validISO[name] = true
	}

	list := make([]model.BootOpts, 0, len(validHDD)+len(validISO))
	// 系统盘优先
	list = append(list, model.BootOpts{EfiType: false, EfiName: cfg.VmUUID})
	for _, name := range hddNames {
		full := cfg.VmUUID + "-" + name
		if validHDD[full] {
			list = append(list, model.BootOpts{EfiType: false, EfiName: full})
		}
	}

	isoNames := make([]string, 0, len(validISO))
	for name := range validISO {
		isoNames = append(isoNames, name)
	}
	sort.Strings(isoNames)
	for _, name := range isoNames {
		list = append(list, model.BootOpts{EfiType: true, EfiName: name})
	}
	return list
}

// ============================================================
// 系统安装主流程
// ============================================================

// installSystem 为虚拟机导入/复制系统镜像并挂载为系统盘
//
// 对应 Python ProxmoxQemu.VMSetups：
//  1. 判断 images_path 是 PVE 存储名（import 模式）还是物理路径（复制模式）
//  2. 通过 qm importdisk 或 cp 把模板镜像放到系统存储
//  3. 将生成的磁盘挂载到 ide0，并设置启动项
//  4. 按 hdd_num 扩容系统盘
func (d *ProxmoxDriver) installSystem(cfg *model.VMConfig) (*model.ZMessage, error) {
	if cfg == nil {
		return &model.ZMessage{Success: false, Action: "VInstall", Message: "虚拟机配置为空"}, nil
	}
	if strings.TrimSpace(cfg.OsName) == "" {
		return &model.ZMessage{Success: false, Action: "VInstall", Message: "未指定系统镜像"}, nil
	}
	imagesPath := strings.TrimSpace(d.Config.ImagesPath)
	if imagesPath == "" {
		return &model.ZMessage{Success: false, Action: "VInstall", Message: "未配置镜像路径"}, nil
	}

	vmid, err := d.getVMID(cfg.VmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VInstall", Message: err.Error()}, nil
	}
	vm, err := d.getVM(cfg.VmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VInstall", Message: err.Error()}, nil
	}

	systemStorage := strings.TrimSpace(d.Config.SystemPath)
	if systemStorage == "" {
		systemStorage = "local"
	}

	// 计算启动顺序（磁盘挂载后设置）
	if len(cfg.EfiAll) == 0 {
		cfg.EfiAll = buildEfiList(cfg)
	}
	bootOrder := d.proxmoxBootOrder(cfg)

	ctx := context.Background()

	// 判断模式：images_path 不含 "/" 视为 PVE 存储名
	if !strings.Contains(imagesPath, "/") {
		// ---------- import 模式 ----------
		storagePath := d.nodeStoragePath(imagesPath)
		srcAbs := path.Join(storagePath, "template", "import", cfg.OsName)
		srcAlt := path.Join(storagePath, "import", cfg.OsName)

		actualSrc := srcAbs
		if !d.nodeFileExists(actualSrc) {
			if d.nodeFileExists(srcAlt) {
				actualSrc = srcAlt
			} else {
				return &model.ZMessage{
					Success: false, Action: "VInstall",
					Message: fmt.Sprintf("镜像文件不存在: %s 或 %s", srcAbs, srcAlt),
				}, nil
			}
		}

		cmd := fmt.Sprintf("qm importdisk %d %s %s --format qcow2",
			vmid, shellQuote(actualSrc), shellQuote(systemStorage))
		res, err := d.runOnNode(cmd, pveInstallTimeout)
		if err != nil {
			return &model.ZMessage{Success: false, Action: "VInstall", Message: "importdisk 执行失败: " + err.Error()}, nil
		}
		if res.ExitCode != 0 {
			return &model.ZMessage{
				Success: false, Action: "VInstall",
				Message: fmt.Sprintf("importdisk 失败: %s", firstNonEmpty(res.Stderr, res.Stdout)),
			}, nil
		}
	} else {
		// ---------- 复制模式 ----------
		idx := d.nextDiskIndex(vm)
		if idx <= 0 {
			idx = 1
		}
		ext := path.Ext(cfg.OsName)
		if ext == "" {
			ext = ".qcow2"
		}
		diskName := fmt.Sprintf("vm-%d-disk-%d%s", vmid, idx, ext)
		diskDir := fmt.Sprintf("/var/lib/vz/images/%d", vmid)
		destImage := fmt.Sprintf("%s/%s", diskDir, diskName)

		srcFile := path.Join(imagesPath, cfg.OsName)
		if !d.nodeFileExists(srcFile) {
			return &model.ZMessage{
				Success: false, Action: "VInstall",
				Message: fmt.Sprintf("镜像文件不存在: %s", srcFile),
			}, nil
		}

		cmd := fmt.Sprintf("mkdir -p %s && cp %s %s",
			shellQuote(diskDir), shellQuote(srcFile), shellQuote(destImage))
		res, err := d.runOnNode(cmd, pveInstallTimeout)
		if err != nil {
			return &model.ZMessage{Success: false, Action: "VInstall", Message: "复制镜像失败: " + err.Error()}, nil
		}
		if res.ExitCode != 0 {
			return &model.ZMessage{
				Success: false, Action: "VInstall",
				Message: fmt.Sprintf("复制镜像失败: %s", firstNonEmpty(res.Stderr, res.Stdout)),
			}, nil
		}
	}

	// 重新拉取 VM 配置，让 PVE 识别新导入的磁盘（进入 unused 列表）
	vm, err = d.getVM(cfg.VmUUID)
	if err != nil {
		return &model.ZMessage{Success: false, Action: "VInstall", Message: "刷新虚拟机配置失败: " + err.Error()}, nil
	}

	actualDisk := d.findUnusedDisk(vm, systemStorage)
	if actualDisk == "" {
		return &model.ZMessage{
			Success: false, Action: "VInstall",
			Message: "未在 PVE 中找到导入后的磁盘（unused 为空），请检查存储配置与 qm 权限",
		}, nil
	}

	// 挂载系统盘到 ide0 并设置启动项
	opts := []proxmox.VirtualMachineOption{
		{Name: "ide0", Value: actualDisk},
		{Name: "boot", Value: bootOrder},
		{Name: "bootdisk", Value: "ide0"},
	}
	if _, err := vm.Config(ctx, opts...); err != nil {
		return &model.ZMessage{
			Success: false, Action: "VInstall",
			Message: fmt.Sprintf("挂载系统盘失败: %v", err),
		}, nil
	}

	// 按 hdd_num(MB) 扩容系统盘
	if cfg.HddNum > 0 {
		sizeGB := cfg.HddNum / 1024
		if cfg.HddNum%1024 != 0 {
			sizeGB++
		}
		if sizeGB < 1 {
			sizeGB = 1
		}
		if _, err := vm.ResizeDisk(ctx, "ide0", fmt.Sprintf("%dG", sizeGB)); err != nil {
			// 扩容失败不阻断安装（部分存储不支持在线扩容）
			// 注意：此处仅忽略错误，不回滚已完成的系统盘挂载
		}
	}

	return &model.ZMessage{Success: true, Action: "VInstall", Message: "系统安装成功"}, nil
}

// firstNonEmpty 返回第一个非空字符串
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// stringMapEqual 判断两个字符串 map 是否完全相等
func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}
