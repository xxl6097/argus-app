// history.js — 上下线记录 tab.
//
// Per-day timeline view of ONLINE/OFFLINE entries from /api/history.
// Server-side retention is 30 days. Keyboard ←/→ to flip days; the
// listener self-removes when this pane leaves the DOM (user switches
// tabs or collapses the row).

import { esc, formatLocalDate, shiftDate } from "./state.js";

// Map argusd's syslog event kind tags → Chinese labels for the
// timeline pill. Kept in sync with historySyslogLabels in
// interval/web/notify_dispatch.go.
const HISTORY_SYSLOG_LABELS = {
  "WIFI_CONNECT":    "无线接入",
  "WPA_COMPLETE":    "认证完成",
  "DHCP_ACK":        "DHCP 分配",
  "MACTABLE_INSERT": "MAC 表新增",
  "WIFI_DISCONNECT": "无线断开",
  "DEAUTH":          "认证踢出",
  "MACTABLE_DELETE": "MAC 表移除",
};

// historySourceLabel(src) → {label, group} | null.
// Returns the human-readable Chinese pill for a history.src tag plus
// a CSS suffix (s-syslog / s-fetcher / s-seed) for color coding.
// Examples:  "syslog:WPA_COMPLETE" → {label: "认证完成", group: "syslog"}
//            "fetcher:ahsapd"      → {label: "ahsapd 轮询", group: "fetcher"}
//            "seed"                → {label: "启动快照", group: "seed"}
// Unknown / empty src → null (UI hides the badge).
export function historySourceLabel(src) {
  if (!src) return null;
  if (src === "seed") return { label: "启动快照", group: "seed" };
  const i = src.indexOf(":");
  if (i < 0) {
    return { label: src, group: "fetcher" };
  }
  const head = src.slice(0, i);
  const tail = src.slice(i + 1);
  if (head === "syslog") {
    return { label: HISTORY_SYSLOG_LABELS[tail] || tail, group: "syslog" };
  }
  if (head === "fetcher") {
    // fetcher 名 (ahsapd / hostapd / ...) 直接显示, 不另外汉化。
    return { label: tail + " 轮询", group: "fetcher" };
  }
  return { label: src, group: "fetcher" };
}

// Per-day history pane:
//   ◀ [date] ▶ 今天   ← 左右键 / 点按钮切换日期
// 服务端保留 30 天,可任意翻看。
export async function renderHistoryPane(body, mac) {
  const today = formatLocalDate(new Date());
  body.innerHTML =
    '<div class="wt-controls">' +
    '<label>日期 ' +
    '<button class="hdr-btn h-day-prev" title="上一天 (←)">◀</button>' +
    '<input type="date" class="h-day-in" value="' + esc(today) + '" max="' + esc(today) + '">' +
    '<button class="hdr-btn h-day-next" title="下一天 (→)">▶</button>' +
    '<button class="hdr-btn h-day-today" title="今天">今天</button>' +
    '</label>' +
    '<span class="wt-note">使用键盘 ← / → 快速翻页,服务端保留 30 天</span>' +
    '</div>' +
    '<div class="h-list-wrap"><div class="hist-empty">加载中…</div></div>';

  const dayIn = body.querySelector(".h-day-in");
  const listWrap = body.querySelector(".h-list-wrap");

  async function load(day) {
    dayIn.value = day;
    listWrap.innerHTML = '<div class="hist-empty">加载中…</div>';
    try {
      const url = "/api/history?mac=" + encodeURIComponent(mac) +
                  "&from=" + day + "&to=" + day;
      const r = await fetch(url, { cache: "no-store" });
      if (!r.ok) {
        const b = await r.json().catch(() => ({}));
        listWrap.innerHTML = '<div class="hist-empty">加载失败: ' + esc(b.error || r.status) + '</div>';
        return;
      }
      const data = await r.json();
      const entries = (data.entries || []).slice().reverse(); // newest first
      if (entries.length === 0) {
        listWrap.innerHTML = '<div class="hist-empty">' + esc(day) + ' 无上下线记录</div>';
        return;
      }
      let html = '<ul class="hist-list">';
      for (const e of entries) {
        const d = new Date(e.t);
        const time = d.toLocaleTimeString([], {hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false});
        const kindClass = e.k === "ONLINE" ? "online" : "offline";
        const kindLabel = e.k === "ONLINE" ? "上线" : "离线";
        const extra = [e.ip, e.host].filter(Boolean).join(" · ");
        const src = historySourceLabel(e.src);
        const srcCell = src
          ? '<span class="h-src s-' + esc(src.group) + '" title="' + esc(e.src) + '">' + esc(src.label) + '</span>'
          : '<span></span>';
        html += '<li>' +
          '<span class="h-ts">' + esc(time) + '</span>' +
          '<span class="h-kind ' + kindClass + '">' + kindLabel + '</span>' +
          srcCell +
          '<span class="h-extra" title="' + esc(extra) + '">' + esc(extra) + '</span>' +
          '</li>';
      }
      html += '</ul><div class="hist-empty" style="font-style:normal">' +
              esc(day) + ' · 共 ' + entries.length + ' 条</div>';
      listWrap.innerHTML = html;
    } catch (e) {
      listWrap.innerHTML = '<div class="hist-empty">网络错误: ' + esc(e.message) + '</div>';
    }
  }

  function shift(n) {
    const next = shiftDate(dayIn.value || today, n);
    // Don't navigate past today (history can't be in the future).
    if (n > 0 && next > today) return;
    load(next);
  }

  body.querySelector(".h-day-prev").addEventListener("click", () => shift(-1));
  body.querySelector(".h-day-next").addEventListener("click", () => shift(1));
  body.querySelector(".h-day-today").addEventListener("click", () => load(today));
  dayIn.addEventListener("change", () => {
    if (dayIn.value) load(dayIn.value);
  });

  // Keyboard ← / → shortcuts. Scoped: only when no input/textarea has
  // focus (we don't want to hijack the date picker), and only while
  // this pane is still in the DOM (the listener self-removes when the
  // user switches tabs / collapses the row).
  const onKey = (ev) => {
    if (!body.isConnected) {
      document.removeEventListener("keydown", onKey);
      return;
    }
    const tag = (document.activeElement && document.activeElement.tagName) || "";
    if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return;
    if (ev.key === "ArrowLeft")  { ev.preventDefault(); shift(-1); }
    if (ev.key === "ArrowRight") { ev.preventDefault(); shift(1); }
  };
  document.addEventListener("keydown", onKey);

  load(today);
}
