#!/bin/sh
# argus-app installer for OpenWrt.
#
# 直接在路由器上执行：
#   wget -O- https://github.com/xxl6097/argus-app/releases/latest/download/install.sh | sh
#   curl -fsSL https://github.com/xxl6097/argus-app/releases/latest/download/install.sh | sh
#
# 国内访问 GitHub 慢/失败时，脚本会自动按 GH_MIRRORS 列表回退到加速镜像，
# 不需要任何配置。也可显式指定：
#   PROXY=https://ghproxy.net sh install.sh        # 强制走某个加速前缀
#   PROXY=none sh install.sh                       # 强制直连 GitHub
#   GH_MIRRORS="https://your.mirror" sh install.sh # 替换内置镜像列表
#
# 其他环境变量：
#   VERSION=v0.1.0     指定版本，默认拉 latest
#   ARCH=linux_arm64   手动指定架构，默认按 uname -m + 字节序自动识别
#   PORT=9099          Web UI 监听端口，默认 9099
#   FORCE=1            升级时强制覆盖 init 脚本（默认只换二进制）
set -eu

REPO="xxl6097/argus-app"
INSTALL_DIR="/usr/bin"
INIT_DIR="/etc/init.d"
DATA_DIR="/etc/argusd"
TMP_DIR="${TMPDIR:-/tmp}/argus-app-install.$$"

PORT="${PORT:-9099}"

# 内置 GitHub 加速镜像列表（顺序 = 优先级）。镜像可能临时下线，
# 列表外用户也可通过 GH_MIRRORS="https://x https://y" 覆盖。
GH_MIRRORS_DEFAULT="https://ghproxy.net https://gh-proxy.com https://mirror.ghproxy.com https://github.moeyy.xyz"
GH_MIRRORS="${GH_MIRRORS:-$GH_MIRRORS_DEFAULT}"

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
        mips|mips64)
            echo linux_mips_softfloat ;;
        mipsel|mips64el)
            echo linux_mipsle_softfloat ;;
        *)
            die "不支持的架构: $machine（可用 ARCH=linux_xxx 手动指定）" ;;
    esac
}

# ---------- 2. 下载工具 ----------
# 单次尝试：成功返回 0
try_dl() {
    url="$1"; dest="$2"; t="${3:-30}"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --connect-timeout 5 --max-time "$t" --retry 1 -o "$dest" "$url" 2>/dev/null
    elif command -v wget >/dev/null 2>&1; then
        wget -q -T "$t" --tries=1 -O "$dest" "$url" 2>/dev/null
    else
        die "需要 curl 或 wget"
    fi
}

# 带镜像回退的 fetch：URL 是逻辑 URL（github.com/... 或 raw.githubusercontent.com/...）
#   PROXY=none      → 只走直连
#   PROXY=<前缀>    → 只走该前缀
#   PROXY 未设置    → 先直连，失败再依次试 GH_MIRRORS
fetch() {
    raw="$1"; dest="$2"
    case "${PROXY:-}" in
        none|NONE|off|OFF)
            try_dl "$raw" "$dest" && return 0
            return 1 ;;
        ?*)
            try_dl "${PROXY%/}/$raw" "$dest" && return 0
            return 1 ;;
    esac
    # auto 模式
    if try_dl "$raw" "$dest" 12; then
        return 0
    fi
    warn "直连失败，尝试加速镜像..."
    for m in $GH_MIRRORS; do
        log "  → $m"
        if try_dl "${m%/}/$raw" "$dest"; then
            log "镜像 $m 可用，本次安装继续走该前缀"
            PROXY="$m"   # 后续请求复用同一镜像，避免每个文件都重探一遍
            return 0
        fi
    done
    return 1
}

# ---------- 3. 版本解析 ----------
# 用 GitHub API 拿 latest release 的 tag_name。比 HTML / 重定向探测稳:
# 响应几 KB JSON, 不依赖 busybox wget 的 redirect 行为。
# 顺序: 直连 api.github.com → gh-proxy.com 代理 → 用户给的 PROXY 前缀。
resolve_version() {
    if [ -n "${VERSION:-}" ]; then
        RESOLVED_VERSION="$VERSION"
        return
    fi
    json="$TMP_DIR/.latest.json"
    api="https://api.github.com/repos/${REPO}/releases/latest"
    candidates="$api"
    case "${PROXY:-}" in
        none|NONE|off|OFF) ;;
        ?*)               candidates="${PROXY%/}/$api $candidates" ;;
        *)                candidates="$candidates https://gh-proxy.com/$api" ;;
    esac
    for u in $candidates; do
        if try_dl "$u" "$json" 15 && [ -s "$json" ]; then
            RESOLVED_VERSION=$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$json" | head -n1)
            [ -n "$RESOLVED_VERSION" ] && return
        fi
    done
    die "无法解析最新版本号，请用 VERSION=vX.Y.Z 指定"
}

# ---------- 4. 主流程 ----------
# 类似 install -m 0755 SRC DST, 但只依赖 busybox 自带的 cp + chmod。
install_bin() {
    cp -f "$1" "$2" || die "拷贝失败: $1 → $2"
    chmod 0755 "$2" || die "chmod 失败: $2"
}

main() {
    [ "$(id -u)" = "0" ] || die "需要 root（直接 ssh 到路由器，或 sudo 执行）"
    need uname; need tar

    mkdir -p "$TMP_DIR"
    trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

    arch=$(detect_arch)
    resolve_version
    version="$RESOLVED_VERSION"
    pkg="argus-app_${version}_${arch}.tar.gz"
    base_url="https://github.com/${REPO}/releases/download/${version}"

    log "目标版本: $version"
    log "目标架构: $arch"
    log "下载文件: $pkg"
    if [ -n "${PROXY:-}" ] && [ "$PROXY" != "none" ]; then
        log "加速前缀: $PROXY"
    fi

    fetch "$base_url/$pkg" "$TMP_DIR/$pkg" || die "下载主包失败：$pkg"
    if fetch "$base_url/SHA256SUMS" "$TMP_DIR/SHA256SUMS"; then
        if command -v sha256sum >/dev/null 2>&1; then
            log "校验 SHA256..."
            ( cd "$TMP_DIR" && grep " $pkg\$" SHA256SUMS | sha256sum -c - ) \
                || die "SHA256 校验失败"
        fi
    else
        warn "无 SHA256SUMS（旧版本可能未发布），跳过校验"
    fi

    log "解压..."
    tar -xzf "$TMP_DIR/$pkg" -C "$TMP_DIR"
    src="$TMP_DIR/argus-app"
    [ -x "$src/argus-app" ] || die "压缩包内未找到可执行文件"

    if [ -x "$INIT_DIR/argus-app" ]; then
        log "停止已运行的实例..."
        "$INIT_DIR/argus-app" stop 2>/dev/null || true
        sleep 1
    fi

    log "安装二进制 → $INSTALL_DIR/argus-app"
    install_bin "$src/argus-app" "$INSTALL_DIR/argus-app"

    if [ ! -f "$INIT_DIR/argus-app" ] || [ "${FORCE:-0}" = "1" ]; then
        log "安装 init 脚本 → $INIT_DIR/argus-app"
        install_bin "$src/packaging/openwrt/argus-app.init" "$INIT_DIR/argus-app"
    else
        log "保留现有 init 脚本（FORCE=1 可强制覆盖）"
    fi

    mkdir -p "$DATA_DIR" "$DATA_DIR/history"

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
