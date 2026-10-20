#!/usr/bin/env bash
# ============================================================================
# OpenIDCS HostAgent (Go) macOS .pkg 构建脚本
#
# 前置: 需先构建 Darwin 二进制（CI 或本地）:
#   GOOS=darwin GOARCH=arm64 CGO_ENABLED=1 \
#     go build -trimpath -ldflags "-s -w" -o dist/hostagent-darwin-arm64 ./cmd/server
#
# 打包:
#   ./build-pkg.sh [版本号] [arm64|amd64]
#   ./build-pkg.sh 1.0.0 arm64
#
# 产物: BuildCache/OpenIDCS-HostAgent-<版本>-macos-<架构>.pkg
# 安装路径: /usr/local/openidcs，launchd 服务 com.openidcst.hostagent
# ============================================================================
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${HERE}/../.." && pwd)"

VERSION="${1:-0.0.0}"
VERSION="${VERSION#v}"

HOST_ARCH="$(uname -m)"
case "${HOST_ARCH}" in
    arm64|aarch64) DEFAULT_ARCH="arm64" ;;
    x86_64|amd64)  DEFAULT_ARCH="amd64" ;;
    *)             DEFAULT_ARCH="arm64" ;;
esac
ARCH="${2:-${DEFAULT_ARCH}}"
case "${ARCH}" in
    arm64|aarch64) ARCH="arm64"; PKG_ARCH="arm64" ;;
    amd64|x86_64)  ARCH="amd64"; PKG_ARCH="x86_64" ;;
    *) echo "不支持的架构: ${ARCH}" >&2; exit 1 ;;
esac

BINARY="${BINARY:-${ROOT}/dist/hostagent-darwin-${ARCH}}"
WORK="${ROOT}/BuildCache/macos"
STAGE="${WORK}/root"
SCRIPTS="${HERE}/scripts"

[[ -f "${BINARY}" ]] || { echo "找不到二进制: ${BINARY}" >&2; exit 1; }
[[ -d "${ROOT}/static" ]] || { echo "找不到 static 目录: ${ROOT}/static" >&2; exit 1; }
command -v pkgbuild >/dev/null 2>&1 || { echo "pkgbuild 不可用（需要 macOS + Xcode Command Line Tools）" >&2; exit 1; }

echo "[1/4] 准备安装根目录 ..."
rm -rf "${WORK}"
mkdir -p "${STAGE}/usr/local/openidcs" "${STAGE}/Library/LaunchDaemons"

install -m 0755 "${BINARY}" "${STAGE}/usr/local/openidcs/hostagent"
cp -R "${ROOT}/static" "${STAGE}/usr/local/openidcs/static"
install -m 0644 "${HERE}/launchd/com.openidcst.hostagent.plist" \
    "${STAGE}/Library/LaunchDaemons/com.openidcst.hostagent.plist"

chmod +x "${SCRIPTS}/preinstall" "${SCRIPTS}/postinstall"

echo "[2/4] pkgbuild ..."
pkgbuild \
    --root "${STAGE}" \
    --scripts "${SCRIPTS}" \
    --identifier "com.openidcst.hostagent" \
    --version "${VERSION}" \
    --install-location "/" \
    "${WORK}/component.pkg"

echo "[3/4] productbuild ..."
OUT="${ROOT}/BuildCache/OpenIDCS-HostAgent-${VERSION}-macos-${PKG_ARCH}.pkg"
mkdir -p "$(dirname "${OUT}")"
productbuild --package "${WORK}/component.pkg" "${OUT}"

echo "[4/4] 完成: ${OUT}"
