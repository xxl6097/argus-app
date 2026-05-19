// devices.js — 设备列表 (table + cards) + click delegation + detail panel
// dispatcher.
//
// renderDevices is the entry point called by reloadDevices() in main.js.
// It renders the device table + mobile cards, restores any previously
// expanded detail rows from state.openDetailRows, then attaches a single
// click delegate on the document for the badges / chevrons / interactive
// cells. The detail dispatcher (renderDetail) routes each tab to the
// right module.

import {
  esc, cssEsc, relativeTime,
  rssiClass, rssiText, linkText,
  elBody, elCards, elCountOnline, elCountOffline,
  state,
} from "./state.js";
import { renderHistoryPane } from "./history.js";
import { renderNotifyPane } from "./notify.js";
import { renderWorktimePane, renderMonthlyStatsPane } from "./worktime.js";
import { openStaticIPModal } from "./dhcp.js";

export function renderDevices(rows) {
  let onlineN = 0, offlineN = 0;
  const caps = state.caps || {};
  const leases = state.staticLeases || {};

  // Desktop table
  elBody.innerHTML = "";
  for (const d of rows) {
    const isOff = d.status === "offline";
    if (isOff) offlineN++; else onlineN++;
    const tr = document.createElement("tr");
    tr.className = "dev-row";
    if (isOff) tr.classList.add("is-offline");
    tr.dataset.mac = d.mac;
    if (d.is_me) tr.dataset.isMe = "1";
    const offAgo = relativeTime(d.offline_at_ms);
    const statusPill = isOff
      ? '<span class="pill offline" title="离线于 ' + esc(offAgo) + '">离线 ' + esc(offAgo) + '</span>'
      : '<span class="pill online">在线</span>';
    const hostTitle = (d.alias ? d.alias + (d.hostname ? " (" + d.hostname + ")" : "") : (d.hostname || ""));
    const vendor = d.vendor || "—";
    const link = linkText(d);
    const meBadge = caps.settings
      ? (d.is_me
          ? '<span class="me-badge" data-action="toggle-me" title="当前的打卡设备,工作时长以此设备在线状态为准(点击取消)">打卡设备</span>'
          : '<span class="me-badge is-not-me" data-action="toggle-me" title="设为打卡设备,以此设备上下线记录为准统计工作时长与加班">设为打卡</span>')
      : '';
    // 踢下线徽章: 只在线 + 非有线设备显示。有线设备无法 deauth, 离线
    // 设备踢了也无意义 (重连后就上线了)。
    const kickBadge = (!isOff && !d.wired)
      ? '<span class="kick-badge" data-action="kick" title="踢下线: 强制断开此设备的 WiFi 连接, 设备会自动重连。适合临时屏蔽某个设备 30s。">踢下线</span>'
      : '';
    tr.innerHTML =
      '<td><span class="expand-chev">▶</span>' + statusPill + '</td>' +
      '<td class="mac" title="' + esc(d.mac) + '">' + esc(d.mac) + meBadge + kickBadge + '</td>' +
      '<td class="ip-cell" title="' + esc(d.ip || "") + '">' + ipCell(d, caps, leases) + '</td>' +
      '<td class="host" title="' + esc(hostTitle) + '">' + hostCell(d) + '</td>' +
      '<td class="vendor" title="' + esc(vendor) + '">' + esc(vendor) + '</td>' +
      '<td class="' + rssiClass(d.rssi) + '" title="' + esc(rssiText(d)) + '">' + esc(rssiText(d)) + '</td>' +
      '<td title="' + esc(link) + '">' + esc(link) + '</td>';
    elBody.appendChild(tr);
  }

  // Mobile cards
  elCards.innerHTML = "";
  for (const d of rows) {
    const isOff = d.status === "offline";
    const card = document.createElement("div");
    card.className = "card-row dev-row" + (isOff ? " is-offline" : "");
    card.dataset.mac = d.mac;
    if (d.is_me) card.dataset.isMe = "1";
    const offAgo = relativeTime(d.offline_at_ms);
    const statusPill = isOff
      ? '<span class="pill offline">离线 ' + esc(offAgo) + '</span>'
      : '<span class="pill online">在线</span>';
    const vendorRow = d.vendor
      ? '<div class="cr-vendor" title="' + esc(d.vendor) + '">厂商 ' + esc(d.vendor) + '</div>'
      : '';
    const hostTitle = (d.alias ? d.alias + (d.hostname ? " (" + d.hostname + ")" : "") : (d.hostname || ""));
    const meBadge = caps.settings
      ? (d.is_me
          ? ' <span class="me-badge" data-action="toggle-me" title="当前的打卡设备">打卡设备</span>'
          : ' <span class="me-badge is-not-me" data-action="toggle-me" title="设为打卡设备">设为打卡</span>')
      : '';
    const kickBadge = (!isOff && !d.wired)
      ? ' <span class="kick-badge" data-action="kick" title="踢下线">踢下线</span>'
      : '';
    card.innerHTML =
      '<div class="cr-mac" title="' + esc(d.mac) + '"><span class="expand-chev">▶</span>' + esc(d.mac) + meBadge + kickBadge + '</div>' +
      '<div class="cr-status">' + statusPill + '</div>' +
      '<div class="cr-host" title="' + esc(hostTitle) + '">' + hostCell(d) + ' <span class="ip">· ' + ipCell(d, caps, leases) + '</span></div>' +
      vendorRow +
      '<div class="cr-link" title="' + esc(linkText(d)) + '">' + esc(linkText(d)) + '</div>' +
      '<div class="cr-rssi ' + rssiClass(d.rssi) + '">' + esc(rssiText(d)) + '</div>';
    elCards.appendChild(card);
  }

  elCountOnline.textContent  = "在线 " + onlineN;
  elCountOffline.textContent = "离线 " + offlineN;

  // Re-open any rows that were expanded before the reload, so periodic
  // refreshes don't collapse the user's work.
  for (const mac of Array.from(state.openDetailRows)) {
    const row = elBody.querySelector('tr.dev-row[data-mac="' + cssEsc(mac) + '"]');
    if (row) openDetailFor(row, true);
    else state.openDetailRows.delete(mac);
  }
}

// ipCell renders the IP with an optional 🔒 lock icon (static

function ipCell(d, caps, leases) {
  const ip = d.ip || "—";
  const lease = leases[d.mac];
  const isStatic = !!lease;
  let html = isStatic
    ? '<span class="ip-text ip-static" title="已静态分配">🔒 ' + esc(ip) + '</span>'
    : '<span class="ip-text">' + esc(ip) + '</span>';
  if (caps.dhcp) {
    html += '<span class="staticip-btn" data-action="staticip" title="设为静态 IP">📌</span>';
  }
  return html;
}

// hostCell builds the hostname cell with an optional alias prefix

function hostCell(d) {
  const hasAlias = d.alias && d.alias.length > 0;
  const origHost = d.hostname || "";
  let inner;
  if (hasAlias) {
    inner = '<span class="alias">' + esc(d.alias) + '</span>';
    if (origHost && origHost !== d.mac && origHost.replace(/:/g,"") !== d.mac.replace(/:/g,"")) {
      inner += ' <span class="orig-host">(' + esc(origHost) + ')</span>';
    }
  } else {
    inner = esc(origHost || "—");
  }
  inner += '<span class="rename-btn" data-action="rename" title="重命名">✎</span>';
  return inner;
}

// Delegate click events on pencil / pin buttons.
document.addEventListener("click", ev => {
  const renameBtn = ev.target.closest(".rename-btn");
  if (renameBtn) {
    const host = renameBtn.closest(".host, .cr-host");
    const row  = renameBtn.closest("[data-mac]");
    if (host && row) openRenameForm(host, row.dataset.mac);
    return;
  }
  const staticBtn = ev.target.closest(".staticip-btn");
  if (staticBtn) {
    const row = staticBtn.closest("[data-mac]");
    if (row) openStaticIPModal(row.dataset.mac);
    return;
  }
  const meBtn = ev.target.closest(".me-badge");
  if (meBtn) {
    ev.stopPropagation();
    const row = meBtn.closest("[data-mac]");
    if (row) toggleMe(row.dataset.mac, meBtn.classList.contains("is-not-me"));
    return;
  }
  const kickBtn = ev.target.closest(".kick-badge");
  if (kickBtn) {
    ev.stopPropagation();
    const row = kickBtn.closest("[data-mac]");
    if (row) kickDevice(row.dataset.mac);
    return;
  }
  // Click anywhere on a device row toggles the detail panel,
  // excluding any interactive cell (rename form, static-ip button,
  // me-badge) which have their own handlers above.
  if (ev.target.closest("input, button, .rename-form, .rename-btn, .staticip-btn, .me-badge, .kick-badge")) return;
  const devRow = ev.target.closest("tr.dev-row, .card-row.dev-row");
  if (devRow) toggleDetail(devRow);
});

// toggleMe adds or removes the given MAC from the 打卡设备 set.
// Non-exclusive: multiple devices can be in the set simultaneously,

async function toggleMe(mac, makeMe) {
  try {
    let r;
    if (makeMe) {
      r = await fetch("/api/settings", {
        method: "POST", headers: {"Content-Type": "application/json"},
        body: JSON.stringify({ punch_mac: mac, punch: true }),
      });
    } else {
      r = await fetch("/api/settings?mac=" + encodeURIComponent(mac), { method: "DELETE" });
    }
    if (!r.ok) {
      const b = await r.json().catch(() => ({}));
      alert("操作失败: " + (b.error || r.status));
      return;
    }
    state.currentSettings = null;
    state.reloadDevices();
  } catch (e) {
    alert("网络错误: " + e.message);
  }
}

// kickDevice 强制把某 WiFi 设备踢下线 (deauth)。
// 路由器侧只对在线 + 非有线设备渲染按钮, 后端再校验一次。
// 设备一般会自动重连; 后端的 ahsapd staDisconnect 默认 dismissTime=30s,

async function kickDevice(mac) {
  if (!confirm(
    "确定要把 " + mac + " 踢下线吗?\n\n" +
    "• 设备的 WiFi 会立即断开\n" +
    "• 多数情况下设备会自动重连 (~30 秒后)\n" +
    "• 仅适用于无线设备, 有线设备无效"
  )) return;
  try {
    const r = await fetch("/api/devices/kick", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ mac }),
    });
    const b = await r.json().catch(() => ({}));
    if (!r.ok) {
      alert("踢下线失败: " + (b.error || r.status));
      return;
    }
    const t = document.createElement("div");
    t.className = "toast";
    if (b.kicked) {
      t.innerHTML = "<b>已踢下线</b><br>" + esc(mac) + "<br><span style='opacity:.7'>" + esc(b.kicked) + "</span>";
    } else {
      t.innerHTML = "<b>已下发踢下线指令</b><br>" + esc(mac) + "<br>路由器无可用 deauth 工具, 仅作记录";
    }
    document.body.appendChild(t);
    setTimeout(() => t.classList.add("fade"), 4000);
    setTimeout(() => t.remove(), 4500);
  } catch (e) {
    alert("网络错误: " + e.message);
  }
}

// toggleDetail opens/closes the detail panel below (desktop) or

function toggleDetail(row) {
  const mac = row.dataset.mac;
  if (!mac) return;
  if (state.openDetailRows.has(mac)) {
    closeDetailFor(row);
    state.openDetailRows.delete(mac);
  } else {
    openDetailFor(row, false);
    state.openDetailRows.add(mac);
  }
}

function closeDetailFor(row) {
  row.classList.remove("expanded");
  const mac = row.dataset.mac;
  // Desktop: detail row is the <tr> immediately following.
  const next = row.nextElementSibling;
  if (next && next.classList.contains("detail-row") && next.dataset.mac === mac) {
    next.remove();
    return;
  }
  // Mobile: detail block is a child of the card.
  const inner = row.querySelector(":scope > .detail-wrap");
  if (inner) inner.remove();
}

function openDetailFor(row, silent) {
  row.classList.add("expanded");
  const mac = row.dataset.mac;
  // isMe is sourced from the row's dataset (set during render from
  // the per-device `is_me` field). This keeps the decision tied to
  // the authoritative /api/devices response instead of the cached
  // settings snapshot, which may lag a 设为打卡/取消 toggle by one
  // render cycle.
  const isMe = row.dataset.isMe === "1";
  const isTable = row.tagName === "TR";
  let wrap;
  if (isTable) {
    const detail = document.createElement("tr");
    detail.className = "detail-row";
    detail.dataset.mac = mac;
    const td = document.createElement("td");
    td.colSpan = 7;
    detail.appendChild(td);
    row.parentNode.insertBefore(detail, row.nextSibling);
    wrap = document.createElement("div");
    wrap.className = "detail-wrap";
    td.appendChild(wrap);
  } else {
    wrap = document.createElement("div");
    wrap.className = "detail-wrap";
    row.appendChild(wrap);
  }
  renderDetail(wrap, mac, isMe);
  if (!silent && !isTable) {
    // Give the browser a beat before scrolling so the layout has settled.
    setTimeout(() => wrap.scrollIntoView({ behavior: "smooth", block: "nearest" }), 30);
  }
}

function renderDetail(wrap, mac, isMe) {
  const caps = state.caps || {};
  // 工作时长 + 月统计 are gated on "this is the 打卡设备" — the
  // user flipped its is_me flag via the row badge. Everyone else
  // sees only the on/off timeline.
  const hasWorktime = caps.worktime && isMe;
  const notifyTab = caps.notifications
    ? '<span class="tab" data-tab="notify">⚙ 信息设置</span>'
    : '';
  const tabs = hasWorktime
    ? '<span class="tab active" data-tab="worktime">📊 工作时长</span>' +
      '<span class="tab" data-tab="months">📅 月统计</span>' +
      '<span class="tab" data-tab="history">📜 上下线记录</span>' +
      notifyTab
    : '<span class="tab active" data-tab="history">📜 上下线记录</span>' +
      notifyTab;
  wrap.innerHTML =
    '<div class="detail-head">' + tabs +
    '<span style="margin-left:auto">MAC ' + esc(mac) + '</span>' +
    '</div>' +
    '<div class="detail-body"></div>';
  const body = wrap.querySelector(".detail-body");
  // Drill-through target: when 月统计 invokes activateTab("worktime",
  // "2026-03"), the 工作时长 pane loads with that month preselected.
  let worktimeInitMonth = null;
  const activateTab = (name, arg) => {
    wrap.querySelectorAll(".tab").forEach(t => t.classList.toggle("active", t.dataset.tab === name));
    if (name === "worktime") {
      renderWorktimePane(body, mac, arg || worktimeInitMonth);
      worktimeInitMonth = null;
    } else if (name === "months") {
      renderMonthlyStatsPane(body, mac, (month) => {
        worktimeInitMonth = month;
        activateTab("worktime", month);
      });
    } else if (name === "notify") {
      renderNotifyPane(body, mac);
    } else {
      renderHistoryPane(body, mac);
    }
  };
  wrap.querySelector(".detail-head").addEventListener("click", ev => {
    const t = ev.target.closest(".tab");
    if (t) activateTab(t.dataset.tab);
  });
  activateTab(hasWorktime ? "worktime" : "history");
}

// formatLocalDate returns YYYY-MM-DD in the user's locale (not UTC),

function openRenameForm(hostEl, mac) {
  const original = hostEl.innerHTML;
  const currentAlias = hostEl.querySelector(".alias");
  const initialValue = currentAlias ? currentAlias.textContent : "";

  hostEl.innerHTML =
    '<span class="rename-form">' +
    '<input type="text" value="' + esc(initialValue) + '" maxlength="64" placeholder="别名" />' +
    '<button data-act="save">保存</button>' +
    '<button data-act="clear">清除</button>' +
    '<button data-act="cancel">取消</button>' +
    '</span>';

  const form  = hostEl.querySelector(".rename-form");
  const input = form.querySelector("input");
  input.focus();
  input.select();

  const restore = () => { hostEl.innerHTML = original; };
  const submit  = async (name) => {
    try {
      const r = await fetch("/api/aliases", {
        method: "POST", headers: {"Content-Type": "application/json"},
        body: JSON.stringify({ mac, name }),
      });
      if (!r.ok) {
        const body = await r.json().catch(() => ({}));
        alert("保存失败: " + (body.error || r.status));
        restore();
        return;
      }
      state.reloadDevices();
    } catch (e) {
      alert("网络错误: " + e.message);
      restore();
    }
  };

  form.addEventListener("click", ev => {
    const b = ev.target.closest("button");
    if (!b) return;
    if (b.dataset.act === "save")   submit(input.value);
    if (b.dataset.act === "clear")  submit("");
    if (b.dataset.act === "cancel") restore();
  });
  input.addEventListener("keydown", ev => {
    if (ev.key === "Enter")  submit(input.value);
    if (ev.key === "Escape") restore();
  });
}
