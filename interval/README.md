# `interval/web` — argus-app 内部模块导览

包含 `argus-app` 的 HTTP/SSE 仪表板 + 全部业务逻辑。本文件说明每
个 Go 源文件的职责, 以及几个跨文件的不变量。

> 顶层 README 在 [`../../README.md`](../../README.md), 本文件给的是
> "我打算改这个模块, 应该读哪个文件" 的导航。

## 一、目录结构

```
interval/
├── README.md                 ← 你正在看的文档
└── web/
    ├── assets/               ← go:embed 进二进制的静态文件
    │   ├── dashboard.html    单文件 Web UI (vanilla JS + EventSource)
    │   ├── login.html        登录页 (强制改密流程)
    │   ├── favicon.ico
    │   └── favicon-256.png
    │
    ├── (HTTP 总线)
    ├── server.go             Options / Server struct / NewServer / 路由注册
    ├── auth.go               cookie session 中间件 + 登录/退出/改密
    ├── events.go             OnEvent / OnSyslog / SSE 流 / 离线缓存
    ├── devices.go            / + /favicon + /api/devices + capabilities
    │
    ├── (路由 handlers, 按业务域分组)
    ├── handlers_aliases.go   /api/aliases CRUD
    ├── handlers_settings.go  /api/settings + /api/holidays
    ├── handlers_worktime.go  /api/history + /api/worktime{,/month,/override}
    ├── handlers_notify.go    /api/notifications{,/test,/messages}
    ├── handlers_version.go   /api/version{,/check} + /api/upgrade
    ├── backup_handlers.go    /api/backup/{export,import}
    ├── kick.go               /api/devices/kick + handler
    ├── system.go             /api/system/{reboot,restart-network}
    │
    ├── (持久化 store, 按 JSON 文件一对一)
    ├── aliases.go            aliases.json — MAC → 友好名
    ├── settings.go           settings.json — 打卡设备 + 工时 + 全局 webhook + 钉钉关键词
    ├── overrides.go          overrides.json — (alias, date) 手动 in/out 覆写, 按月嵌套
    ├── notify.go             notifications.json — per-device webhook + ntfy 配置 + 派发器
    ├── credentials.go        credentials.json — bcrypt 哈希 + 用户名 (0600)
    ├── holidays.go           holidays.json + holidays_system.json — 节假日双层存储
    ├── history.go            history/<mac>.jsonl — 上下线流水 + 工时计算核心
    │
    ├── (业务逻辑 / 系统集成)
    ├── notify_dispatch.go    dispatchNotify + 打卡分类 + 自动写下班时间 + markdown 渲染
    ├── version.go            GitHub Releases 探测 + 自升级触发
    ├── backup.go             /etc/argus-app tar.gz 打包 / 解包 / 原子切换
    ├── dhcp.go               OpenWrt uci 静态 IP 管理 + DHCP 重载链
    │
    └── *_test.go             单元测试
```

## 二、文件速查表

### HTTP 总线 (route plumbing, no business logic)

| 文件 | 作用 |
|---|---|
| `server.go` | `Server` struct + `Option` + `NewServer` (路由注册) + `ServeHTTP` + 共享小工具 (`writeJSONErr`, `normalizeMAC`, `nonZeroTime`, `dayKindFor`) + go:embed 三个静态文件 |
| `auth.go` | `requireAuth` 中间件 (cookie session, HTML→302 / API→401), `defaultLANAuth` (默认放行) + `defaultLANAuth1` (RFC1918+ULA 备查), `handleLogin` / `handleAPILogin` / `handleAPILogout` / `handleAPIPassword` |
| `events.go` | `OnEvent` (Watcher 事件入口 — 见下方"OnEvent 顺序"不变量), `OnSyslog` (syslog 提示缓存, 用于事件归因), `sourceFor`, `updateOfflineCache`, `handleEvents` (SSE), `Shutdown` |
| `devices.go` | `handleIndex` (no-cache + ETag, 升级后浏览器立刻拿新 HTML), `handleFavicon`, `deviceRow` 线协议结构, `applyAlias`, `handleDevices`, `capabilities` |

### 路由 handlers (CRUD + 业务出口)

| 文件 | 端点 | 备注 |
|---|---|---|
| `handlers_aliases.go` | `/api/aliases` | MAC 别名增删改查 |
| `handlers_settings.go` | `/api/settings` + `/api/holidays` | 打卡设备 / 工时 / 全局 webhook / 钉钉关键词 / 手动节假日 |
| `handlers_worktime.go` | `/api/history` + `/api/worktime{,/month,/override}` | 历史记录 + 单日 / 月度工时报告 + 手动 in/out 覆写 |
| `handlers_notify.go` | `/api/notifications{,/test,/messages}` | per-device webhook+ntfy 配置 + 合成事件触发 + ntfy 收件箱 |
| `handlers_version.go` | `/api/version{,/check}` + `/api/upgrade` | 版本徽章 + GitHub 探测 + 一键升级 |
| `backup_handlers.go` | `/api/backup/{export,import}` | tar.gz 打包下载 / 上传恢复 |
| `kick.go` | `/api/devices/kick` | 强制 deauth, 仅 WiFi |
| `system.go` | `/api/system/{reboot,restart-network}` | 重启路由器 / 重启网络 |

### 持久化 stores

每个 store 都遵循同样的契约:
- 单文件 JSON, 写入用 `tmp + rename` 原子化, 永不留半写状态
- `New*Store(path)` 构造, 空 path = in-memory (用于测试)
- 私有 `load()` 在构造时调用; **公开 `Reload()` 用于 backup import 后**
- 内部用 RWMutex, 所有方法 goroutine-safe

| 文件 | 文件路径 | 内容 |
|---|---|---|
| `aliases.go` | `aliases.json` | MAC → 友好名映射, MAC 大小写归一化 |
| `settings.go` | `settings.json` | 打卡设备集 + 工时窗口 + 全局 webhook URL + dingtalk 关键词; 兼容旧 `MeMAC` 字段, 启动时迁移到 `PunchMACs[]` |
| `overrides.go` | `overrides.json` | per-(alias, YYYY-MM-DD) 手动 in/out, **按月嵌套**结构, 兼容旧扁平结构启动时迁移 |
| `notify.go` | `notifications.json` (0600) | per-device webhook + ntfy 配置, 加上 `Notifier` 派发器 (HTTP POST + ntfy publish + ntfy subscribe → res 主题收件箱) |
| `credentials.go` | `credentials.json` (0600) | bcrypt 哈希 + 用户名 + must_change 标记; `SessionStore` 内存 token → 用户名 (24h TTL) |
| `holidays.go` | `holidays.json` + `holidays_system.json` | **双层**: 用户手动层 + timor.tech 自动层; 启动 + 每天 03:00 拉取; 查询优先级 手动 > 系统 > 周末判定 |
| `history.go` | `history/<mac>.jsonl` | append-only JSONL, **保留 30 天**, 自动滚动压缩; 工时计算核心 `ComputeWorktime` / `MonthlyReport` 也在这里 |

### 业务逻辑 / 系统集成

| 文件 | 作用 |
|---|---|
| `notify_dispatch.go` | **dispatchNotify** (webhook + ntfy 派发的所有路由决策), **classifyPunchEvent** (4 类: NotPunch / CheckIn / CheckOut / Transient), **recordPunchCheckout** (打卡设备下班时间自动写 overrides.out), **formatNotifyMarkdown** (上班了/下班了/上线啦/下线啦 4 种 body 模板), `appendWebhookKeyword`, `monthOvertimeSecs`, `humanDuration`, `sourceLabel` |
| `version.go` | `VersionService` (GitHub Releases API + 30min 缓存 + gh-proxy.com fallback), `HasUpdate` (semver 比较), `triggerUpgrade` (写 bootstrap 脚本, setsid 启动, install.sh 接管) |
| `backup.go` | `packDataDir` (tar+gz 流式打包), `importBackup` (multipart → 校验 manifest → staging → 原子 rename → 清理 .bak), zip-slip 防护, 大小上限, `restore_credentials=false` 时跳过 + 复制保留 |
| `dhcp.go` | `UCIDHCPManager` (OpenWrt `uci` + dnsmasq), 分配/修改/移除静态 IP, 「立即生效」 = 删 lease 文件 + ARP flush + per-station kick + (可选) wifi reload; **`staKickCmds` / `wifiRestartCmds` / `iwpriv DisConnectSta` 链同时被 kick.go 复用** |

## 三、跨文件不变量

读改这三个之前请务必看一眼对应代码 — 字段顺序错了会静默坏行为。

### 3.1 OnEvent 顺序 (`events.go:OnEvent`)

```
1. updateOfflineCache(e)          ← 离线设备进缓存, /api/devices 还能看见
2. history.Record(e, sourceFor)   ← 落 jsonl, 工时报告读这里
3. recordPunchCheckout(e)         ← 打卡设备下班时间写 overrides.out
4. dispatchNotify(e)              ← webhook + ntfy
5. SSE fan-out                    ← 通知 /api/events 订阅者
```

依赖关系:
- 步骤 4 (dispatchNotify) 内部会 query history (`classifyPunchEvent` 看
  当天有没有 prior ONLINE), 所以**必须在步骤 2 之后**
- 步骤 4 也读 syslog hint 缓存 (sourceLabel), 该缓存有 8s TTL,
  所以同步执行就好不要 spawn goroutine
- 步骤 1 优先于步骤 4, 否则 webhook 看到的设备状态会和 /api/devices 不一致

### 3.2 Webhook 路由 (`notify_dispatch.go:dispatchNotify`)

两层闸刀, 拒掉一切就直接 `return`:

```
notifier == nil                              → 整个特性关闭
event 不是 ONLINE/OFFLINE                    → 跳过
↓
classify (打卡设备) → CheckIn / CheckOut / Transient
↓
opt-in 闸刀: per-device 没配 webhook 也没配 ntfy → 跳过整个分支
              (邻居手机蹭网不会刷屏全局 webhook)
↓
全局 webhook (settings.GlobalWebhookURL)      ← 不论 cls 类型, 都推
                                                payload.scope = "global"
↓
cls == Transient                             → 不再走 per-device
↓
per-device webhook + ntfy                     payload.scope = "device"
```

打卡设备消息体:
- CheckIn / CheckOut → 重量 markdown ("上班了/下班了" + 工时统计)
- Transient → 轻量 ("上线啦/下线啦", 同普通设备)

### 3.3 Backup 导入后的 store reload (`backup_handlers.go:handleBackupImport`)

`importBackup` 是文件层面的原子 swap (rename live → .bak, rename
staging → live, RemoveAll .bak)。文件换了内容**之后**必须挨个调每
个 store 的 `Reload()`, 否则内存里还是旧数据:

```go
s.aliases.Reload()
s.settings.Reload()
s.notifyStore.Reload()        // 还要 s.refreshNotifySubs() 重建 ntfy 订阅
s.overrides.Reload()
s.holidays.Reload()
s.creds.Reload()              // 仅 restoreCreds=true; 顺带 RevokeAll() 踢出所有会话
```

`history` 是按文件懒加载的, 不需要 reload (下次 Query 自然读到新文件)。

## 四、测试

| 文件 | 覆盖 |
|---|---|
| `history_test.go` | `ComputeWorktime` 全部边界 (workday/weekend/holiday/makeup/otday + late/early/missing_in/missing_out) |
| `notify_test.go` | per-device 派发链路 (webhook + ntfy 各自的格式) |
| `credentials_test.go` | bcrypt + session TTL + RevokeAll |
| `version_test.go` | semver 比较 + GitHub API 缓存 + mirror fallback |
| `backup_test.go` | round-trip + 跳过凭据 + zip-slip 拒绝 + 缺/错 manifest |
| `punch_classify_test.go` | dispatchNotify 的打卡分类四象限 |
| `punch_checkout_test.go` | recordPunchCheckout 各分支 + last-write-wins |

跑全部:

```sh
go test ./interval/web/...
```

## 五、零依赖政策

模块**只依赖标准库 + golang.org/x/crypto + github.com/xxl6097/argusd**
(见根目录 `go.mod`)。新增功能时:
- 优先用 stdlib (`archive/tar`, `compress/gzip`, `crypto/sha256`, ...)
- 向 OpenWrt 路由器发请求时用 `os/exec` + `ubus` / `uci` / `iwpriv` 直接 shell out, 不引入 ubus Go binding
- 任何"装个 lib 一行就解决"的诱惑都先看下能不能 50 行 stdlib 搞定 — 路由器 ROM/RAM 紧, 二进制每多 1 MB 都要算一下

## 六、添加新功能的标准动作

1. **新 endpoint**: 在 `handlers_xxx.go` 加 handler, 在 `server.go:NewServer` 的 `s.mux.HandleFunc` 区块挂上 `gate(s.handleXxx)` (gate 即 requireAuth, 看 §3.2 看是否需要 `s.writeAuth`)
2. **新 store**: 新建 `xxx.go` 模仿 `aliases.go` 的模板 (path / data / load / Reload / 原子 persist), 在 `Server` struct 加字段, 在 `server.go` 加 `WithXxx` Option
3. **新事件类型 / 新 notify 通道**: 改 `notify_dispatch.go`, 多数情况下能在不改 OnEvent 顺序的前提下完成
4. **新外部命令**: 模仿 `dhcp.go` 的 `staKickCmds [][]string` 模式 — 优先级数组 + `exec.LookPath` + 短超时, 头一个返回 0 的命令获胜
5. 别忘了:
   - `interval/web/server.go:NewServer` 路由列表
   - `assets/dashboard.html` 把 capability 在前端检测出来
   - `cmd/app/main.go` 的 `flag.String(...)` 把新路径暴露成命令行参数
   - 写测试 (table-driven 优先)
