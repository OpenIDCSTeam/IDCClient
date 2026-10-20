#!/usr/bin/env bash
# ============================================================================
# OpenIDCS HostAgent (Go) Linux 一键安装脚本
#
# 在线安装（默认最新正式版）:
#   curl -fsSL https://raw.githubusercontent.com/OpenIDCSTeam/IDCClient/main/install.sh | sudo bash
#
# 指定版本 / 端口 / 目录:
#   sudo bash install.sh --version v1.0.0 --port 1881
#   sudo bash install.sh --version beta --dir /opt/openidcs
#
# 离线安装（使用 CI 产物 hostagent-linux-amd64.tar.gz）:
#   sudo bash install.sh --file ./hostagent-linux-amd64.tar.gz
#
# 卸载（默认保留 /var/lib/openidcs 数据）:
#   sudo bash install.sh --uninstall
#   sudo bash install.sh --uninstall --purge    # 连同数据一起删除
# ============================================================================
set -euo pipefail

# ======================== 配置 ========================
REPO="OpenIDCSTeam/IDCClient"
SERVICE_NAME="openidcs"
PREFIX="/opt/openidcs"
DATA_DIR="/var/lib/openidcs"
BIN_LINK="/usr/local/bin/hostagent"
UNIT_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
DEFAULT_PORT="1881"

PORT="${DEFAULT_PORT}"
VERSION="latest"
OFFLINE_FILE=""
DO_UNINSTALL=0
PURGE=0
OPEN_FIREWALL=0

# ======================== 输出 ========================
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; CYAN='\033[0;36m'; NC='\033[0m'
info() { echo -e "${GREEN}[INFO]${NC} $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }
die() { error "$*"; exit 1; }

# ======================== 参数 ========================
usage() {
    sed -n '2,20p' "$0" 2>/dev/null || true
    exit 0
}

while [[ $# -gt 0 ]]; do
    case "$1" in
        --version|-v)   VERSION="${2:?缺少版本号}"; shift 2 ;;
        --port|-p)      PORT="${2:?缺少端口}"; shift 2 ;;
        --dir)          PREFIX="${2:?缺少目录}"; shift 2 ;;
        --data-dir)     DATA_DIR="${2:?缺少目录}"; shift 2 ;;
        --file|-f)      OFFLINE_FILE="${2:?缺少文件路径}"; shift 2 ;;
        --firewall)     OPEN_FIREWALL=1; shift ;;
        --uninstall)    DO_UNINSTALL=1; shift ;;
        --purge)        PURGE=1; shift ;;
        --help|-h)      usage ;;
        *) die "未知参数: $1（--help 查看用法）" ;;
    esac
done

[[ $EUID -eq 0 ]] || die "请使用 root 权限运行，例如: sudo bash install.sh"

command -v systemctl >/dev/null 2>&1 || die "未检测到 systemd，本脚本仅支持 systemd 发行版"

detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64)   ARCH="amd64" ;;
        aarch64|arm64)  ARCH="arm64" ;;
        *) die "不支持的架构: $(uname -m)，仅提供 amd64/arm64" ;;
    esac
}

# ======================== 卸载 ========================
do_uninstall() {
    info "停止并移除服务 ${SERVICE_NAME} ..."
    systemctl stop "${SERVICE_NAME}" 2>/dev/null || true
    systemctl disable "${SERVICE_NAME}" 2>/dev/null || true
    rm -f "${UNIT_FILE}"
    systemctl daemon-reload
    systemctl reset-failed "${SERVICE_NAME}" 2>/dev/null || true

    rm -rf "${PREFIX}"
    rm -f "${BIN_LINK}"

    if [[ ${PURGE} -eq 1 ]]; then
        info "删除数据目录 ${DATA_DIR}"
        rm -rf "${DATA_DIR}"
    else
        info "已保留数据目录 ${DATA_DIR}（如需删除请加 --purge）"
    fi
    info "卸载完成"
}

# ======================== 下载 ========================
build_url() {
    local asset="hostagent-linux-${ARCH}.tar.gz"
    if [[ "${VERSION}" == "latest" || -z "${VERSION}" ]]; then
        echo "https://github.com/${REPO}/releases/latest/download/${asset}"
    else
        local tag="${VERSION}"
        [[ "${tag}" == v* ]] || tag="v${tag}"
        echo "https://github.com/${REPO}/releases/download/${tag}/${asset}"
    fi
}

download() {
    local url="$1" out="$2"
    if command -v curl >/dev/null 2>&1; then
        curl -fL --retry 3 --connect-timeout 15 --progress-bar -o "${out}" "${url}"
    elif command -v wget >/dev/null 2>&1; then
        wget -O "${out}" "${url}"
    else
        die "需要 curl 或 wget"
    fi
}

# ======================== 安装 ========================
do_install() {
    detect_arch
    info "系统架构: ${ARCH}   端口: ${PORT}   安装目录: ${PREFIX}   数据目录: ${DATA_DIR}"

    local tmp
    tmp="$(mktemp -d)"
    trap "rm -rf '${tmp}'" EXIT

    if [[ -n "${OFFLINE_FILE}" ]]; then
        [[ -f "${OFFLINE_FILE}" ]] || die "离线包不存在: ${OFFLINE_FILE}"
        cp -f "${OFFLINE_FILE}" "${tmp}/pkg.tar.gz"
    else
        local url
        url="$(build_url)"
        info "下载: ${url}"
        download "${url}" "${tmp}/pkg.tar.gz" || die "下载失败，请检查版本号或网络"
    fi

    info "解压安装包 ..."
    mkdir -p "${tmp}/pkg"
    tar -xzf "${tmp}/pkg.tar.gz" -C "${tmp}/pkg" || die "解压失败，文件可能损坏"
    [[ -f "${tmp}/pkg/hostagent" ]] || die "安装包内缺少 hostagent 可执行文件"
    [[ -f "${tmp}/pkg/static/index.html" ]] || die "安装包内缺少 static/index.html"

    # 升级场景先停服务，避免覆盖运行中的二进制
    if systemctl list-unit-files | grep -q "^${SERVICE_NAME}.service"; then
        info "检测到已安装版本，执行升级 ..."
        systemctl stop "${SERVICE_NAME}" 2>/dev/null || true
    fi

    info "安装程序文件到 ${PREFIX} ..."
    install -d -m 0755 "${PREFIX}"
    install -m 0755 "${tmp}/pkg/hostagent" "${PREFIX}/hostagent"
    rm -rf "${PREFIX}/static"
    cp -R "${tmp}/pkg/static" "${PREFIX}/static"
    install -d -m 0750 "${DATA_DIR}"
    ln -sfn "${PREFIX}/hostagent" "${BIN_LINK}"

    cat > "${PREFIX}/.install_info" <<EOF
INSTALL_VERSION=${VERSION}
INSTALL_ARCH=${ARCH}
INSTALL_DATE=$(date '+%Y-%m-%d %H:%M:%S')
INSTALL_PORT=${PORT}
INSTALL_DIR=${PREFIX}
DATA_DIR=${DATA_DIR}
EOF

    info "写入 systemd 服务 ..."
    cat > "${UNIT_FILE}" <<EOF
[Unit]
Description=OpenIDCS HostAgent (Go)
Documentation=https://github.com/${REPO}
After=network-online.target docker.service
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
WorkingDirectory=${PREFIX}
ExecStart=${PREFIX}/hostagent --port ${PORT} --data-dir ${DATA_DIR}
Restart=on-failure
RestartSec=5
StartLimitInterval=60
StartLimitBurst=5
LimitNOFILE=65536
Environment=LANG=en_US.UTF-8
StandardOutput=journal
StandardError=journal
SyslogIdentifier=${SERVICE_NAME}

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable "${SERVICE_NAME}" >/dev/null 2>&1 || true

    if [[ ${OPEN_FIREWALL} -eq 1 ]]; then
        open_firewall
    fi

    info "启动服务 ..."
    systemctl restart "${SERVICE_NAME}"
    sleep 3

    if ! systemctl is-active --quiet "${SERVICE_NAME}"; then
        error "服务启动失败，最近日志："
        journalctl -u "${SERVICE_NAME}" -n 40 --no-pager || true
        exit 1
    fi

    local ip token
    ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
    token="$(journalctl -u "${SERVICE_NAME}" -n 80 --no-pager 2>/dev/null \
        | grep -o 'Bearer Token: .*' | tail -1 | sed 's/Bearer Token: //' || true)"

    echo ""
    echo -e "${GREEN}OpenIDCS HostAgent 安装完成${NC}"
    echo -e "  安装目录: ${CYAN}${PREFIX}${NC}"
    echo -e "  数据目录: ${CYAN}${DATA_DIR}${NC}"
    echo -e "  访问地址: ${CYAN}http://${ip:-127.0.0.1}:${PORT}${NC}"
    [[ -n "${token}" ]] && echo -e "  初始 Token: ${CYAN}${token}${NC}"
    echo ""
    echo -e "常用命令: ${CYAN}systemctl {status|restart|stop} ${SERVICE_NAME}${NC}"
    echo -e "查看日志: ${CYAN}journalctl -u ${SERVICE_NAME} -f${NC}"
    echo -e "卸载:     ${CYAN}sudo bash install.sh --uninstall${NC}"
    echo ""
}

open_firewall() {
    if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
        info "放行端口 ${PORT}/tcp, 6080/tcp, 7681/tcp (firewalld)"
        firewall-cmd --permanent --add-port="${PORT}/tcp" >/dev/null
        firewall-cmd --permanent --add-port=6080/tcp >/dev/null
        firewall-cmd --permanent --add-port=7681/tcp >/dev/null
        firewall-cmd --reload >/dev/null
    elif command -v ufw >/dev/null 2>&1 && ufw status | grep -q "Status: active"; then
        info "放行端口 ${PORT}/tcp, 6080/tcp, 7681/tcp (ufw)"
        ufw allow "${PORT}/tcp" >/dev/null
        ufw allow 6080/tcp >/dev/null
        ufw allow 7681/tcp >/dev/null
    else
        warn "未检测到启用中的 firewalld/ufw，跳过放行"
    fi
}

# ======================== 主流程 ========================
if [[ ${DO_UNINSTALL} -eq 1 ]]; then
    do_uninstall
else
    do_install
fi
