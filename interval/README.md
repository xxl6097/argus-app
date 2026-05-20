# `interval/` — argus-app 内部模块导览

按职责分包管理。`web` 是组合层 (HTTP + SSE + 业务编排), 下面所有
`store/*` 是单文件 JSON 持久化, `owrt` 是 OpenWrt 系统集成,
`release` 是版本/备份, `util` 是无状态小工具。

> 顶层 README 在 [`../README.md`](../README.md), 本文件给的是
> "我打算改这个模块, 应该读哪个包" 的导航。

## 一、包拓扑

```
interval/
├── README.md                      ← 你正在看的文档
├── util/                          util.NormalizeMAC / util.NonZeroTime / util.ParseClock /
│                                  util.WriteJSONAtomic — 无依赖小工具
├── store/                         JSON 持久化 store, 一个文件一种数据
│   ├── alias/                     aliases.json — MAC → 友好名
│   ├── settings/                  settings.json — 打卡设备 + 工时 + 全局 webhook + 钉钉关键词
│   ├── override/                  overrides.json — (alias, date) 手动 in/out, 按月嵌套
│   ├── notify/                    notifications.json (0600) — per-device webhook + ntfy + Notifier 派发器
│   ├── credentials/               credentials.json (0600) — bcrypt 哈希 + SessionStore (内存 token)
│   ├── openid/                    openids.json (0600) — 免登录白名单
│   ├── holidays/                  holidays.json + holidays_system.json — 双层节假日 + DayKind
│   └── history/                   history/<mac>.jsonl — 上下线流水 + ComputeWorktime / MonthlyReport
├── owrt/                          OpenWrt 系统集成 — uci / iwpriv / wifi reload / reboot
│                                  UCIDHCPManager (静态 IP) + KickStation (deauth) + 重启网络
├── release/                       版本 + 备份
│                                  VersionService (GitHub Releases 探测) +
│                                  PackDataDir / ImportBackup (tar.gz)
└── web/                           HTTP 服务 + SSE + 业务编排
    ├── assets/                    go:embed: dashboard.html / login.html / favicon
    ├── server.go                  Options + Server struct + NewServer 路由注册
    ├── auth.go                    cookie session 中间件 + 登录/退出/改密
    ├── events.go                  OnEvent / OnSyslog / SSE 流 / 离线缓存
    ├── devices.go                 / + /favicon + /api/devices + capabilities
    ├── handlers_aliases.go        /api/aliases CRUD
    ├── handlers_settings.go       /api/settings + /api/holidays
    ├── handlers_worktime.go       /api/history + /api/worktime{,/month,/override}
    ├── handlers_notify.go         /api/notifications{,/test,/messages}
    ├── handlers_openid.go         /api/openids + /api/login/openid (免登录)
    ├── handlers_version.go        /api/version{,/check} + /api/upgrade
    ├── handlers_dhcp.go           /api/dhcp (handler; 核心在 owrt/dhcp.go)
    ├── handlers_kick.go           /api/devices/kick (handler; 核心在 owrt/kick.go)
    ├── handlers_system.go         /api/system/{reboot,restart-network} (handler; 核心在 owrt/system.go)
    ├── backup_handlers.go         /api/backup/{export,import} (handler; 核心在 release/backup.go)
    └── notify_dispatch.go         dispatchNotify + 打卡分类 + 自动写下班时间 + markdown 渲染
```

设计原则:
- **`store/*` 只管自己的 JSON 文件** — 每个 store 独立可 import, 没有循环依赖
- **`web` 是唯一引用所有 store 的包** — handler 调用 `alias.New(...)` / `settings.Store.Get()` 这种, 不反向
- **`owrt`/`release` 不依赖 `web` 也不依赖任何 store** — 想拆给 CLI 工具复用很容易
- **`util` 不依赖任何 interval/* 包** — 出现互相依赖立刻是设计 bug

## 二、依赖矩阵

| 包 | 依赖 | 被谁依赖 |
|---|---|---|
| `util` | (无 interval 依赖) | 几乎所有 |
| `store/alias` | util | `store/notify`, `store/override`, `web`, `cmd/app` |
| `store/settings` | util | `web`, `cmd/app` |
| `store/override` | util, `store/alias` | `web`, `store/history` (test only), `cmd/app` |
| `store/notify` | util, `store/alias` | `web`, `cmd/app` |
| `store/credentials` | (仅 stdlib + bcrypt) | `web`, `cmd/app` |
| `store/openid` | util | `web`, `cmd/app` |
| `store/holidays` | util | `web`, `store/history`, `cmd/app` |
| `store/history` | util, `store/holidays`, `store/override` | `web`, `cmd/app` |
| `owrt` | (仅 stdlib) | `web`, `cmd/app` |
| `release` | (仅 stdlib) | `web`, `cmd/app` |
| `web` | 全部 + `argusd` 库 | `cmd/app` |

`store/history` 依赖 `holidays.DayKind` (常量) + `override.Override` (struct) 是因为
`ComputeWorktime` 一并消化这两个上下文。这个反向不会引出循环 (holidays 和
override 都不引用 history)。

## 三、跨文件不变量

读改这三个之前请务必看一眼对应代码 — 字段顺序错了会静默坏行为。

### 3.1 OnEvent 顺序 (`web/events.go:OnEvent`)

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
- 步骤 4 也读 syslog hint 缓存, 该缓存有 8s TTL, 同步执行就好
- 步骤 1 优先于步骤 4, 否则 webhook 看到的设备状态会和 /api/devices 不一致

### 3.2 Webhook 路由 (`web/notify_dispatch.go:dispatchNotify`)

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
全局 webhook (settings.GlobalWebhookURL)      ← 都推, payload.scope = "global"
↓
cls == Transient                             → 不再走 per-device
↓
per-device webhook + ntfy                    payload.scope = "device"
```

打卡设备消息体:
- CheckIn / CheckOut → 重量 markdown ("上班了/下班了" + 工时统计)
- Transient → 轻量 ("上线啦/下线啦", 同普通设备)

### 3.3 Backup 导入后的 store reload (`web/backup_handlers.go:handleBackupImport`)

`release.ImportBackup` 是文件层面的原子 swap (rename live → .bak,
rename staging → live, RemoveAll .bak)。文件换了内容**之后**必须挨
个调每个 store 的 `Reload()`, 否则内存里还是旧数据:

```go
s.aliases.Reload()
s.settings.Reload()
s.notifyStore.Reload()        // 还要 s.refreshNotifySubs() 重建 ntfy 订阅
s.overrides.Reload()
s.holidays.Reload()
s.openids.Reload()
s.creds.Reload()              // 仅 restoreCreds=true; 顺带 RevokeAll() 踢出所有会话
```

`history` 是按文件懒加载的, 不需要 reload (下次 Query 自然读到新文件)。

## 四、HTTP API 一览

总体规则:
- 默认走 cookie session 鉴权 (`requireAuth`); 没挂 `credentials.Store` 时
  退化为透传, 接口直接对外可读。
- 标 ✏️ 的写接口在 session 基础上额外过 `writeAuth` (默认 noop, 旧版本是
  RFC1918/ULA LAN-only — 见 `WithWriteAuth`)。
- 对应 store 没挂上时返回 `503 service not available`; `/api/devices`
  响应里 `capabilities.<feature>` 字段告诉前端哪些可用。
- 错误一律 `{"error": "<msg>"}` + 适当的 4xx/5xx; 成功体在每节列出。
- 路径区分 trailing slash; `/api/notifications` 和
  `/api/notifications/messages` / `/api/notifications/test` 是三个独立
  endpoint, 在 mux 里就近匹配。

### 4.1 公开页面 + 静态资源 (无鉴权)

| Method | Path | 摘要 |
|---|---|---|
| GET  | `/login`              | 登录页 HTML; 已登录则 302 至 `?next` 或 `/` |
| POST | `/api/login`          | `{username, password}` → 写 session cookie; 默认密码会返回 `must_change: true` |
| POST | `/api/login/openid`   | `{openid}` → 命中白名单则发 admin session cookie; 否则 401 |
| GET  | `/api/login/openid?openid=XXX[&next=/path]` | 同上, 但 302 跳转 (扫码 / 微信菜单 / iframe 引导用); 不在白名单 → 302 至 `/login?error=invalid_openid` |
| POST | `/api/logout`         | 撤销当前 session, 清 cookie; 幂等 |
| GET  | `/favicon.ico`        | 嵌入图标 |
| GET  | `/assets/app.css`     | 嵌入样式表 (no-cache + ETag) |
| GET  | `/assets/app/<x>.js`  | ES Module 文件; 共用 `app/` 目录的一个 ETag |

### 4.2 仪表板 + 实时事件

| Method | Path | 摘要 |
|---|---|---|
| GET  | `/`                  | 仪表板 HTML; 经过品牌字串替换 (`brand.go`), ETag 折入品牌签名 |
| GET  | `/api/devices`       | 在线 + 离线缓存合并; 含 `capabilities` 字段 |
| GET  | `/api/events`        | SSE 流; 事件类型 `hello` / `ONLINE` / `OFFLINE` / `CHANGE` |
| ✏️ POST | `/api/devices/kick` | `{mac, restart_wifi?}` → 调 `owrt.KickStation` 强行踢一台 WiFi 设备; 有线设备会 400 |

`/api/devices` 响应:

```json
{
  "devices": [
    {"mac":"AA:BB:..","ip":"...","hostname":"...","vendor":"...","type":"...",
     "radio":"...","ssid":"...","channel":36,"rssi":-55,"wired":false,
     "last_seen_ms":1716206400000,"status":"online","alias":"小明的手机","is_me":true}
  ],
  "count": 12, "online": 10, "offline": 2,
  "capabilities": {"aliases":true,"dhcp":true,"history":true,"worktime":true,
                   "overrides":true,"settings":true,"holidays":true,"notifications":true}
}
```

`/api/devices/kick` 响应: `{ok, mac, kicked, iwpriv_kicked, wifi_restarted}`。

### 4.3 设备别名 + 静态 DHCP

依赖 `WithAliases` / `WithDHCPManager`。

| Method | Path | 入参 | 摘要 |
|---|---|---|---|
| GET    | `/api/aliases`               | —                       | `{aliases: {MAC: name, ...}}` |
| ✏️ POST   | `/api/aliases`               | `{mac, name}`           | 空 name = 删别名 |
| ✏️ DELETE | `/api/aliases?mac=XX`        | —                       | 等价于 POST 空 name |
| GET    | `/api/dhcp`                  | —                       | `{leases: {MAC: {mac,ip,name}}}` |
| ✏️ POST   | `/api/dhcp[?restart_wifi=1]` | `{mac, ip, name}`       | 冲突 IP 返回 409 + `owner_mac` |
| ✏️ POST   | `/api/dhcp?purge_argus=1`    | —                       | 清掉所有 `argus_-` 前缀的保留项 (恢复用); 仅 `UCIDHCPManager` |
| ✏️ DELETE | `/api/dhcp?mac=XX[&restart_wifi=1]` | —                | 删保留项 |

### 4.4 工时统计 (打卡设备)

依赖 `WithHistory` + `WithSettings`; override 端点额外要 `WithOverrides`。

| Method | Path | 入参 | 摘要 |
|---|---|---|---|
| GET    | `/api/history?mac=XX[&from=YYYY-MM-DD][&to=YYYY-MM-DD]` | — | 上下线流水, 默认最近 30 天 |
| GET    | `/api/worktime?mac=XX[&date=YYYY-MM-DD][&start=HH:MM][&end=HH:MM]` | — | 单日工时报告 (`history.WorktimeReport`) |
| GET    | `/api/worktime/month?mac=XX[&month=YYYY-MM][&start=HH:MM][&end=HH:MM]` | — | 月度聚合 (`history.MonthlyReport`); 跳过零出勤天, 当月只算到今天 |
| GET    | `/api/worktime/override?mac=XX&date=YYYY-MM-DD` | — | 查一条 (mac, date) 手动覆写 |
| ✏️ POST   | `/api/worktime/override`         | `{mac, date, in, out}` | 手动填补漏掉的上下班 |
| ✏️ DELETE | `/api/worktime/override?mac=XX&date=YYYY-MM-DD` | — | 删一条覆写 |

`history.Query` 的时间窗以本地时区为基准; `from`/`to` 都是 `YYYY-MM-DD`,
`to` 内部会 +1 天以包含整天。

### 4.5 通知 (webhook + ntfy)

依赖 `WithNotifications`。`/api/notifications/messages` 是 ntfy 的反向通道
收件箱 (`Notifier.Inbox`)。

| Method | Path | 入参 | 摘要 |
|---|---|---|---|
| GET    | `/api/notifications?mac=XX`        | —                              | `{mac, exists, config}` |
| ✏️ POST   | `/api/notifications`               | `{mac, webhook_url?, ntfy_*?}` | 写完自动 `refreshNotifySubs` |
| ✏️ DELETE | `/api/notifications?mac=XX`        | —                              | 同上 |
| GET    | `/api/notifications/messages?mac=XX` | —                            | `{mac, messages: [...]}` 倒序最多 100 条 |
| ✏️ POST   | `/api/notifications/test`          | `{mac, kind: "ONLINE"\|"OFFLINE"}` | 合成事件走 `dispatchNotify`, 用来验证 webhook 文案 |

### 4.6 全局设置 + 节假日

依赖 `WithSettings` / `WithHolidays`。

| Method | Path | 入参 | 摘要 |
|---|---|---|---|
| GET    | `/api/settings`                | — | 见下方响应 shape |
| ✏️ POST   | `/api/settings`                | 见下 | 字段都是 optional, 只发哪个改哪个 |
| ✏️ DELETE | `/api/settings?mac=XX`         | — | 移除单个 punch_mac |
| ✏️ DELETE | `/api/settings?clear=punch`    | — | (或 `clear=me`) 清空打卡设备集合 |
| GET    | `/api/holidays`                | — | `{holidays: {"YYYY-MM-DD": "holiday"\|"workday"}}` 合并系统 + 手动 |
| ✏️ POST   | `/api/holidays`                | `{date, kind}` | `kind ∈ {"holiday","workday",""}`; 空 = 删 |
| ✏️ DELETE | `/api/holidays?date=YYYY-MM-DD`| — | 同上 |

`/api/settings` GET:

```json
{
  "punch_macs":   ["AA:BB:..", ...],          // 打卡设备
  "webhook_macs": ["AA:BB:..", ...],          // 全局 webhook 订阅 (per-device 开关)
  "work_start":   "09:00",
  "work_end":     "18:30",
  "global_webhook_url": "https://...",
  "webhook_keyword":    "argus",              // 钉钉/飞书关键词
  "brand_title":    "WiFi 考勤",              // 应用名称, 留空恢复默认
  "brand_subtitle": "工时统计"
}
```

`/api/settings` POST 字段(全部 optional, 三态指针: `nil` 不动 / 空串 = 清空 / 非空 = 设值):

| 字段 | 类型 | 含义 |
|---|---|---|
| `punch_mac` + `punch` | string + bool | 加/删一台打卡设备; `punch` 缺省视作 true |
| `webhook_mac` + `webhook` | string + bool | 加/删一台全局 webhook 订阅 |
| `work_start` / `work_end` | string | `HH:MM`, 必须 start < end |
| `global_webhook_url` | string | 全局 webhook URL; 空串 = 关闭 |
| `webhook_keyword` | string | ≤64 字, 追加到 markdown 末尾 |
| `brand_title` / `brand_subtitle` | string | ≤64 字, 应用名称; 改后 ETag 变化, 浏览器自动拉新 HTML |
| `me_mac` | string | 老客户端兼容: 等价于 `{punch_mac, punch:true}` |

### 4.7 账户

依赖 `WithCredentials`。

| Method | Path | 入参 | 摘要 |
|---|---|---|---|
| ✏️ POST | `/api/password` | `{old_password, new_password}` | 改密 + `RevokeAll` 撤销其它会话, 当前请求颁发新 cookie |

### 4.7b OpenID 免登录白名单

依赖 `WithOpenIDs` + `WithCredentials` (issued cookie 复用 admin 用户)。
公开入口在 4.1 的 `/api/login/openid`; 这里是登录后的管理 API。
`/etc/argus-app/openids.json` 存的是登录凭据, 文件 0600。

> 完整使用指南 + curl/前端/微信菜单/二维码集成示例:
> [`docs/openid-login.md`](../docs/openid-login.md)

| Method | Path | 入参 | 摘要 |
|---|---|---|---|
| GET    | `/api/openids`                      | —                  | `{openids: ["wx_aaa", ...]}` |
| ✏️ POST   | `/api/openids`                      | `{openid}`         | 加一条; 已存在则幂等 |
| ✏️ DELETE | `/api/openids?openid=wx_aaa`        | —                  | 删一条 |
| ✏️ DELETE | `/api/openids?clear=1`              | —                  | 清空白名单 |

### 4.8 系统 + 版本 + 升级

| Method | Path | 入参 | 摘要 |
|---|---|---|---|
| ✏️ POST | `/api/system/reboot`           | — | 0.5 秒后调 `reboot(8)`; 响应已发送 |
| ✏️ POST | `/api/system/restart-network`  | — | 后台 fire-and-forget, 因为命令会断掉当前 HTTP 连接 |
| GET    | `/api/version`                  | — | `{version, commit, date, upgrade_open}` 来自 `WithVersion` 戳的字段 |
| GET    | `/api/version/check[?force=1]`  | — | 30 分钟缓存的 GitHub Release 探测; `force=1` 跳过缓存 |
| ✏️ POST | `/api/upgrade`                  | `{version?}` | spawn `install.sh`; 空版本 = latest |

`/api/version/check` 响应包含 `current/latest/has_update/release_url/notes/prerelease/fetched_at`。

### 4.9 备份导出 / 导入

依赖 `WithDataDir`。两端都过 `writeAuth` (导出虽然是 GET, 但包里有凭据
哈希, 当作敏感写操作处理)。

| Method | Path | 入参 | 摘要 |
|---|---|---|---|
| ✏️ GET  | `/api/backup/export` | — | 流式 `application/gzip`, 文件名 `argus-app-backup-YYYYMMDD-HHMMSS.tar.gz` |
| ✏️ POST | `/api/backup/import` | multipart: `file` + `restore_credentials?` | 原子替换 `<dataDir>` + reload 各 store |

`/api/backup/import` 响应:

```json
{
  "ok": true,
  "restored": ["aliases.json","settings.json", ...],
  "skipped":  ["credentials.json"],            // restore_credentials=false 时
  "manifest": {"format":"argus-app-backup","format_version":1,
               "exported_at":"...","exporter_version":"v0.1.x","exporter_hostname":"..."},
  "session_revoked": true                       // 仅 restore_credentials=true 时
}
```

`session_revoked=true` 时, 前端要把用户踢回 `/login`。

## 五、测试

| 包 | 测试文件 | 覆盖 |
|---|---|---|
| `release` | `version_test.go` | semver 比较 8 case (含 pre-release / 反向 / 等价) |
| `release` | `backup_test.go` | round-trip + `restore_credentials=false` 跳过 + 3 种 zip-slip 拒绝 + 缺/错 manifest |
| `store/credentials` | `store_test.go` | bcrypt + 0600 文件权限 + 改密落盘 + SessionStore TTL/Revoke/RevokeAll |
| `store/notify` | `store_test.go` | `Set` 永不删 + `notifications.json` 强制 0600 |
| `store/history` | `store_test.go` | `ComputeWorktime` 全部边界 (workday/weekend/holiday/makeup/otday + late/early/missing_in/missing_out) |
| `store/holidays` | `store_test.go` | manual > system > weekend 优先级 |
| `store/override` | `store_test.go` | 扁平 → 嵌套迁移 |
| `web` | `punch_classify_test.go` | dispatchNotify 打卡分类四象限 |
| `web` | `punch_checkout_test.go` | recordPunchCheckout 7 个分支 + last-write-wins |

跑全部:

```sh
go test ./...
```

> CI 只在 `v*.*.*` tag 触发 (`.github/workflows/ci.yml`), push 到 main
> 不再自动跑测试。改完代码本地 `go test ./... && go vet ./... && go build ./...`
> 一遍, 把绿后再合 main, 打 tag 时 CI + Release 一起跑。

## 六、零依赖政策

模块**只依赖标准库 + golang.org/x/crypto + github.com/xxl6097/argusd**
(见根目录 `go.mod`)。新增功能时:
- 优先用 stdlib (`archive/tar`, `compress/gzip`, `crypto/sha256`, ...)
- 向 OpenWrt 路由器发请求时用 `os/exec` + `ubus` / `uci` / `iwpriv` 直接 shell out
- 任何"装个 lib 一行就解决"的诱惑都先看下能不能 50 行 stdlib 搞定 — 路由器 ROM/RAM 紧

## 七、调用入口 (cmd/app/main.go)

```go
import (
    "github.com/xxl6097/argus-app/interval/owrt"
    "github.com/xxl6097/argus-app/interval/release"
    "github.com/xxl6097/argus-app/interval/store/alias"
    "github.com/xxl6097/argus-app/interval/store/credentials"
    "github.com/xxl6097/argus-app/interval/store/history"
    "github.com/xxl6097/argus-app/interval/store/holidays"
    "github.com/xxl6097/argus-app/interval/store/notify"
    "github.com/xxl6097/argus-app/interval/store/override"
    "github.com/xxl6097/argus-app/interval/store/settings"
    "github.com/xxl6097/argus-app/interval/web"
    owrtd "github.com/xxl6097/argusd"
)

aliasStore := alias.New("/etc/argus-app/aliases.json")
srv := web.NewServer(watcher,
    web.WithDataDir("/etc/argus-app"),
    web.WithAliases(aliasStore),
    web.WithSettings(settings.New("/etc/argus-app/settings.json")),
    web.WithOverrides(override.New("/etc/argus-app/overrides.json", aliasStore)),
    web.WithCredentials(credentials.New("/etc/argus-app/credentials.json")),
    web.WithVersion(release.VersionInfo{Version: ver}, "xxl6097/argus-app"),
    // ...
)

http.ListenAndServe(":9099", srv)

// Watcher 事件入口要把 srv.OnEvent 串进去:
watcher.Run(ctx, srv.OnEvent, onError)
// syslog 可选, 串进来能让 history 落 src=syslog:WPA_COMPLETE 之类:
go owrtd.WatchSyslog(ctx, srv.OnSyslog, onError)
```

完整 Option 见 `web/server.go`, 完整 flag 见 `cmd/app/main.go` + `argus-app -help`。

## 八、添加新功能的标准动作

1. **新 endpoint**: 在 `web/handlers_xxx.go` 加 handler, 在 `web/server.go:NewServer` 的 `s.mux.HandleFunc` 区块挂上 `gate(s.handleXxx)` (gate 即 requireAuth)
2. **新 store**: 在 `store/<name>/store.go` 新建一个包, 模板照 `store/alias/store.go`:
   - `Store` 类型 + `New(path) *Store` + `Reload()` + 私有 `load()` + `persistLocked()`
   - 用 `util.WriteJSONAtomic` 做原子写
   - 在 `web/server.go` 加 `WithXxx(*xxx.Store) Option`, `Server` struct 加字段
   - 在 `cmd/app/main.go` 加 `flag.String("xxx", ...)` + `xxx.New(path)`
3. **新事件类型 / 新 notify 通道**: 改 `web/notify_dispatch.go`, 多数情况下不需要改 OnEvent 顺序
4. **新外部命令**: 模仿 `owrt/dhcp.go` 的 `staKickCmds [][]string` 模式 — 优先级数组 + `exec.LookPath` + 短超时, 头一个返回 0 的命令获胜
5. 别忘了:
   - 新 store 在 `web/backup_handlers.go:handleBackupImport` 调 `Reload()`
   - `web/server.go:capabilities()` 把 capability 暴露给前端
   - `web/assets/dashboard.html` 把 capability 在前端检测出来
   - 写测试 (table-driven 优先)
