# OpenWrt 安装指南

把 argus-app 装成系统服务,开机自启、异常自动重启、日志并入路由器 syslog。

## 前置要求

- OpenWrt 21.02+（带 `procd`）
- 架构：aarch64 / armv7 / amd64 / mips / mipsle —— 与 Release 页预编译二进制一致
- 路由器能访问互联网（拉取节假日 + 在线检测升级）；离线场景见下文「离线模式」

## 安装

### 方式 0: 一键脚本 (推荐)

直接 SSH 到路由器执行:

```sh
wget -O- https://github.com/xxl6097/argus-app/releases/latest/download/install.sh | sh
```

或:

```sh
curl -fsSL https://github.com/xxl6097/argus-app/releases/latest/download/install.sh | sh
```

国内 GitHub 直连不通时用加速镜像 (任选一条, 安装脚本自己后续也会做镜像 fallback):

```sh
wget -O- https://gh-proxy.com/https://github.com/xxl6097/argus-app/releases/latest/download/install.sh | sh
wget -O- https://cdn.jsdelivr.net/gh/xxl6097/argus-app@main/install.sh | sh
```

常用环境变量:

```sh
PORT=18099 sh install.sh             # 改监听端口 (默认 9099)
VERSION=v0.1.27 sh install.sh        # 指定版本 (默认 latest)
PROXY=https://gh-proxy.com sh install.sh   # 强制走某个加速前缀
PROXY=none sh install.sh             # 强制只直连
FORCE=1 sh install.sh                # 强制覆盖已有 init 脚本
```

脚本完成后会自动: 校验 SHA256 → 安装到 `/usr/bin/argus-app` → 安装 `/etc/init.d/argus-app` → 创建 `/etc/argus-app/` 数据目录 → 启用开机自启 → 启动服务 → 打印访问 URL。

### 方式 1: 手动 scp (高级)

从 [Releases](https://github.com/xxl6097/argus-app/releases) 下载对应架构的压缩包,或本地编译后 scp 上传:

```bash
scp argus-app_linux_arm64 root@192.168.1.1:/usr/bin/argus-app
scp packaging/openwrt/argus-app.init root@192.168.1.1:/etc/init.d/argus-app
ssh root@192.168.1.1 '
    chmod +x /usr/bin/argus-app /etc/init.d/argus-app
    mkdir -p /etc/argus-app /etc/argus-app/history
    /etc/init.d/argus-app enable
    /etc/init.d/argus-app start
    /etc/init.d/argus-app status
'
```

打开浏览器访问 `http://<路由器 IP>:9099`,默认账号 `admin / admin`,登录后强制改密。

### 查看日志

argus-app 的 stdout / stderr 走的是 procd,全部流进路由器系统日志:

```bash
ssh root@192.168.1.1 'logread -e argus-app -f'    # 实时跟踪
ssh root@192.168.1.1 'logread -e argus-app | tail -30'  # 历史
ssh root@192.168.1.1 "logread -e 'notifier:' -f"  # 只看 webhook/ntfy 派发
```

## 管理命令

| 命令 | 作用 |
|---|---|
| `/etc/init.d/argus-app start` | 启动 |
| `/etc/init.d/argus-app stop` | 停止 |
| `/etc/init.d/argus-app restart` | 重启 |
| `/etc/init.d/argus-app reload` | 发 SIGHUP 到 Watcher (保留 known / cooldown 状态) |
| `/etc/init.d/argus-app status` | 查看运行状态 |
| `/etc/init.d/argus-app enable` | 开机自启 |
| `/etc/init.d/argus-app disable` | 关闭开机自启 |
| `argus-app -h` / `argus-app -help` / `argus-app help` | 打印中文帮助 (用法 + 安装/卸载 + 信号 + 完整选项) |
| `argus-app -v` / `argus-app -version` | 打印版本号 |

## 自定义监听地址 / 数据目录

默认 init 脚本读以下环境变量 (在 `/etc/init.d/argus-app` 顶部可改):

```sh
LISTEN="${LISTEN:-0.0.0.0:9099}"
CONFIG_DIR="${CONFIG_DIR:-/etc/argus-app}"
```

只想暴露给内网回环,把 `LISTEN` 改成 `127.0.0.1:9099` 再反代 (nginx / caddy)。

## 升级

**推荐: 仪表板一键升级** (无需 SSH)

仪表板右上角版本徽章会自动轮询 GitHub Releases,发现新版会变橙 + 加 🆙 标记。点击徽章 → 「立即升级」, 整个过程 30–60 秒, 期间页面短暂不可用, 完成后浏览器手动刷新即可。

**手动升级**

```bash
# 重新跑一键脚本即可,会保留旧数据 + 仅替换二进制
wget -O- https://github.com/xxl6097/argus-app/releases/latest/download/install.sh | sh

# 想强制重装 init 脚本:
FORCE=1 sh install.sh
```

## 备份与恢复

仪表板 ⚙ 设置 → 备份与恢复:
- 「📦 导出全部数据」 → 浏览器下载 `argus-app-backup-<时间戳>.tar.gz` (含全部 JSON + history/)
- 「📥 从备份恢复」 → 选 .tar.gz, 二级确认 (是否同时恢复账户/凭据), 服务端原子替换 `/etc/argus-app`

也可以从命令行手动备份:

```bash
ssh root@192.168.1.1 'tar czf - /etc/argus-app' > argus-backup-$(date +%Y%m%d).tar.gz
```

## 离线模式

如果路由器 WAN 口没通公网、或不想访问 timor.tech, 两种选择:

1. **彻底关闭自动拉取**: 编辑 init 脚本, 把 `-holidays-system` 那行整行删掉 + 把 `-holidays` 改成一个你手动维护的文件
2. **只关自动拉取但保留手动条目**: 传 `-holidays-system=""`

仪表板里的「设为工作日 / 节假日 / 加班日」按钮始终可用, 这些都走手动层。

在线升级也会失败 (`/api/version/check` 探测 GitHub), UI 上的版本徽章保持当前版, 不会刷红, 没有副作用。

## 卸载

**保留数据** (留下 `/etc/argus-app/` 方便以后恢复):

```bash
ssh root@192.168.1.1 '
    /etc/init.d/argus-app stop
    /etc/init.d/argus-app disable
    rm -f /usr/bin/argus-app /etc/init.d/argus-app
'
```

**彻底清除** (含数据, 谨慎):

```bash
ssh root@192.168.1.1 '
    /etc/init.d/argus-app stop
    /etc/init.d/argus-app disable
    rm -f /usr/bin/argus-app /etc/init.d/argus-app
    rm -rf /etc/argus-app
'
```

## 故障排查

**路由器启动后 argus-app 没跑起来**

```bash
logread -e argus-app | tail -30
```

常见原因: 端口被占用 / 架构不匹配 (`cannot execute binary file`) / `/etc/argus-app/` 不可写。

**端口占用** (默认 9099 被其他程序占了):

```bash
netstat -lntp | grep 9099
# 改 init 里的 LISTEN, 例如 18099 后 restart
```

**浏览器打不开 9099**

```bash
uci show firewall | grep input    # 防火墙是否拦截 LAN
```

**配置文件写失败**

```bash
ls -la /etc/argus-app/    # 目录是否存在且 root 可写
```

**忘记 admin 密码**

```bash
ssh root@192.168.1.1 'rm -f /etc/argus-app/credentials.json && /etc/init.d/argus-app restart'
# 重启后默认 admin/admin, 登录会要求重新改密
```

**钉钉/飞书 webhook 报 errcode 310000 关键词不匹配**

进 ⚙ 设置 → 全局通知 Webhook → 「钉钉关键词」 填上你在机器人后台设置的关键词, 保存即可。

**想看 notifier 发送详情**

```bash
logread -e 'notifier:' -f
```

## 服务文件清单

```
/usr/bin/argus-app                    可执行二进制
/etc/init.d/argus-app                 procd init 脚本
/etc/rc.d/S95argus-app  → ../init.d/argus-app    开机自启软链
/etc/argus-app/                       数据目录
├── aliases.json                          MAC → 别名
├── credentials.json (0600)               admin bcrypt 哈希
├── settings.json                         打卡设备 + 工时 + 全局 webhook + 钉钉关键词
├── notifications.json (0600)             per-device webhook + ntfy 配置
├── overrides.json                        手动 in/out 覆写 (按月嵌套)
├── holidays.json                         用户手动节假日
├── holidays_system.json                  自动拉取的节假日缓存
└── history/<mac>.jsonl                   per-MAC 上下线流水, 30 天保留
```
