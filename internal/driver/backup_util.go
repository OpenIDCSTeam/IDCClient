// Package driver 归档工具
// 备份优先调用外部 7z（与 Python 版 BasicServer.path_zip 行为一致），
// 未安装 7z 时回退到 Go 标准库 tar.gz，保证功能可用。
package driver

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/OpenIDCSTeam/HostAgent-Go/internal/security"
)

// archiveTimeout 打包/解包超时（大磁盘镜像可能耗时较长）
const archiveTimeout = 6 * time.Hour

// SevenZipPath 返回 7z 可执行文件路径（优先环境变量 SEVEN_ZIP_BIN），未找到返回空串
func SevenZipPath() string {
	if p := strings.TrimSpace(os.Getenv("SEVEN_ZIP_BIN")); p != "" {
		return p
	}
	for _, name := range []string{"7z", "7za", "7zr", "7z.exe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// isArchiveName 判断是否为受支持的备份文件名
func isArchiveName(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".7z") ||
		strings.HasSuffix(lower, ".tar.gz") ||
		strings.HasSuffix(lower, ".tgz")
}

// archiveDir 将 srcDir 打包为 dstBase（自动追加扩展名），返回实际生成的文件路径
func archiveDir(srcDir, dstBase string) (string, error) {
	if sz := SevenZipPath(); sz != "" {
		dst := dstBase + ".7z"
		out, _, err := security.SafeExecWithTimeout([]string{sz, "a", "-t7z", dst, srcDir}, archiveTimeout)
		if err != nil {
			return "", fmt.Errorf("7z压缩失败: %w (%s)", err, strings.TrimSpace(out))
		}
		if _, statErr := os.Stat(dst); statErr != nil {
			return "", fmt.Errorf("7z压缩未生成文件: %w", statErr)
		}
		return dst, nil
	}

	dst := dstBase + ".tar.gz"
	if err := tarGzDir(srcDir, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// extractArchive 解压归档到 dstDir（按扩展名选择 7z / tar.gz）
func extractArchive(archivePath, dstDir string) error {
	if strings.HasSuffix(strings.ToLower(archivePath), ".7z") {
		sz := SevenZipPath()
		if sz == "" {
			return fmt.Errorf("未找到7z可执行文件，无法解压 %s", filepath.Base(archivePath))
		}
		out, _, err := security.SafeExecWithTimeout(
			[]string{sz, "x", archivePath, "-o" + dstDir, "-y"}, archiveTimeout)
		if err != nil {
			return fmt.Errorf("7z解压失败: %w (%s)", err, strings.TrimSpace(out))
		}
		return nil
	}
	return untarGzDir(archivePath, dstDir)
}

// tarGzDir 使用标准库将目录打包为 tar.gz（归档根目录保留源目录名）
func tarGzDir(srcDir, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("创建归档文件失败: %w", err)
	}
	defer f.Close()

	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()

	base := filepath.Base(filepath.Clean(srcDir))
	return filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		name := base
		if rel != "." {
			name = filepath.Join(base, rel)
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(name)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
}

// untarGzDir 解压 tar.gz 到目标目录（含路径穿越防护）
func untarGzDir(src, dstDir string) error {
	f, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开归档文件失败: %w", err)
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("读取归档失败: %w", err)
	}
	defer gr.Close()

	cleanDst := filepath.Clean(dstDir)
	tr := tar.NewReader(gr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("读取归档条目失败: %w", err)
		}

		target := filepath.Clean(filepath.Join(dstDir, filepath.FromSlash(hdr.Name)))
		if target != cleanDst && !strings.HasPrefix(target, cleanDst+string(os.PathSeparator)) {
			return fmt.Errorf("非法的归档路径: %s", hdr.Name)
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&0777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
	return nil
}
