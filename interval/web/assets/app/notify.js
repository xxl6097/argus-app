// notify.js — 信息设置 tab (per-device webhook + ntfy).
//
// Renders the form for one MAC's notification config, plus a "res
// topic" inbox showing the most recent ntfy reply messages received
// for that device.

import { esc } from "./state.js";

export async function renderNotifyPane(body, mac) {
  body.innerHTML = '<div class="wt-empty">加载中…</div>';
  let cfg = { webhook_url: "", ntfy_server: "", ntfy_username: "",
              ntfy_password: "", ntfy_req_topic: "", ntfy_res_topic: "" };
  let exists = false;
  try {
    const r = await fetch("/api/notifications?mac=" + encodeURIComponent(mac), { cache: "no-store" });
    if (r.ok) {
      const b = await r.json();
      exists = !!b.exists;
      if (b.config) cfg = { ...cfg, ...b.config };
    }
  } catch (_) { /* best-effort */ }

  body.innerHTML =
    '<div class="notify-form">' +
    '<div class="notify-section"><h3>Webhook</h3>' +
    '<label class="modal-field">' +
    '  <span>Webhook 地址 (上下线时 POST JSON,留空关闭)</span>' +
    '  <input type="url" class="nf-webhook" placeholder="https://example.com/hook" value="' + esc(cfg.webhook_url) + '">' +
    '</label>' +
    '</div>' +
    '<div class="notify-section"><h3>ntfy 推送 + 接收</h3>' +
    '<label class="modal-field">' +
    '  <span>服务器地址 (例 https://ntfy.sh,留空关闭)</span>' +
    '  <input type="url" class="nf-server" placeholder="https://ntfy.sh" value="' + esc(cfg.ntfy_server) + '">' +
    '</label>' +
    '<div class="notify-row">' +
    '<label class="modal-field"><span>用户名 (可选)</span>' +
    '<input type="text" class="nf-user" autocomplete="off" value="' + esc(cfg.ntfy_username) + '"></label>' +
    '<label class="modal-field"><span>密码 (可选)</span>' +
    '<input type="password" class="nf-pass" autocomplete="new-password" value="' + esc(cfg.ntfy_password) + '"></label>' +
    '</div>' +
    '<div class="notify-row">' +
    '<label class="modal-field"><span>req 主题 (推送上下线消息)</span>' +
    '<input type="text" class="nf-req" placeholder="device-events" value="' + esc(cfg.ntfy_req_topic) + '"></label>' +
    '<label class="modal-field"><span>res 主题 (订阅,接收外部消息)</span>' +
    '<input type="text" class="nf-res" placeholder="device-replies" value="' + esc(cfg.ntfy_res_topic) + '"></label>' +
    '</div>' +
    '</div>' +
    '<div class="notify-actions">' +
    '<button class="hdr-btn nf-save">保存</button>' +
    (exists ? '<button class="hdr-btn hdr-danger nf-del">移除</button>' : '') +
    '<span class="nf-status" style="margin-left:6px;color:var(--muted);font-size:11px"></span>' +
    '</div>' +
    '</div>' +
    '<div class="notify-msgs">' +
    '<h3 style="display:flex;align-items:center;gap:8px">res 主题最近消息' +
    '<button class="hdr-btn nf-refresh" style="font-size:11px">刷新</button>' +
    '</h3>' +
    '<div class="nf-msg-list"><div class="wt-empty">尚未加载</div></div>' +
    '</div>';

  const webhookEl = body.querySelector(".nf-webhook");
  const serverEl = body.querySelector(".nf-server");
  const userEl = body.querySelector(".nf-user");
  const passEl = body.querySelector(".nf-pass");
  const reqEl = body.querySelector(".nf-req");
  const resEl = body.querySelector(".nf-res");
  const statusEl = body.querySelector(".nf-status");
  const saveBtn = body.querySelector(".nf-save");
  const delBtn = body.querySelector(".nf-del");
  const msgList = body.querySelector(".nf-msg-list");

  const collect = () => ({
    mac,
    webhook_url: webhookEl.value.trim(),
    ntfy_server: serverEl.value.trim(),
    ntfy_username: userEl.value,
    ntfy_password: passEl.value,
    ntfy_req_topic: reqEl.value.trim(),
    ntfy_res_topic: resEl.value.trim(),
  });
  const flash = (msg, ok) => {
    statusEl.textContent = msg;
    statusEl.style.color = ok ? "var(--accent)" : "var(--offline)";
    setTimeout(() => { statusEl.textContent = ""; }, 2000);
  };
  saveBtn.addEventListener("click", async () => {
    try {
      const r = await fetch("/api/notifications", {
        method: "POST", headers: {"Content-Type": "application/json"},
        body: JSON.stringify(collect()),
      });
      const b = await r.json().catch(() => ({}));
      if (!r.ok) { flash(b.error || ("HTTP " + r.status), false); return; }
      flash("✓ 已保存", true);
      loadMessages();
    } catch (e) { flash("网络错误: " + e.message, false); }
  });
  if (delBtn) {
    delBtn.addEventListener("click", async () => {
      if (!confirm("移除该设备的全部通知配置?")) return;
      try {
        const r = await fetch("/api/notifications?mac=" + encodeURIComponent(mac), { method: "DELETE" });
        const b = await r.json().catch(() => ({}));
        if (!r.ok) { flash(b.error || ("HTTP " + r.status), false); return; }
        renderNotifyPane(body, mac);
      } catch (e) { flash("网络错误: " + e.message, false); }
    });
  }

  const loadMessages = async () => {
    msgList.innerHTML = '<div class="wt-empty">加载中…</div>';
    try {
      const r = await fetch("/api/notifications/messages?mac=" + encodeURIComponent(mac), { cache: "no-store" });
      if (!r.ok) {
        msgList.innerHTML = '<div class="wt-empty">未启用 res 主题订阅</div>';
        return;
      }
      const b = await r.json();
      const msgs = b.messages || [];
      if (msgs.length === 0) {
        msgList.innerHTML = '<div class="wt-empty">暂无消息(若已配置 res 主题,等待外部端推送)</div>';
        return;
      }
      let html = '<ul class="hist-list" style="max-height:280px">';
      for (const m of msgs) {
        const t = new Date(m.received_at).toLocaleString();
        const title = m.title ? '<b>' + esc(m.title) + '</b> ' : '';
        html += '<li>' +
          '<span class="h-ts">' + esc(t) + '</span>' +
          '<span class="h-kind online">' + esc(m.topic || "") + '</span>' +
          '<span class="h-extra">' + title + esc(m.message || "") + '</span>' +
          '</li>';
      }
      html += '</ul>';
      msgList.innerHTML = html;
    } catch (e) { msgList.innerHTML = '<div class="wt-empty">网络错误: ' + esc(e.message) + '</div>'; }
  };
  body.querySelector(".nf-refresh").addEventListener("click", loadMessages);
  loadMessages();
}
// (file ends here)
