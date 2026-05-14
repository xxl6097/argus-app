#!/bin/sh
# argus-app installer for OpenWrt.
#
# 直接在路由器上执行：
#   wget -O- https://github.com/xxl6097/argus-app/releases/latest/download/install.sh | sh
#   # 或:
#   curl -fsSL https://github.com/xxl6097/argus-app/releases/latest/download/install.sh | sh
#
# 环境变量覆盖：
#   VERSION=v0.1.0     指定版本，默认拉 latest
#   ARCH=linux_arm64   手动指定架构，默认按 uname -m + 字节序自动识别
#   PORT=9099          Web UI 监听端口，默认 9099
#   PROXY=...          下载前缀代理（如 https://ghproxy.com/），默认空
#   FORCE=1            已安装时强制覆盖（默认升级时只替换二进制）
set -eu

REPO="xxl6097/argus-app"
INSTALL_DIR="/usr/bin"
INIT_DIR="/etc/init.d"
DATA_DIR="/etc/argusd"
TMP_DIR="${TMPDIR:-/tmp}/argus-app-install.$$"

PORT="${PORT:-9099}"
PROXY="${PROXY:-}"

log()  { printf '\033[1;36m[argus-app]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[argus-app]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[argus-app]\033[0m %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令: $1"; }

# ---------- 1. 架构识别 ----------
detect_arch() {
    if [ -n "${ARCH:-}" ]; then
        echo "$ARCH"
        return
    fi
    machine=$(uname -m)
    case "$machine" in
        aarch64|arm64)
            echo linux_arm64 ;;
        armv7l|armv7|armv6l)
            echo linux_armv7 ;;
        x86_64|amd64)
            echo linux_amd64 ;;
        mips|mipsel|mips64|mips64el)
            # ELF e_ident[EI_DATA] (offset 5): 1=LSB(le), 2=MSB(be)
            target=/bin/busybox
            [ -r "$target" ] || target=/bin/sh
            byte=$(od -An -N1 -j5 -tu1 "$target" 2>/dev/null | tr -d ' \n' || true)
            if [ "$byte" = "1" ]; then
                echo linux_mipsle_softfloat
            else
                echo linux_mips_softfloat
            fi ;;
        *)
            die "不支持的架构: $machine（可用 ARCH=linux_xxx 手动指定）" ;;
    esac
}

# ---------- 2. 下载工具 ----------
fetch() {
    url="$1"; dest="$2"
    [ -n "$PROXY" ] && url="${PROXY%/}/$url"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --retry 3 -o "$dest" "$url"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O "$dest" "$url"
    else
        die "需要 curl 或 wget"
    fi
}

# ---------- 3. 版本解析 ----------
resolve_version() {
    if [ -n "${VERSION:-}" ]; then
        echo "$VERSION"
        return
    fi
    # latest 重定向
    base="https://github.com/${REPO}/releases/latest"
    [ -n "$PROXY" ] && base="${PROXY%/}/$base"
    if command -v curl >/dev/null 2>&1; then
        url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$base" 2>/dev/null || true)
    else
        url=$(wget --max-redirect=10 --spider -S "$base" 2>&1 | awk '/Location:/{u=$2} END{print u}')
    fi
    tag=$(echo "$url" | sed -n 's@.*/tag/\(v[^/]*\).*@\1@p')
    [ -n "$tag" ] || die "无法解析最新版本号，请用 VERSION=vX.Y.Z 指定"
    echo "$tag"
}

# ---------- 4. 主流程 ----------
main() {
    [ "$(id -u)" = "0" ] || die "需要 root（直接 ssh 到路由器，或 sudo 执行）"
    need uname; need tar; need od

    arch=$(detect_arch)
    version=$(resolve_version)
    pkg="argus-app_${version}_${arch}.tar.gz"
    base_url="https://github.com/${REPO}/releases/download/${version}"

    log "目标版本: $version"
    log "目标架构: $arch"
    log "下载文件: $pkg"

    mkdir -p "$TMP_DIR"
    trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

    # 下载 + 校验
    fetch "$base_url/$pkg"            "$TMP_DIR/$pkg"
    fetch "$base_url/SHA256SUMS"      "$TMP_DIR/SHA256SUMS" || warn "无 SHA256SUMS，跳过校验"
    if [ -s "$TMP_DIR/SHA256SUMS" ] && command -v sha256sum >/dev/null 2>&1; then
        log "校验 SHA256..."
        ( cd "$TMP_DIR" && grep " $pkg\$" SHA256SUMS | sha256sum -c - ) \
            || die "SHA256 校验失败"
    fi

    log "解压..."
    tar -xzf "$TMP_DIR/$pkg" -C "$TMP_DIR"
    src="$TMP_DIR/argus-app"
    [ -x "$src/argus-app" ] || die "压缩包内未找到可执行文件"

    # 已安装则停服务
    if [ -x "$INIT_DIR/argus-app" ]; then
        log "停止已运行的实例..."
        "$INIT_DIR/argus-app" stop 2>/dev/null || true
        sleep 1
    fi

    # 安装
    log "安装二进制 → $INSTALL_DIR/argus-app"
    install -m 0755 "$src/argus-app" "$INSTALL_DIR/argus-app"

    if [ ! -f "$INIT_DIR/argus-app" ] || [ "${FORCE:-0}" = "1" ]; then
        log "安装 init 脚本 → $INIT_DIR/argus-app"
        install -m 0755 "$src/packaging/openwrt/argus-app.init" "$INIT_DIR/argus-app"
    else
        log "保留现有 init 脚本（FORCE=1 可强制覆盖）"
    fi

    mkdir -p "$DATA_DIR" "$DATA_DIR/history"

    # 启动
    log "启用开机自启 + 启动服务..."
    "$INIT_DIR/argus-app" enable
    LISTEN="0.0.0.0:${PORT}" "$INIT_DIR/argus-app" start

    sleep 2
    if pidof argus-app >/dev/null 2>&1; then
        ip=$(uci -q get network.lan.ipaddr 2>/dev/null \
             || ip -4 -o addr show 2>/dev/null | awk '$2!="lo"{split($4,a,"/"); print a[1]; exit}')
        [ -n "$ip" ] || ip="<router-ip>"
        log "启动成功 ✓  浏览器访问  http://${ip}:${PORT}/"
        log "查看日志:  logread -f | grep argus-app"
        log "停止服务:  /etc/init.d/argus-app stop"
    else
        warn "进程未起来，看一下日志：logread | grep argus-app | tail -30"
        exit 1
    fi
}

main "$@"
