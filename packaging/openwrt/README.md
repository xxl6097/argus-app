# OpenWrt 安装指南

把 argus-app 装成系统服务,开机自启、异常自动重启、日志并入路由器 syslog。

## 前置要求

* OpenWrt 21.02+（带 `procd`）
* 架构：aarch64 / armv7 / mips —— 与 Release 页预编译二进制一致
* 路由器能访问互联网（需要拉取节假日数据）；离线场景见下文"离线模式"

## 步骤

### 1. 下载二进制

从 [Releases](https://github.com/xxl6097/argus-app/releases) 下载对应架构的
压缩包，或本地编译后 scp 上传：

```bash
scp argus-app_linux_arm64 root@192.168.1.1:/usr/bin/argus-app
scp packaging/openwrt/argus-app.init root@192.168.1.1:/etc/init.d/argus-app
```

### 2. 配置权限

```bash
ssh root@192.168.1.1 'chmod +x /usr/bin/argus-app /etc/init.d/argus-app'
```

### 3. 启用并启动

```bash
ssh root@192.168.1.1 '
    /etc/init.d/argus-app enable
    /etc/init.d/argus-app start
    /etc/init.d/argus-app status
'
```

打开浏览器访问 `http://192.168.1.1:9099` 即可。

### 4. 查看日志

argus-app 的 stdout / stderr 走的是 procd,全部流进路由器系统日志:

```bash
ssh root@192.168.1.1 'logread -e argus-app -f'
```

## 管理命令

| 命令 | 作用 |
|---|---|
| `/etc/init.d/argus-app start` | 启动 |
| `/etc/init.d/argus-app stop` | 停止 |
| `/etc/init.d/argus-app restart` | 重启 |
| `/etc/init.d/argus-app reload` | 发 SIGHUP 到 Watcher(保留 known/cooldown 状态) |
| `/etc/init.d/argus-app status` | 查看运行状态 |
| `/etc/init.d/argus-app enable` | 开机自启 |
| `/etc/init.d/argus-app disable` | 关闭开机自启 |

## 自定义监听地址 / 配置目录

默认 init 脚本读以下环境变量(在 `/etc/init.d/argus-app` 顶部可改):

```sh
LISTEN="${LISTEN:-0.0.0.0:9099}"
CONFIG_DIR="${CONFIG_DIR:-/etc/argus-app}"
```

只想暴露给内网回环,把 `LISTEN` 改成 `127.0.0.1:9099` 再反代(nginx/caddy)。

## 离线模式

如果路由器 WAN 口没通公网、或不想访问 timor.tech,两种选择:

1. **彻底关闭自动拉取**:编辑 init 脚本,把 `-holidays-system` 那行整行删掉 +
   把 `-holidays` 改成一个你手动维护的文件
2. **只关自动拉取但保留手动条目**: 传 `-holidays-system=""`

仪表板里的"设为工作日/节假日/加班日"按钮始终可用,这些都走手动层。

## 卸载

```bash
ssh root@192.168.1.1 '
    /etc/init.d/argus-app stop
    /etc/init.d/argus-app disable
    rm -f /usr/bin/argus-app /etc/init.d/argus-app
    # 以下目录保存了你的考勤数据,按需选择保留或删除:
    # rm -rf /etc/argus-app
'
```

## 故障排查

**路由器启动后 argus-app 没跑起来**
```bash
logread -e argus-app | head -30
```

**端口占用**:默认 9099 被其他程序占了:
```bash
netstat -lntp | grep 9099
# 改 init 里的 LISTEN 环境变量或参数,换成 9098 等
```

**配置文件写失败**:
```bash
ls -la /etc/argus-app/
# 确保 /etc/argus-app 目录存在且 root 可写
```

**想看 notifier 发送详情**:
```bash
logread -e 'notifier:' -f
```
