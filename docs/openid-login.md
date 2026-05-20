# OpenID 免登录接口使用指南

argus-app 在常规账号密码 (`/api/login`) 之外, 提供一条 **OpenID 白名单**
免登录通道, 适用于:

- 微信 / 钉钉 / 飞书等已经认证过的入口跳转回 argus-app
- 二维码 / NFC 标签 / 印刷链接, 扫一下就进
- iframe / 内嵌 webview, 不希望用户再输一次密码
- 内部脚本 / 看板大屏, 长期免登录

> ⚠️ **OpenID 即凭据。** 任何拿到一个白名单内 OpenID 的人都能登录。请像
> 对待密码一样保护它: 不要 commit 到代码、不要写在公网 URL 上、定期轮换。

## 1. 服务端启用

启动 argus-app 时, 默认会从 `/etc/argus-app/openids.json` 读白名单
(命令行 flag `-openids`, 设为空字符串 `""` 可禁用)。

```sh
argus-app -listen 0.0.0.0:9099 \
  -credentials /etc/argus-app/credentials.json \
  -openids /etc/argus-app/openids.json \
  ...其他参数
```

`openids.json` 是一个 JSON 字符串数组, 文件权限自动 `0600`:

```json
[
  "wx_oxQ7BvE1abcd1234",
  "ops-dashboard-2026"
]
```

文件不存在 / 内容损坏会被当成空白名单处理, 不会让进程起不来。

> 注意: OpenID 免登录依赖 `-credentials` 同时启用 (issued cookie 复用 admin
> 身份)。`-credentials=""` 即关闭整个登录系统时, 该接口会直接 503 / 跳
> `/login?error=openid_disabled`。

## 2. 管理白名单 (登录后)

下面所有接口都要先有一份合法的 admin session cookie, 通常通过
`/api/login` 或 `/api/login/openid` 拿到。

### 2.1 列出白名单

```http
GET /api/openids HTTP/1.1
Cookie: argus_session=<your-session-token>
```

**示例**

```sh
curl -s -b "argus_session=xxx" https://router.example.com/api/openids
```

```json
{
  "openids": [
    "ops-dashboard-2026",
    "wx_oxQ7BvE1abcd1234"
  ]
}
```

### 2.2 添加一个 OpenID

```http
POST /api/openids HTTP/1.1
Content-Type: application/json
Cookie: argus_session=<your-session-token>

{"openid": "wx_oxQ7Bvxyz5678"}
```

```sh
curl -s -b "argus_session=xxx" \
  -H 'Content-Type: application/json' \
  -d '{"openid":"wx_oxQ7Bvxyz5678"}' \
  https://router.example.com/api/openids
```

返回最新的全量列表:

```json
{
  "ok": true,
  "openids": ["ops-dashboard-2026", "wx_oxQ7BvE1abcd1234", "wx_oxQ7Bvxyz5678"]
}
```

| 校验 | 失败响应 |
|---|---|
| 必传非空 | 400 `openid: openid required` |
| ≤128 字符 | 400 `openid: too long (N > 128)` |
| 已存在 | 200 (幂等, 不重复添加) |

### 2.3 删除一个 OpenID

```http
DELETE /api/openids?openid=wx_oxQ7Bvxyz5678 HTTP/1.1
Cookie: argus_session=<your-session-token>
```

```sh
curl -s -X DELETE -b "argus_session=xxx" \
  "https://router.example.com/api/openids?openid=wx_oxQ7Bvxyz5678"
```

```json
{"ok": true, "openids": ["ops-dashboard-2026", "wx_oxQ7BvE1abcd1234"]}
```

不存在的 OpenID 也返回 200 (幂等)。

### 2.4 清空白名单

```http
DELETE /api/openids?clear=1 HTTP/1.1
Cookie: argus_session=<your-session-token>
```

```json
{"ok": true, "openids": []}
```

## 3. 免登录入口

```
POST /api/login/openid          ← 推荐, 用于前端 fetch
GET  /api/login/openid?openid=… ← 用于跳转 / 二维码
```

两个端点都是 **公开** 的, 不需要任何 cookie。命中白名单时 server 颁发
一个标准的 admin session cookie, 之后所有受保护接口都能直接调用。

### 3.1 POST 形式 (推荐)

适合前端拿到 OpenID 后直接调:

```http
POST /api/login/openid HTTP/1.1
Content-Type: application/json

{"openid": "wx_oxQ7BvE1abcd1234"}
```

```sh
curl -i -X POST \
  -H 'Content-Type: application/json' \
  -d '{"openid":"wx_oxQ7BvE1abcd1234"}' \
  https://router.example.com/api/login/openid
```

成功 (200):

```http
HTTP/1.1 200 OK
Content-Type: application/json
Set-Cookie: argus_session=<token>; Path=/; HttpOnly; SameSite=Lax

{"ok": true}
```

失败:

| 状态 | 响应体 | 触发条件 |
|---|---|---|
| 400 | `{"error":"invalid json body"}` | body 不是合法 JSON |
| 400 | `{"error":"openid required"}` | `openid` 为空字符串 |
| 401 | `{"error":"openid not whitelisted"}` | 不在白名单 |
| 503 | `{"error":"openid login not enabled"}` | 服务端未启用 |

> 注意: 失败响应不会回显输入的 openid, 也不区分"拼错"和"已被移出白名单",
> 故意做成跟 `/api/login` 一样的模糊错误信息, 防止枚举。

### 3.2 GET 形式 (跳转用)

适合扫码 / 链接分享 / 微信菜单跳转:

```
GET /api/login/openid?openid=wx_oxQ7BvE1abcd1234
GET /api/login/openid?openid=wx_oxQ7BvE1abcd1234&next=/devices
```

成功:

```http
HTTP/1.1 302 Found
Location: /                     ← 或 ?next= 指定的同源相对路径
Set-Cookie: argus_session=<token>; Path=/; HttpOnly; SameSite=Lax
```

失败也会 302, 落到 `/login?error=...` (而不是 401), 这样 webview / 微信
浏览器都不会显示 raw JSON:

| Location | 触发条件 |
|---|---|
| `/login?error=missing_openid`  | `?openid=` 为空 |
| `/login?error=invalid_openid`  | 不在白名单 |
| `/login?error=openid_disabled` | 服务端未启用 |
| `/login?error=session`         | session 颁发失败 (极少见) |

`next` 参数仅接受**同源相对路径** (`/...`, 排除 `//...`), 防止 open redirect。
非法值会被忽略, 等同于跳到 `/`。

## 4. 集成示例

### 4.1 前端 fetch 自助登录

```html
<!-- 应用启动时, 后端把当前用户的 OpenID 注入页面 -->
<script>
  // 已在白名单的 OpenID; 服务端模板渲染后注入
  const openid = "{{ .OpenID }}";

  async function ensureLogin() {
    // 试一下 /api/version, 没登录会 401
    const probe = await fetch("/api/version", { credentials: "same-origin" });
    if (probe.ok) return;

    // 否则用 OpenID 换一个 cookie
    const r = await fetch("/api/login/openid", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ openid }),
      credentials: "same-origin",
    });
    if (!r.ok) {
      // 让用户走密码登录兜底
      location.href = "/login";
      return;
    }
    // 成功了, 刷一下页面拿到完整数据
    location.reload();
  }

  ensureLogin();
</script>
```

### 4.2 微信公众号菜单一键进控制台

公众号管理后台 → 自定义菜单 → 跳转网页:

```
https://router.example.com/api/login/openid?openid={{微信用户的openid}}&next=/
```

(实际接入时, OpenID 由你自己的中转服务从微信侧拿到再拼到链接上;
argus-app 不直接对接微信 OAuth — 它只校验白名单。)

### 4.3 二维码 (打印贴在前台)

把下面这串 URL 编进二维码:

```
https://router.example.com/api/login/openid?openid=lobby-tablet-2026
```

`lobby-tablet-2026` 提前用 `/api/openids` POST 加进白名单。前台员工扫
一下进, 不需要密码; 想撤销时把这条 OpenID DELETE 掉即可。

### 4.4 脚本 / 监控大屏长期免登录

```sh
#!/bin/bash
# /usr/local/bin/argus-tail.sh — 长期跑的监控脚本

OPENID="ops-dashboard-2026"
BASE="https://router.example.com"
COOKIE_JAR=/tmp/argus.cookie

# 拿一次 cookie, 后续 curl 复用
curl -s -c "$COOKIE_JAR" -X POST \
  -H 'Content-Type: application/json' \
  -d "{\"openid\":\"$OPENID\"}" \
  "$BASE/api/login/openid" >/dev/null

# 后续业务调用 — 全部用同一个 cookie
curl -s -b "$COOKIE_JAR" "$BASE/api/devices" | jq '.online'
curl -s -b "$COOKIE_JAR" "$BASE/api/worktime?mac=AA:BB:..." | jq '.overtime_secs'
```

> session 默认 24h 过期, 长跑脚本要么周期性重换, 要么捕获 401 后再
> 调一次 `/api/login/openid`。

## 5. Web UI 操作

登录后 → 右上角 ⚙ 设置 → "OpenID 免登录白名单":

- 输入框输入 OpenID, 回车或点 "添加" 入列
- 列表项右侧 "删除" 一键移除 (会弹确认对话框)
- 区块在服务端 `capabilities.openids = false` 时自动隐藏 (例如二进制
  没启用 credentials, 或 `-openids=""`)

## 6. 安全提示

1. **OpenID = 凭据**, 不是身份标识; 一旦泄露, 立即 DELETE 掉换新的。
2. **GET 形式会进访问日志和 Referer**。如果可以选, 优先用 POST。需要
   GET 的场景 (扫码/菜单跳转), 把 OpenID 设计成**长且随机** (32+ 字符,
   推荐 `openssl rand -hex 16`), 而不是 `admin-2026` 这种可猜的。
3. **白名单 ≠ 用户系统**。所有 OpenID 登录后都是同一个 admin 身份,
   操作日志里看不到是哪个 OpenID 进来的。需要审计请用 nginx access_log
   层做。
4. **导出备份**默认会包含 `openids.json`。导入时如果勾选 "同时恢复账户
   与凭据", 整张白名单也会随之回滚, 注意场景 (例如把测试备份恢复到
   生产)。
5. **生产环境强烈建议在 argus-app 前面挂 HTTPS** (Caddy / Nginx /
   frpc + tls)。明文 HTTP 下 `/api/login/openid?openid=...` 很容易被
   局域网抓包。

## 7. 相关接口

完整 HTTP API 一览见 [`interval/README.md` 第四节](../interval/README.md#四http-api-一览)。

| Path | 方法 | 鉴权 | 用途 |
|---|---|---|---|
| `/api/login/openid` | POST | 公开 | OpenID → cookie (JSON 模式) |
| `/api/login/openid` | GET  | 公开 | OpenID → cookie + 302 跳转 |
| `/api/openids`      | GET  | session | 列出白名单 |
| `/api/openids`      | POST | session + writeAuth | 加一个 |
| `/api/openids`      | DELETE | session + writeAuth | 删一个 / `?clear=1` 清空 |
| `/api/logout`       | POST | session | 撤销当前 cookie |
