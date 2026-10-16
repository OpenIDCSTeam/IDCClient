// Package driver 截图能力实现。
//
// 与 Python 版各平台 VMScreen 对齐，统一返回 "data:image/png;base64,..."：
//   - QEMU      : virsh screenshot 生成 PPM → 转 PNG
//   - VirtualBox: VBoxManage controlvm <vm> screenshotpng
//   - MEmu      : adb shell screencap + adb pull
//
// 容器类平台（LXD / Docker / SmolVM）在 Python 版中同样走基类返回空，
// 因此这里保持基类行为（空字符串），不做伪实现。
package driver

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/security"
)

// readFileAsDataURL 读取本地图片文件并转成 data URL
func readFileAsDataURL(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", fmt.Errorf("截图为空")
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(data), nil
}

// ppmToDataURL 把 P6 格式的 PPM 图像转换为 PNG data URL。
//
// Go 标准库没有 PPM 解码器，而 QEMU 的 virsh screenshot 只输出 PPM，
// 因此这里手写最小 P6 解析：跳过注释 → 读取宽高与最大灰度 → 读原始 RGB → 编码 PNG。
func ppmToDataURL(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	reader := bufio.NewReader(bytes.NewReader(raw))

	token := func() (string, error) {
		for {
			b, err := reader.ReadByte()
			if err != nil {
				return "", err
			}
			if b == '#' { // 注释行
				if _, err := reader.ReadString('\n'); err != nil {
					return "", err
				}
				continue
			}
			if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
				continue
			}
			buf := []byte{b}
			for {
				nb, err := reader.ReadByte()
				if err != nil {
					return string(buf), nil
				}
				if nb == ' ' || nb == '\t' || nb == '\n' || nb == '\r' {
					return string(buf), nil
				}
				buf = append(buf, nb)
			}
		}
	}

	magic, err := token()
	if err != nil {
		return "", fmt.Errorf("读取PPM头失败: %w", err)
	}
	if magic != "P6" {
		return "", fmt.Errorf("不支持的PPM格式: %s", magic)
	}
	wStr, err := token()
	if err != nil {
		return "", err
	}
	hStr, err := token()
	if err != nil {
		return "", err
	}
	maxStr, err := token()
	if err != nil {
		return "", err
	}

	width, err := strconv.Atoi(wStr)
	if err != nil || width <= 0 {
		return "", fmt.Errorf("PPM宽度非法: %s", wStr)
	}
	height, err := strconv.Atoi(hStr)
	if err != nil || height <= 0 {
		return "", fmt.Errorf("PPM高度非法: %s", hStr)
	}
	if maxStr != "255" {
		return "", fmt.Errorf("仅支持8位PPM，最大灰度=%s", maxStr)
	}

	// 头部结束后有且仅有一个空白字符，其后为像素数据
	headerEnd := len(raw) - reader.Buffered()
	if headerEnd < 0 || headerEnd >= len(raw) {
		return "", fmt.Errorf("PPM数据不完整")
	}
	pixels := raw[headerEnd+1:]
	need := width * height * 3
	if len(pixels) < need {
		return "", fmt.Errorf("PPM像素数据不足: 期望 %d 字节，实际 %d", need, len(pixels))
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := (y*width + x) * 3
			img.Set(x, y, color.RGBA{R: pixels[i], G: pixels[i+1], B: pixels[i+2], A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// ============================================================
// QEMU/KVM 截图
// ============================================================

// captureQEMUScreenshot 通过 virsh screenshot 截图并转换为 PNG data URL
func (d *QEMUDriver) captureQEMUScreenshot(vmUUID string) (string, error) {
	tmp := filepath.Join(os.TempDir(),
		fmt.Sprintf("hostagent-screen-%s-%d.ppm", vmUUID, time.Now().UnixNano()))
	defer os.Remove(tmp)

	if out, err := d.execVirsh("screenshot", vmUUID, tmp, "--screen", "0"); err != nil {
		return "", fmt.Errorf("virsh screenshot 失败: %v (%s)", err, strings.TrimSpace(out))
	}
	if _, err := os.Stat(tmp); err != nil {
		return "", fmt.Errorf("截图文件未生成")
	}
	return ppmToDataURL(tmp)
}

// ============================================================
// VirtualBox 截图
// ============================================================

// captureVirtualBoxScreenshot 通过 VBoxManage screenshotpng 截图
func (d *VirtualBoxDriver) captureVirtualBoxScreenshot(vmUUID string) (string, error) {
	tmp := filepath.Join(os.TempDir(),
		fmt.Sprintf("hostagent-screen-%s-%d.png", vmUUID, time.Now().UnixNano()))
	defer os.Remove(tmp)

	if out, err := d.execVBox("controlvm", vmUUID, "screenshotpng", tmp); err != nil {
		return "", fmt.Errorf("VBoxManage screenshotpng 失败: %v (%s)", err, strings.TrimSpace(out))
	}
	if _, err := os.Stat(tmp); err != nil {
		return "", fmt.Errorf("截图文件未生成")
	}
	return readFileAsDataURL(tmp)
}

// ============================================================
// MEmu Android 截图
// ============================================================

// captureMemuScreenshot 通过 adb screencap + pull 截图
func (d *MemuDriver) captureMemuScreenshot(vmIndex string) (string, error) {
	// 模拟器内部截图
	if out, err := d.adb(vmIndex, "shell", "screencap", "-p", "/sdcard/screen_cap.png"); err != nil {
		return "", fmt.Errorf("adb screencap 失败: %v (%s)", err, strings.TrimSpace(out))
	}
	time.Sleep(500 * time.Millisecond)

	tmp := filepath.Join(os.TempDir(),
		fmt.Sprintf("hostagent-screen-%s-%d.png", vmIndex, time.Now().UnixNano()))
	defer os.Remove(tmp)

	if out, err := d.adb(vmIndex, "pull", "/sdcard/screen_cap.png", tmp); err != nil {
		return "", fmt.Errorf("adb pull 失败: %v (%s)", err, strings.TrimSpace(out))
	}
	if _, err := os.Stat(tmp); err != nil {
		return "", fmt.Errorf("截图文件未生成")
	}
	return readFileAsDataURL(tmp)
}

// ============================================================
// Hyper-V 截图（WMI GetVirtualSystemThumbnailImage → 原始像素）
// ============================================================

// captureHyperVScreenshot 通过 Hyper-V WMI 缩略图接口获取屏幕画面。
//
// Hyper-V 没有直接的截图命令，官方途径是
// Msvm_VirtualSystemManagementService.GetVirtualSystemThumbnailImage，
// 返回 RGB565 像素（低字节序），需要手动还原为 PNG。
func (d *HyperVDriver) captureHyperVScreenshot(vmUUID string) (string, error) {
	const width, height = 640, 480
	script := fmt.Sprintf(`$ErrorActionPreference='Stop';
$ns='root\virtualization\v2';
$cs=Get-CimInstance -Namespace $ns -ClassName Msvm_ComputerSystem -Filter "Name='%s'";
if(-not $cs){ exit 0 }
$svc=Get-CimInstance -Namespace $ns -ClassName Msvm_VirtualSystemManagementService;
$r=$svc | Invoke-CimMethod -MethodName GetVirtualSystemThumbnailImage -Arguments @{ TargetSystem=$cs; WidthPixels=%d; HeightPixels=%d };
if($r.ImageData){ [Convert]::ToBase64String($r.ImageData) }`,
		psEscape(vmUUID), width, height)

	out, _, err := security.SafeExecWithTimeout(
		[]string{"powershell", "-NoProfile", "-NonInteractive", "-Command", script},
		40*time.Second)
	if err != nil {
		return "", fmt.Errorf("Hyper-V 缩略图获取失败: %w", err)
	}

	b64 := strings.TrimSpace(out)
	if b64 == "" {
		return "", fmt.Errorf("Hyper-V 未返回缩略图（虚拟机可能未运行）")
	}
	pix, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("缩略图数据解码失败: %w", err)
	}
	return rgb565ToDataURL(pix, width, height)
}

// rgb565ToDataURL 把 RGB565（低字节在前）像素缓冲编码为 PNG data URL
func rgb565ToDataURL(pix []byte, width, height int) (string, error) {
	need := width * height * 2
	if len(pix) < need {
		return "", fmt.Errorf("缩略图像素不足: 期望 %d，实际 %d", need, len(pix))
	}
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for i := 0; i < width*height; i++ {
		// Hyper-V 返回 16bpp RGB565，小端序
		v := uint16(pix[i*2]) | uint16(pix[i*2+1])<<8
		r := uint8((v >> 11) & 0x1F)
		g := uint8((v >> 5) & 0x3F)
		b := uint8(v & 0x1F)
		img.Set(i%width, i/width, color.RGBA{
			R: uint8(int(r) * 255 / 31),
			G: uint8(int(g) * 255 / 63),
			B: uint8(int(b) * 255 / 31),
			A: 255,
		})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
