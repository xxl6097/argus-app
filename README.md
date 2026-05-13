# argus-app

基于 [argusd](https://github.com/xxl6097/argusd) 的 OpenWrt 设备监控工具，
在原有 WiFi 上下线探测能力之上扩展了一套面向**个人考勤 / 加班统计**的 Web 仪表板。
路由器把 WiFi 上线时间当作"打卡"，离线当作"下班"，自动算出每天的在岗时长、加班时长、迟到 / 早退状态，
按月汇总并推送到 Webhook / ntfy。

> 名字虽叫 `argus-app`，仪表板上的对外标题已经改成了 **WiFi 考勤 · 工时统计**。

## 设计目标

- **单文件部署**：纯 Go，零外部依赖（HTML 嵌入二进制）。CGO 关闭，交叉编译到 ARM64
  跑在主流 OpenWrt 路由器上（MT7981、ipq60xx 等）。
- **零侵入**：不动路由器原生功能。所有持久化都在 `/etc/argusd/*.json` 单独管理。
- **本地优先**：默认绑定 `0.0.0.0:9099`，按 RFC1918 网段做写权限控制，不需要鉴权服务。
- **自治**：每天凌晨从公开 API 拉取国家法定节假日，省下手工维护。

---

## 项目结构

```
argus-app/
├── cmd/app/main.go                  # 入口 + 命令行参数
├── interval/web/
│   ├── server.go                    # HTTP / SSE / 路由总线
│   ├── aliases.go                   # MAC → 友好名 持久化
│   ├── dhcp.go                      # OpenWrt uci 静态 IP 管理
│   ├── system.go                    # 重启网络 / 重启路由器
│   ├── history.go                   # 上下线历史 + 工时计算核心
│   ├── settings.go                  # 打卡设备列表 + 标准工时配置
│   ├── overrides.go                 # 按月嵌套的 (alias, date) 手动工时覆写
│   ├── holidays.go                  # 双层节假日存储 + timor.tech 自动拉取
│   ├── notify.go                    # 每设备 Webhook + ntfy 推送 / 订阅
│   └── assets/dashboard.html        # 单文件 Web UI（vanilla JS + EventSource）
├── buildAndUpRun.sh                 # 交叉编译 + 上传 + 启动 一键脚本
└── go.mod
```

---

## 后端能力

### 1. 设备探测（来自 argusd）
- 自动选择数据源：`ahsapd` / `dhcp.leases` / `arp` 等
- 每秒轮询，cooldown / 抖动抑制
- 监听 OpenWrt syslog 捕获 `MAC表新增 / 无线接入 / 认证完成 / DHCP分配` 等底层事件

### 2. MAC 别名 (`aliases.json`)
为某 MAC 起易记名字（如 `iphone17`、`lenovo`），仪表板以及内部存储均优先使用别名。

### 3. 静态 IP 管理（DHCP）
通过 OpenWrt `uci` 写 `dhcp` 段：
- 设静态 IP / 修改 / 移除
- 可选「立即生效」：`wifi reload` 让设备瞬断重连，新 IP 即刻拿到
- 冲突检测：同一 IP 已属于其他 MAC 时弹替换确认

### 4. 上下线历史 (`history/<mac>.jsonl`)
每个 MAC 一份 JSONL 追加日志，记录 ONLINE / OFFLINE 事件。
- **保留 7 天**，超出阈值自动压缩
- 启动时把当前在线设备播种为 ONLINE，避免长期无事件丢上线时点

### 5. 工时统计核心（`history.go`）
按日 (`ComputeWorktime`) / 按月 (`MonthlyReport`) 两套算法。

**日级输出字段**：
| 字段 | 含义 |
|---|---|
| `present_secs` | 在岗时长 = 末次下线 − min(首次上线, 标准上班) |
| `early_ot_secs` | 早到加班 = max(0, 标准上班 − 首次上线) |
| `late_ot_secs` | 晚走加班 = max(0, 末次下线 − 标准下班) |
| `overtime_secs` | 加班时长 = `early_ot + late_ot`（OT 日例外，详见下文） |
| `arrival_status` | `""` / `late` / `missed_in`（迟到 / 漏刷卡） |
| `departure_status` | `""` / `early_leave`（早退） |
| `day_kind` | `workday` / `weekend` / `holiday` / `makeup` / `otday` |
| `ot_day` | 是否「整天算加班」的日子 |
| `manual` | 是否使用了手动 override |
| `missing_out` | 仅有上班记录、没有下班 |

**日子类型与口径**：
| 类型 | 触发 | 加班口径 | 迟到/早退判定 |
|---|---|---|---|
| workday | 工作日（默认） | 早到 + 晚走 | ✅ |
| weekend | 周六/周日（未被调休覆盖） | 整天 | ❌ |
| holiday | 法定节假日（API 推送） | **不算加班** | ❌ |
| makeup | 调休工作日（API 推送 `workday`） | 早到 + 晚走 | ✅ |
| otday | 用户手动标记的工作日加班 | 整天 | ❌ |

**月度聚合**：累计加班 / 累计在岗 / 出勤天数 / 周末加班天数 / 迟到 / 漏刷卡 / 早退 / 日均加班 / 周均加班（按 5 个工作日折算）。

### 6. 标准工时与打卡设备 (`settings.json`)
- 工时窗口：`work_start` / `work_end` 全局共用，支持 HH:MM 与 HH:MM:SS
- 打卡设备：`punch_macs[]` —— 多选，每台设备独立统计

### 7. 手动覆写 (`overrides.json`)
- 当系统漏检（路由器宕机、忘带手机）时手动补录某天的上班/下班时间
- 文件按月嵌套：`{alias: {YYYY-MM: {YYYY-MM-DD: {in, out}}}}`
- 兼容旧扁平结构，启动时自动迁移

### 8. 节假日双层存储 (`holidays.json` + `holidays_system.json`)
- **手动层**（`holidays.json`）：UI 中「设为工作日 / 设为加班日 / 设为节假日 / 恢复默认」
- **系统层**（`holidays_system.json`）：从 `timor.tech/api/holiday/year/YYYY` 拉取
  - 启动立即拉一次当前年 + 后续 9 年
  - 之后每天 **03:00 本地时间** 重新拉
  - 单年失败不影响其他年；网络全挂保留旧缓存
  - 中国国务院通常只公布次年节假日，未公布年份自动跳过
- 查询优先级：手动 > 系统 > 周末判定

### 9. 通知派发 (`notifications.json`)
每台设备可独立配置：
- **Webhook**：HTTP(S) 端点，POST 一份 JSON（含结构化字段 + `markdown` 字段）
- **ntfy**：服务器 + 用户名/密码 + req 主题（推送上下线消息）+ res 主题（订阅外部消息）
- res 主题消息保留每设备最近 100 条，UI 实时展示

**消息内容（Markdown）**：
- 打卡设备 ONLINE → 「【alias】上班了」+ 上班时间 + 今日加班 + 本月加班
- 打卡设备 OFFLINE → 「【alias】下班了」+ 下班时间 + 今日加班 + 本月加班
- 普通设备 → 「【alias】上线啦 / 下线啦」+ 设备 / IP / MAC / 时间

---

## Web UI 功能

主页面：左栏 = 局域网设备，右栏 = 打卡事件 SSE 流。

### 设备行（每行）
- 状态徽章（在线 / 离线 + 离线时长）
- MAC 字段右侧「**设为打卡 / 打卡设备**」徽章 — 点击即加入 / 移出打卡集合（可多选）
- 主机名 / 别名 + ✎ 内联重命名
- IP 显示 + 🔒 静态租约标记 + 📌 设静态 IP 弹窗
- 整行点击展开**详情面板**

### 详情面板（按 tab 分）

#### 📊 工作时长（仅打卡设备显示）
- 顶部月度汇总卡：累计加班 / 在岗 / 出勤 / 日均加班 / 周均加班
- 中部每日列表：日期 / 周几（按日子类型变色）/ 上班 / 下班 / 在岗 / 加班 / 状态徽章 / 🗑 删除
- 月份 ◀ / ▶ / 「本月」按钮 + 上班/下班时间快速调整 + 「保存为默认」+ 「+ 补录」
- 选中某天后下方展开当日详情卡 + 「手动编辑 / 设为工作日 / 设为加班日 / 设为节假日 / 恢复默认」按钮组
- 节假日 / 周末加班 / 调休 / 手动加班 / 缺下班 / 仍在线等情况会显示带颜色的横条提示
- 日期/月份维度的迟到、漏刷卡、早退会用红字突出

#### 📅 月统计（仅打卡设备显示）
- 近 12 个月一表罗列：月份 / 加班 / 在岗 / 出勤 / 日均加班 / 周均加班 / 状态
- 顶部 5 列汇总：近 12 月累计加班 / 在岗 / 出勤天数 / 月均加班 / 有记录月份数
- 点击任意月份 → 自动跳到「工作时长」tab 并加载该月

#### 📜 上下线记录
- 最近 7 天的上线 / 离线时间线，按日分组、最新在前
- 显示 IP、主机名等附加字段

#### ⚙ 信息设置
- Webhook 地址输入框
- ntfy：服务器 / 用户名 / 密码 / req 主题 / res 主题
- 「保存」/ 「移除」按钮 + 状态提示
- 下方实时显示 res 主题最近 100 条消息（标题 + 内容）

### 顶部全局按钮
- **重启网络服务**（5–15 秒瞬断、保留配置）
- **重启路由器**（30–60 秒断网，二次确认）

---

## 安装与部署

### 1. 准备工作（首次）
- 路由器：OpenWrt 21.02+，aarch64 / armv7 / x86_64，开启 SSH
- 本机：Go 1.25+，`sshpass`（macOS：`brew install hudochenkov/sshpass/sshpass`）

### 2. 一键脚本

```bash
./buildAndUpRun.sh
```

可通过环境变量覆盖默认值：

```bash
ROUTER_HOST=192.168.1.1 \
ROUTER_USER=root \
ROUTER_PASS='your-pass' \
ROUTER_PORT=22 \
LISTEN_ADDR=0.0.0.0:9099 \
GO_BIN=/usr/local/go/bin/go \
./buildAndUpRun.sh
```

脚本流程：
1. `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build` 交叉编译
2. SSH 到路由器：`killall argus-app`
3. SCP 上传 `/tmp/argus-app`
4. 后台启动：`nohup /tmp/argus-app -listen=0.0.0.0:9099 ... &`

### 3. 命令行参数

| 参数 | 默认值 | 说明 |
|---|---|---|
| `-listen` | `""`（关闭 Web UI） | Web 监听地址，例 `0.0.0.0:9099` |
| `-aliases` | `/etc/argusd/aliases.json` | MAC 别名存储 |
| `-settings` | `/etc/argusd/settings.json` | 打卡设备 + 标准工时 |
| `-overrides` | `/etc/argusd/overrides.json` | 手动工时覆写（按月嵌套） |
| `-notifications` | `/etc/argusd/notifications.json` | Webhook / ntfy 配置 |
| `-holidays` | `/etc/argusd/holidays.json` | 用户手动节假日（不被自动刷新触碰） |
| `-holidays-system` | `/etc/argusd/holidays_system.json` | 自动拉取的节假日缓存 |
| `-holidays-years` | `10` | 拉取年数（当年 + 未来 N−1） |
| `-history-dir` | `/etc/argusd/history` | 上下线历史目录 |

任意路径置空（`-foo=""`）即禁用对应功能。

### 4. 信号

| 信号 | 行为 |
|---|---|
| SIGINT / SIGTERM | 优雅退出 |
| SIGHUP | 重启 Watcher（保留 known / cooldown） |
| SIGUSR1 | 打印 metrics 快照到 stderr |

### 5. 环境变量
- `ARGUSD_DEBUG=1` — 开启 slog Debug + 决策 trace

---

## HTTP API 一览

| 路径 | 方法 | 用途 |
|---|---|---|
| `/` | GET | 嵌入式仪表板 |
| `/api/devices` | GET | 当前设备 + 离线缓存 |
| `/api/events` | GET (SSE) | 上下线 / Change 事件流 |
| `/api/aliases` | GET / POST / DELETE | MAC 别名增删改查 |
| `/api/dhcp` | GET / POST / DELETE | 静态 IP 租约 |
| `/api/history` | GET | 某 MAC 上下线记录（最多 7 天） |
| `/api/worktime` | GET | 单日工时报告 |
| `/api/worktime/month` | GET | 月度工时报告 |
| `/api/worktime/override` | GET / POST / DELETE | 手动覆写 |
| `/api/settings` | GET / POST / DELETE | 打卡设备列表 + 标准工时 |
| `/api/holidays` | GET / POST / DELETE | 合并视图（手动 + 系统）|
| `/api/notifications` | GET / POST / DELETE | 通知配置 |
| `/api/notifications/messages` | GET | res 主题最近消息 |
| `/api/notifications/test` | POST | 触发一次合成事件用于调试 |
| `/api/system/reboot` | POST | 重启路由器 |
| `/api/system/restart-network` | POST | 重启网络服务 |

写操作（POST / DELETE）默认仅允许 RFC1918 私网调用方。

---

## 开发约定

- 所有时间显示统一为 24 小时制 `HH:MM:SS`，时长统一为 `H时M分S秒` 或紧凑 `1h7m13s`
- 日期解析使用 `ParseInLocation(..., time.Local)` 避免 UTC 偏移
- 持久化文件使用「写临时文件 + 原子 rename」保证 crash 安全
- 别名重命名后，旧的 MAC-keyed 条目在下一次写入时自动迁移到 alias key

## 许可

MIT
