#!/bin/sh
# argus-app installer for OpenWrt.
#
# 把 argus-app 注册为 OpenWrt procd 服务:
#   /usr/bin/argus-app                 — 二进制
#   /etc/init.d/argus-app              — procd init 脚本 (USE_PROCD=1)
#   /etc/rc.d/S95argus-app  → ../init.d/argus-app  — 开机自启软链
#   /etc/argusd/                       — 持久化数据 (aliases/settings/history...)
#
# 直接在路由器上执行：
#   wget -O- https://github.com/xxl6097/argus-app/releases/latest/download/install.sh | sh
#   curl -fsSL https://github.com/xxl6097/argus-app/releases/latest/download/install.sh | sh
#
# 国内访问 GitHub 慢/失败时, 脚本会自动按 GH_MIRRORS 列表回退到加速镜像。
# 也可显式指定:
#   PROXY=https://ghproxy.net sh install.sh        # 强制走某个加速前缀
#   PROXY=none sh install.sh                       # 强制直连 GitHub
#   GH_MIRRORS="https://your.mirror" sh install.sh # 替换内置镜像列表
#
# 其他环境变量:
#   VERSION=v0.1.0     指定版本, 默认拉 latest
#   ARCH=linux_arm64   手动指定架构, 默认按 uname -m 自动识别
#   PORT=9099          Web UI 监听端口, 持久化到 init 脚本默认值
#   FORCE=1            强制覆盖 init 脚本 (默认升级时只换二进制)
#   SKIP_OS_CHECK=1    跳过 OpenWrt / procd 健全性检查 (自担风险)
set -eu

REPO="xxl6097/argus-app"
INSTALL_DIR="/usr/bin"
INIT_DIR="/etc/init.d"
RC_DIR="/etc/rc.d"
DATA_DIR="/etc/argusd"
SVC_NAME="argus-app"
TMP_DIR="${TMPDIR:-/tmp}/argus-app-install.$$"

PORT="${PORT:-9099}"

# 内置 GitHub 加速镜像列表 (顺序 = 优先级)。可被 GH_MIRRORS 覆盖。
# 排序经验: gh-proxy.com 实测对 release 资源 + api.github.com 都稳定;
# ghproxy.net 间歇性 502; mirror.ghproxy.com / github.moeyy.xyz 偶尔超时。
GH_MIRRORS_DEFAULT="https://gh-proxy.com https://mirror.ghproxy.com https://ghproxy.net https://github.moeyy.xyz"
GH_MIRRORS="${GH_MIRRORS:-$GH_MIRRORS_DEFAULT}"

log()  { printf '\033[1;36m[argus-app]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[argus-app]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[argus-app]\033[0m %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令: $1"; }

# ---------- 0. 平台检查 ----------
# 这个脚本只针对 OpenWrt + procd 设计, 装到普通 Linux 上 init 脚本跑不起来。
check_openwrt() {
    [ "${SKIP_OS_CHECK:-0}" = "1" ] && return
    if [ ! -r /etc/openwrt_release ] && [ ! -r /etc/os-release ]; then
        warn "未检测到 /etc/openwrt_release, 可能不是 OpenWrt 系统"
    fi
    [ -d "$INIT_DIR" ] || die "$INIT_DIR 不存在, 本脚本仅支持 OpenWrt + procd"
    [ -r /etc/rc.common ] || warn "/etc/rc.common 缺失, init 脚本可能无法工作"
    # procd 的 ubus call service 可用性是好兆头, 但不强求。
    if [ -d "$INSTALL_DIR" ] && ! ( touch "$INSTALL_DIR/.argus-write-test.$$" 2>/dev/null && rm -f "$INSTALL_DIR/.argus-write-test.$$" ); then
        die "$INSTALL_DIR 不可写。如是只读 squashfs 固件, 请先 mount -o remount,rw / 或换 -overlay 路径"
    fi
}

# ---------- 1. 架构识别 ----------
detect_arch() {
    if [ -n "${ARCH:-}" ]; then
        echo "$ARCH"
        return
    fi
    machine=$(uname -m)
    case "$machine" in
        aarch64|arm64)         echo linux_arm64 ;;
        armv7l|armv7|armv6l)   echo linux_armv7 ;;
        x86_64|amd64)          echo linux_amd64 ;;
        mips|mips64)           echo linux_mips_softfloat ;;
        mipsel|mips64el)       echo linux_mipsle_softfloat ;;
        *) die "不支持的架构: $machine (可用 ARCH=linux_xxx 手动指定)" ;;
    esac
}

# ---------- 2. 下载工具 ----------
# 尽量贴 busybox: 不用长选项, 不用 --tries / --retry。
try_dl() {
    url="$1"; dest="$2"; t="${3:-30}"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL --connect-timeout 5 --max-time "$t" -o "$dest" "$url" 2>/dev/null
    elif command -v wget >/dev/null 2>&1; then
        wget -q -T "$t" -O "$dest" "$url" 2>/dev/null
    else
        die "需要 curl 或 wget"
    fi
}

# 带镜像回退的 fetch:
#   PROXY=none      → 只走直连
#   PROXY=<前缀>    → 只走该前缀
#   PROXY 未设置    → 先直连, 失败再依次试 GH_MIRRORS
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
    if try_dl "$raw" "$dest" 12; then
        return 0
    fi
    warn "直连失败, 尝试加速镜像..."
    for m in $GH_MIRRORS; do
        log "  → $m"
        if try_dl "${m%/}/$raw" "$dest"; then
            log "镜像 $m 可用, 本次安装继续走该前缀"
            PROXY="$m"
            return 0
        fi
    done
    return 1
}

# ---------- 3. 版本解析 (走 GitHub API, 镜像也代理 api) ----------
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
        ?*) candidates="${PROXY%/}/$api $candidates" ;;
        *)  candidates="$candidates https://gh-proxy.com/$api" ;;
    esac
    for u in $candidates; do
        if try_dl "$u" "$json" 15 && [ -s "$json" ]; then
            RESOLVED_VERSION=$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$json" | head -n1)
            [ -n "$RESOLVED_VERSION" ] && return
        fi
    done
    die "无法解析最新版本号, 请用 VERSION=vX.Y.Z 指定"
}

# ---------- 4. 工具 ----------
install_bin() {
    cp -f "$1" "$2" || die "拷贝失败: $1 → $2"
    chmod 0755 "$2" || die "chmod 失败: $2"
}

extract_tgz() {
    archive="$1"; dest="$2"
    if tar -xzf "$archive" -C "$dest" 2>/dev/null; then
        return 0
    fi
    if command -v gunzip >/dev/null 2>&1; then
        gunzip -c "$archive" | tar -xf - -C "$dest"
    else
        die "tar 不支持 gzip 且系统无 gunzip, 无法解压 $archive"
    fi
}

# 把用户指定的 PORT 持久化到 init 脚本的 LISTEN 默认值,
# 让开机自启走的端口跟手动安装时一致。保留 ${LISTEN:-...} 形式,
# 这样运行时仍可用 LISTEN=... /etc/init.d/argus-app start 临时覆盖。
persist_port() {
    init="$1"; port="$2"
    if grep -q '^LISTEN=' "$init"; then
        sed -i "s|^LISTEN=\".*\"|LISTEN=\"\${LISTEN:-0.0.0.0:${port}}\"|" "$init"
    fi
}

# 等进程起来 (procd 启动 + respawn 可能有几秒抖动)。
wait_for_proc() {
    name="$1"; tries=0
    while [ $tries -lt 6 ]; do
        if pidof "$name" >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
        tries=$((tries + 1))
    done
    return 1
}

# 验证 procd enable 真的生成了开机自启软链。
is_enabled() {
    ls "$RC_DIR"/S*"$SVC_NAME" >/dev/null 2>&1
}

# 取路由器 LAN IP 用于结束语提示。
router_lan_ip() {
    if command -v uci >/dev/null 2>&1; then
        ip=$(uci -q get network.lan.ipaddr 2>/dev/null || true)
        [ -n "$ip" ] && { echo "$ip"; return; }
    fi
    if command -v ip >/dev/null 2>&1; then
        ip=$(ip -4 -o addr show 2>/dev/null | awk '$2!="lo"{split($4,a,"/"); print a[1]; exit}')
        [ -n "$ip" ] && { echo "$ip"; return; }
    fi
    if command -v ifconfig >/dev/null 2>&1; then
        ip=$(ifconfig br-lan 2>/dev/null | awk '/inet (addr:)?/{ for(i=1;i<=NF;i++) if($i ~ /^(addr:)?[0-9]+\./){gsub(/^addr:/,"",$i); print $i; exit} }')
        [ -n "$ip" ] && { echo "$ip"; return; }
    fi
    echo "<router-ip>"
}

# ---------- 5. 主流程 ----------
main() {
    [ "$(id -u)" = "0" ] || die "需要 root (直接 ssh 到路由器, 或 sudo 执行)"
    need uname; need tar

    mkdir -p "$TMP_DIR"
    trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

    check_openwrt

    arch=$(detect_arch)
    resolve_version
    version="$RESOLVED_VERSION"
    pkg="argus-app_${version}_${arch}.tar.gz"
    base_url="https://github.com/${REPO}/releases/download/${version}"

    log "目标版本: $version"
    log "目标架构: $arch"
    log "下载文件: $pkg"
    [ -n "${PROXY:-}" ] && [ "$PROXY" != "none" ] && log "加速前缀: $PROXY"

    fetch "$base_url/$pkg" "$TMP_DIR/$pkg" || die "下载主包失败: $pkg"
    if fetch "$base_url/SHA256SUMS" "$TMP_DIR/SHA256SUMS"; then
        if command -v sha256sum >/dev/null 2>&1; then
            log "校验 SHA256..."
            expected=$(grep " $pkg\$" "$TMP_DIR/SHA256SUMS" | awk '{print $1}')
            [ -n "$expected" ] || die "SHA256SUMS 中找不到 $pkg 条目"
            actual=$(cd "$TMP_DIR" && sha256sum "$pkg" | awk '{print $1}')
            [ "$expected" = "$actual" ] || die "SHA256 不匹配 (expected=$expected got=$actual)"
        else
            warn "系统无 sha256sum, 跳过校验"
        fi
    else
        warn "无 SHA256SUMS (旧版本可能未发布), 跳过校验"
    fi

    log "解压..."
    extract_tgz "$TMP_DIR/$pkg" "$TMP_DIR"
    src="$TMP_DIR/argus-app"
    [ -x "$src/argus-app" ] || die "压缩包内未找到可执行文件"

    # 已是服务 → 平滑停服, 等老进程退出避免 ETXTBSY。
    if [ -x "$INIT_DIR/$SVC_NAME" ]; then
        log "停止已运行的实例..."
        "$INIT_DIR/$SVC_NAME" stop 2>/dev/null || true
        i=0
        while pidof argus-app >/dev/null 2>&1 && [ $i -lt 5 ]; do
            sleep 1; i=$((i + 1))
        done
    fi

    log "安装二进制 → $INSTALL_DIR/argus-app"
    install_bin "$src/argus-app" "$INSTALL_DIR/argus-app"

    if [ ! -f "$INIT_DIR/$SVC_NAME" ] || [ "${FORCE:-0}" = "1" ]; then
        log "安装 init 脚本 → $INIT_DIR/$SVC_NAME"
        install_bin "$src/packaging/openwrt/argus-app.init" "$INIT_DIR/$SVC_NAME"
    else
        log "保留现有 init 脚本 (FORCE=1 可强制覆盖)"
    fi

    # 端口持久化: 不论是否换 init 脚本, 都把默认 LISTEN 同步成用户给的 PORT。
    persist_port "$INIT_DIR/$SVC_NAME" "$PORT"

    mkdir -p "$DATA_DIR" "$DATA_DIR/history"

    # ---- procd 服务注册 ----
    log "注册为 OpenWrt 服务 (procd)..."
    "$INIT_DIR/$SVC_NAME" enable 2>/dev/null || true
    if is_enabled; then
        log "  开机自启: 已启用 ($(ls $RC_DIR/S*$SVC_NAME 2>/dev/null))"
    else
        warn "  开机自启: 未生效, 重启后服务不会自动起来 (检查 $INIT_DIR/$SVC_NAME 头部 START= 设置)"
    fi

    log "启动服务..."
    "$INIT_DIR/$SVC_NAME" start

    # ---- 启动验证 + 结束语 ----
    if wait_for_proc argus-app; then
        ip=$(router_lan_ip)
        pid=$(pidof argus-app 2>/dev/null || echo "?")
        printf '\n'
        log "✓ argus-app 已注册为 OpenWrt 服务并启动成功"
        printf '\n'
        printf '  服务名      : %s\n' "$SVC_NAME"
        printf '  PID         : %s\n' "$pid"
        printf '  监听地址    : 0.0.0.0:%s\n' "$PORT"
        printf '  数据目录    : %s\n' "$DATA_DIR"
        printf '  访问地址    : http://%s:%s/\n' "$ip" "$PORT"
        printf '\n'
        printf '  服务管理命令:\n'
        printf '    /etc/init.d/%s status      查看运行状态\n' "$SVC_NAME"
        printf '    /etc/init.d/%s restart     重启服务\n' "$SVC_NAME"
        printf '    /etc/init.d/%s reload      不停机重载 (SIGHUP)\n' "$SVC_NAME"
        printf '    /etc/init.d/%s stop        停止服务\n' "$SVC_NAME"
        printf '    /etc/init.d/%s disable     取消开机自启\n' "$SVC_NAME"
        printf '\n'
        printf '  日志查看:\n'
        printf '    logread -e %s             # 历史日志\n' "$SVC_NAME"
        printf '    logread -f -e %s          # 实时跟踪\n' "$SVC_NAME"
        printf '\n'
        printf '  完全卸载:\n'
        printf '    /etc/init.d/%s stop && /etc/init.d/%s disable\n' "$SVC_NAME" "$SVC_NAME"
        printf '    rm -f %s/argus-app %s/%s\n' "$INSTALL_DIR" "$INIT_DIR" "$SVC_NAME"
        printf '    rm -rf %s    # 含数据, 谨慎\n' "$DATA_DIR"
        printf '\n'
    else
        warn "进程未起来。诊断步骤:"
        warn "  1) logread -e $SVC_NAME | tail -30"
        warn "  2) $INSTALL_DIR/argus-app -listen 0.0.0.0:${PORT}    # 前台运行看错误"
        warn "  3) /etc/init.d/$SVC_NAME status"
        exit 1
    fi
}

main "$@"
