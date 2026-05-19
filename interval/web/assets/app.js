(function () {
  "use strict";

  const elBody         = document.getElementById("devices-body");
  const elCards        = document.getElementById("devices-cards");
  const elEvents       = document.getElementById("events-list");
  const elCountOnline  = document.getElementById("count-online");
  const elCountOffline = document.getElementById("count-offline");
  const elConn         = document.getElementById("conn");

  const MAX_EVENT_LINES = 200;
  let eventsStarted = false;
  // Set of MACs whose detail row is currently open. Survives
  // reloadDevices() (which blows away <tr>s) so the re-render can
  // re-attach the expanded panel.
  const openDetailRows = new Set();
  // Cache of the last /api/settings snapshot so we can build the
  // worktime panel without an extra roundtrip.
  let currentSettings = null;

  function rssiClass(r) {
    if (!r || r === 0) return "";
    if (r >= -55) return "rssi-strong";
    if (r >= -70) return "rssi-medium";
    if (r >= -80) return "rssi-weak";
    return "rssi-vweak";
  }

  function esc(s) {
    if (s == null) return "";
    return String(s).replace(/[&<>"']/g, c => ({
      "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
    })[c]);
  }

  function linkText(d) {
    if (d.wired) return "有线";
    return [d.radio, d.ssid && "/" + d.ssid].filter(Boolean).join("");
  }

  function rssiText(d) {
    return d.rssi ? d.rssi + " dBm" : "—";
  }

  function renderDevices(rows) {
    let onlineN = 0, offlineN = 0;
    const caps = window._caps || {};
    const leases = window._staticLeases || {};

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
    for (const mac of Array.from(openDetailRows)) {
      const row = elBody.querySelector('tr.dev-row[data-mac="' + cssEsc(mac) + '"]');
      if (row) openDetailFor(row, true);
      else openDetailRows.delete(mac);
    }
  }

  // ipCell renders the IP with an optional 🔒 lock icon (static
  // reservation) and a 📌 button to open the static-IP modal.
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
  // and a pencil (✎) button that toggles inline rename.
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
  // so we only touch the one the user clicked.
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
      currentSettings = null;
      reloadDevices();
    } catch (e) {
      alert("网络错误: " + e.message);
    }
  }

  // kickDevice 强制把某 WiFi 设备踢下线 (deauth)。
  // 路由器侧只对在线 + 非有线设备渲染按钮, 后端再校验一次。
  // 设备一般会自动重连; 后端的 ahsapd staDisconnect 默认 dismissTime=30s,
  // 即 30 秒内同一 MAC 不被重新接入。
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
  // inside (mobile) the device row.
  function toggleDetail(row) {
    const mac = row.dataset.mac;
    if (!mac) return;
    if (openDetailRows.has(mac)) {
      closeDetailFor(row);
      openDetailRows.delete(mac);
    } else {
      openDetailFor(row, false);
      openDetailRows.add(mac);
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
    const caps = window._caps || {};
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
  // since toISOString() would shift early-morning times to the previous day.
  function formatLocalDate(d) {
    const y = d.getFullYear();
    const m = String(d.getMonth() + 1).padStart(2, "0");
    const dd = String(d.getDate()).padStart(2, "0");
    return y + "-" + m + "-" + dd;
  }

  // shiftDate(YYYY-MM-DD, n) → YYYY-MM-DD shifted by n days.
  function shiftDate(s, n) {
    const [y, m, d] = s.split("-").map(Number);
    const dt = new Date(y, m - 1, d);
    dt.setDate(dt.getDate() + n);
    return formatLocalDate(dt);
  }

  // historySourceLabel maps a Server-emitted src tag to a 短中文文案 +
  // a CSS suffix (s-syslog / s-fetcher / s-seed) for color coding.
  // Examples:  "syslog:WPA_COMPLETE" → {label: "认证完成", group: "syslog"}
  //            "fetcher:ahsapd"      → {label: "ahsapd 轮询", group: "fetcher"}
  //            "seed"                → {label: "启动快照", group: "seed"}
  // Unknown / empty src → null (UI hides the badge).
  const HISTORY_SYSLOG_LABELS = {
    "WIFI_CONNECT":    "无线接入",
    "WPA_COMPLETE":    "认证完成",
    "DHCP_ACK":        "DHCP 分配",
    "MACTABLE_INSERT": "MAC 表新增",
    "WIFI_DISCONNECT": "无线断开",
    "DEAUTH":          "认证踢出",
    "MACTABLE_DELETE": "MAC 表移除",
  };
  function historySourceLabel(src) {
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
  async function renderHistoryPane(body, mac) {
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

  async function renderWorktimePane(body, mac, initialMonth) {
    // Ensure we have the current settings (worktime window).
    if (!currentSettings) {
      try {
        const r = await fetch("/api/settings", { cache: "no-store" });
        if (r.ok) currentSettings = await r.json();
      } catch (_) { /* best-effort */ }
    }
    const cfg = currentSettings || { work_start: "09:00", work_end: "18:30" };
    const defaultMonth = initialMonth || new Date().toISOString().slice(0, 7);
    const caps = window._caps || {};
    const editBtn = caps.overrides
      ? '<button class="hdr-btn wt-edit" title="手动编辑选中日期的上下班时间">手动编辑</button>'
      : '';
    const kindBtns = caps.holidays
      ? '<button class="hdr-btn wt-kind-workday" title="把当天标记为工作日(周六日也当工作日统计)">设为工作日</button>' +
        '<button class="hdr-btn wt-kind-otday"   title="把当天标记为加班日(全部在线计入加班)">设为加班日</button>' +
        '<button class="hdr-btn wt-kind-holiday" title="把当天标记为节假日(不计算加班)">设为节假日</button>' +
        '<button class="hdr-btn wt-kind-reset"   title="恢复为默认(周末/工作日自动判断)">恢复默认</button>'
      : '';
    body.innerHTML =
      '<div class="wt-controls">' +
      '<label>月份 ' +
      '<button class="hdr-btn wt-mo-prev" title="上一月">◀</button>' +
      '<input type="month" class="wt-month-in" value="' + esc(defaultMonth) + '">' +
      '<button class="hdr-btn wt-mo-next" title="下一月">▶</button>' +
      '<button class="hdr-btn wt-mo-this" title="本月">本月</button>' +
      '</label>' +
      '<label>上班 <input type="time" class="wt-start" value="' + esc(cfg.work_start || "09:00") + '"></label>' +
      '<label>下班 <input type="time" class="wt-end" value="' + esc(cfg.work_end || "18:30") + '"></label>' +
      '<button class="hdr-btn wt-save" title="保存为默认标准工时">保存为默认</button>' +
      (caps.overrides ? '<button class="hdr-btn wt-add-day" title="新增/补录某一天的上下班时间">+ 补录</button>' : '') +
      '<span class="wt-note">在岗 = 末次下线 − min(首次上线, 标准上班);加班 = 早到 + 晚走</span>' +
      '</div>' +
      '<div class="wt-month"></div>' +
      '<div class="wt-days"></div>' +
      '<div class="wt-out"></div>' +
      '<div class="wt-day-actions" style="display:none;margin-top:6px;gap:6px;flex-wrap:wrap">' + editBtn + kindBtns + '</div>';
    const monthInEl = body.querySelector(".wt-month-in");
    const startIn = body.querySelector(".wt-start");
    const endIn = body.querySelector(".wt-end");
    const saveBtn = body.querySelector(".wt-save");
    const editBtnEl = body.querySelector(".wt-edit");
    const monthBox = body.querySelector(".wt-month");
    const daysBox = body.querySelector(".wt-days");
    const out = body.querySelector(".wt-out");
    const actionsBox = body.querySelector(".wt-day-actions");

    // Currently drilled-in date. null = "month view only"; clicking
    // a day in the list opens its detail card below.
    let selectedDate = null;
    let currentMonthReport = null;

    const refreshDay = async () => {
      if (!selectedDate) {
        out.innerHTML = '';
        actionsBox.style.display = 'none';
        return;
      }
      actionsBox.style.display = 'flex';
      out.innerHTML = '<div class="wt-empty">计算中…</div>';
      const qs = "mac=" + encodeURIComponent(mac) +
        "&date=" + encodeURIComponent(selectedDate) +
        "&start=" + encodeURIComponent(startIn.value) +
        "&end=" + encodeURIComponent(endIn.value);
      try {
        const r = await fetch("/api/worktime?" + qs, { cache: "no-store" });
        if (!r.ok) {
          const b = await r.json().catch(() => ({}));
          out.innerHTML = '<div class="wt-empty">加载失败: ' + esc(b.error || r.status) + '</div>';
          return;
        }
        out.innerHTML = renderWorktimeCard(await r.json());
      } catch (e) {
        out.innerHTML = '<div class="wt-empty">网络错误: ' + esc(e.message) + '</div>';
      }
    };

    const refreshMonth = async () => {
      const month = monthInEl.value;
      if (!month) return;
      monthBox.innerHTML = '<div class="wt-empty">月度汇总加载中…</div>';
      daysBox.innerHTML = '';
      const qs = "mac=" + encodeURIComponent(mac) +
        "&month=" + encodeURIComponent(month) +
        "&start=" + encodeURIComponent(startIn.value) +
        "&end=" + encodeURIComponent(endIn.value);
      try {
        const r = await fetch("/api/worktime/month?" + qs, { cache: "no-store" });
        if (!r.ok) {
          monthBox.innerHTML = '<div class="wt-empty">加载失败</div>';
          return;
        }
        currentMonthReport = await r.json();
        monthBox.innerHTML = renderMonthSummary(currentMonthReport);
        daysBox.innerHTML = renderMonthDays(currentMonthReport, selectedDate);
      } catch (_) {
        monthBox.innerHTML = '<div class="wt-empty">网络错误</div>';
        return;
      }
      // If the selected day isn't in this month any more, collapse the
      // detail panel. Otherwise re-render it so its data stays fresh.
      if (selectedDate && !selectedDate.startsWith(month)) {
        selectedDate = null;
        out.innerHTML = '';
        actionsBox.style.display = 'none';
      } else if (selectedDate) {
        refreshDay();
      }
    };

    const saveDefault = async () => {
      saveBtn.disabled = true;
      const orig = saveBtn.textContent;
      saveBtn.textContent = "保存中…";
      try {
        const r = await fetch("/api/settings", {
          method: "POST", headers: {"Content-Type": "application/json"},
          body: JSON.stringify({ work_start: startIn.value, work_end: endIn.value }),
        });
        if (!r.ok) {
          const b = await r.json().catch(() => ({}));
          alert("保存失败: " + (b.error || r.status));
          return;
        }
        currentSettings = { ...(currentSettings || {}), work_start: startIn.value, work_end: endIn.value };
        saveBtn.textContent = "✓ 已保存";
        setTimeout(() => { saveBtn.textContent = orig; saveBtn.disabled = false; }, 1500);
      } catch (e) {
        alert("网络错误: " + e.message);
        saveBtn.disabled = false;
        saveBtn.textContent = orig;
      }
    };

    const shiftMonth = (n) => {
      const [y, m] = (monthInEl.value || defaultMonth).split("-").map(Number);
      const d = new Date(y, (m - 1) + n, 1);
      monthInEl.value = d.getFullYear() + "-" + String(d.getMonth() + 1).padStart(2, "0");
      refreshMonth();
    };
    body.querySelector(".wt-mo-prev").addEventListener("click", () => shiftMonth(-1));
    body.querySelector(".wt-mo-next").addEventListener("click", () => shiftMonth(1));
    body.querySelector(".wt-mo-this").addEventListener("click", () => {
      monthInEl.value = new Date().toISOString().slice(0, 7);
      refreshMonth();
    });
    monthInEl.addEventListener("change", refreshMonth);
    startIn.addEventListener("change", refreshMonth);
    endIn.addEventListener("change", refreshMonth);
    saveBtn.addEventListener("click", saveDefault);
    if (editBtnEl) {
      editBtnEl.addEventListener("click", () => {
        if (!selectedDate) return;
        openOverrideModal(mac, selectedDate, () => refreshMonth());
      });
    }
    // "+ 补录" — add/edit any date in the viewed month, including
    // days that didn't show up in the list because present_secs was 0.
    body.querySelector(".wt-add-day")?.addEventListener("click", () => {
      const month = monthInEl.value || defaultMonth;
      const today = new Date().toISOString().slice(0, 10);
      // Default the date prompt to "today if in viewed month, else
      // 1st of the viewed month" so the user types less.
      let prefill = month + "-01";
      if (today.startsWith(month)) prefill = today;
      const date = prompt("补录日期 (YYYY-MM-DD)", prefill);
      if (!date) return;
      if (!/^\d{4}-\d{2}-\d{2}$/.test(date.trim())) {
        alert("日期格式不正确,应为 YYYY-MM-DD");
        return;
      }
      openOverrideModal(mac, date.trim(), () => {
        // If user chose a date outside the current month, jump to
        // that month so the entry they just added is visible.
        if (!date.trim().startsWith(month)) {
          monthInEl.value = date.trim().slice(0, 7);
        }
        selectedDate = date.trim();
        refreshMonth();
      });
    });
    const setDayKind = async (kind) => {
      if (!selectedDate) return;
      try {
        const r = await fetch("/api/holidays", {
          method: "POST", headers: {"Content-Type": "application/json"},
          body: JSON.stringify({ date: selectedDate, kind }),
        });
        if (!r.ok) {
          const b = await r.json().catch(() => ({}));
          alert("操作失败: " + (b.error || r.status));
          return;
        }
        refreshMonth();
      } catch (e) { alert("网络错误: " + e.message); }
    };
    body.querySelector(".wt-kind-workday")?.addEventListener("click", () => setDayKind("workday"));
    body.querySelector(".wt-kind-otday")?.addEventListener("click", () => setDayKind("otday"));
    body.querySelector(".wt-kind-holiday")?.addEventListener("click", () => setDayKind("holiday"));
    body.querySelector(".wt-kind-reset")?.addEventListener("click", () => setDayKind(""));
    // Click a day row → drill into / collapse its detail card.
    daysBox.addEventListener("click", ev => {
      const del = ev.target.closest(".day-del");
      if (del) {
        ev.stopPropagation();
        const date = del.dataset.del;
        if (!date) return;
        if (!confirm("删除 " + date + " 的手动记录?\n\n(包括手动编辑的上/下班时间、手动标记的工作日/节假日/加班日;系统自动判断的周末不受影响)")) return;
        deleteDayManual(mac, date, () => {
          if (selectedDate === date) selectedDate = null;
          refreshMonth();
        });
        return;
      }
      const row = ev.target.closest("[data-date]");
      if (!row) return;
      const nextDate = row.dataset.date;
      selectedDate = (selectedDate === nextDate) ? null : nextDate;
      daysBox.innerHTML = renderMonthDays(currentMonthReport, selectedDate);
      refreshDay();
    });
    refreshMonth();
  }

  // renderMonthSummary is the tier-1 card: totals + averages.
  function renderMonthSummary(mrep) {
    const otH = secsToHM(mrep.overtime_secs);
    const presentH = secsToHM(mrep.present_secs);
    const avgDailyH = secsToHM(mrep.avg_daily_ot_secs);
    const avgWeeklyH = secsToHM(mrep.avg_weekly_ot_secs);
    const otClass = mrep.overtime_secs > 0 ? "pos" : "zero";
    let tags = '';
    if (mrep.ot_days > 0)           tags += ' <span class="pill change" style="font-size:10px">加班日 ' + mrep.ot_days + '</span>';
    if (mrep.late_days > 0)         tags += ' <span class="pill offline" style="font-size:10px">迟到 ' + mrep.late_days + '</span>';
    if (mrep.missed_in_days > 0)    tags += ' <span class="pill offline" style="font-size:10px">漏刷卡 ' + mrep.missed_in_days + '</span>';
    if (mrep.early_leave_days > 0)  tags += ' <span class="pill offline" style="font-size:10px">早退 ' + mrep.early_leave_days + '</span>';
    return '<div class="wt-card wt-summary">' +
      '<div class="wt-cell"><span class="wt-v ' + otClass + '" style="font-size:22px">' + otH + '</span><span class="wt-k">当月累计加班</span></div>' +
      '<div class="wt-cell"><span class="wt-v">' + presentH + '</span><span class="wt-k">当月在岗时长</span></div>' +
      '<div class="wt-cell"><span class="wt-v">' + (mrep.worked_days || 0) + '</span><span class="wt-k">出勤天数' + tags + '</span></div>' +
      '<div class="wt-cell"><span class="wt-v">' + avgDailyH + '</span><span class="wt-k">日均加班</span></div>' +
      '<div class="wt-cell"><span class="wt-v">' + avgWeeklyH + '</span><span class="wt-k">周均加班(按5日)</span></div>' +
      '</div>';
  }

  // renderMonthDays is the tier-2 list: per-day in/out and hours.
  function renderMonthDays(mrep, selectedDate) {
    if (!mrep || !mrep.days || mrep.days.length === 0) {
      return '<div class="wt-empty" style="font-style:normal">本月暂无出勤记录</div>';
    }
    const WEEKDAYS = ["周日","周一","周二","周三","周四","周五","周六"];
    let html = '<table class="wt-day-table"><thead><tr>' +
      '<th>日期</th><th>周几</th><th>上班</th><th>下班</th><th style="text-align:right">在岗</th>' +
      '<th style="text-align:right">加班</th><th>状态</th><th></th>' +
      '</tr></thead><tbody>';
    for (let i = mrep.days.length - 1; i >= 0; i--) {
      const d = mrep.days[i];
      const inT = fmtClock(d.first_seen_ms);
      const outT = fmtClock(d.last_seen_ms);
      const inClass = (d.arrival_status === "late" || d.arrival_status === "missed_in") ? "cell-bad" : "";
      let outClass = d.departure_status === "early_leave" ? "cell-bad" : "";
      if (d.missing_out) outClass = "cell-bad";
      const pH = secsToHM(d.present_secs);
      const otH = secsToHM(d.overtime_secs);
      const otClass = d.overtime_secs > 0 ? "pos" : "zero";
      // Weekday label + styling. day_kind drives the color:
      //   workday / makeup → normal (makeup slightly muted)
      //   weekend          → warm (OT visible)
      //   holiday          → accent/green (no OT expected)
      const parsed = parseYMD(d.date);
      const wd = parsed ? WEEKDAYS[parsed.getDay()] : "";
      let wdClass = "wd-normal";
      if (d.day_kind === "weekend")      wdClass = "wd-weekend";
      else if (d.day_kind === "holiday") wdClass = "wd-holiday";
      else if (d.day_kind === "otday")   wdClass = "wd-weekend";
      else if (d.day_kind === "makeup")  wdClass = "wd-makeup";
      let tag = '';
      if (d.day_kind === "holiday")             tag += '<span class="pill" style="font-size:9px;background:rgba(78,201,176,0.15);color:var(--accent)">节假日</span> ';
      else if (d.day_kind === "weekend")        tag += '<span class="pill change" style="font-size:9px">周末加班</span> ';
      else if (d.day_kind === "otday")          tag += '<span class="pill change" style="font-size:9px">加班日</span> ';
      else if (d.day_kind === "makeup")         tag += '<span class="pill" style="font-size:9px;background:rgba(138,143,152,0.15);color:var(--muted)">调休上班</span> ';
      if (d.manual)                             tag += '<span class="pill change" style="font-size:9px">手动</span> ';
      if (d.missing_out)                        tag += '<span class="pill offline" style="font-size:9px">缺下班</span> ';
      if (d.arrival_status === "late")          tag += '<span class="pill offline" style="font-size:9px">迟到</span> ';
      if (d.arrival_status === "missed_in")     tag += '<span class="pill offline" style="font-size:9px">漏刷卡</span> ';
      if (d.departure_status === "early_leave") tag += '<span class="pill offline" style="font-size:9px">早退</span> ';
      if (d.open_at_end)                        tag += '<span class="pill change" style="font-size:9px">在线中</span> ';
      const selClass = d.date === selectedDate ? " day-selected" : "";
      // Per-row trash icon clears BOTH the manual in/out and any
      // user-set day_kind for that date in one shot. Idempotent —
      // clicking on a date with nothing manual just stays no-op.
      const delBtn = '<span class="day-del" data-del="' + esc(d.date) + '" title="删除该日的手动记录(在/出/类型)">🗑</span>';
      html += '<tr class="day-row' + selClass + '" data-date="' + esc(d.date) + '">' +
        '<td class="mac">' + esc(d.date.slice(5)) + '</td>' +
        '<td class="' + wdClass + '">' + esc(wd) + '</td>' +
        '<td class="' + inClass + '">' + esc(inT) + '</td>' +
        '<td class="' + outClass + '">' + esc(outT) + '</td>' +
        '<td style="text-align:right">' + pH + '</td>' +
        '<td style="text-align:right" class="' + otClass + '">' + otH + '</td>' +
        '<td>' + tag + '</td>' +
        '<td style="text-align:right">' + delBtn + '</td>' +
        '</tr>';
    }
    html += '</tbody></table>';
    return html;
  }

  // renderMonthlyStatsPane shows a 12-month overtime summary. Each
  // row is one month; click a row to jump to the 工作时长 tab with
  // that month preloaded. "Last 12 months" is rolling from the
  // current month backwards, so it adapts as time passes.
  async function renderMonthlyStatsPane(body, mac, onPickMonth) {
    body.innerHTML = '<div class="wt-empty">加载中…</div>';
    const cfg = currentSettings || { work_start: "09:00", work_end: "18:30" };
    // Build 12 months backward from current month.
    const now = new Date();
    const months = [];
    for (let i = 0; i < 12; i++) {
      const d = new Date(now.getFullYear(), now.getMonth() - i, 1);
      months.push(d.getFullYear() + "-" + String(d.getMonth() + 1).padStart(2, "0"));
    }
    // Fetch all 12 in parallel. On failure, keep nulls and render
    // dashes for those rows — one missing month shouldn't blank the
    // whole pane.
    const reports = await Promise.all(months.map(async (m) => {
      const qs = "mac=" + encodeURIComponent(mac) +
        "&month=" + encodeURIComponent(m) +
        "&start=" + encodeURIComponent(cfg.work_start || "09:00") +
        "&end=" + encodeURIComponent(cfg.work_end || "18:30");
      try {
        const r = await fetch("/api/worktime/month?" + qs, { cache: "no-store" });
        if (!r.ok) return null;
        return await r.json();
      } catch (_) { return null; }
    }));

    // Compute 12-month totals for a small header card so the user has
    // a one-glance "yearly picture" without doing the math.
    let totalOT = 0, totalPresent = 0, totalDays = 0, totalOTDays = 0,
        totalLateDays = 0, totalMissedIn = 0, totalEarlyLeave = 0,
        monthsWithData = 0;
    for (const rep of reports) {
      if (!rep) continue;
      if ((rep.worked_days || 0) === 0) continue;
      monthsWithData++;
      totalOT += rep.overtime_secs || 0;
      totalPresent += rep.present_secs || 0;
      totalDays += rep.worked_days || 0;
      totalOTDays += rep.ot_days || 0;
      totalLateDays += rep.late_days || 0;
      totalMissedIn += rep.missed_in_days || 0;
      totalEarlyLeave += rep.early_leave_days || 0;
    }
    const avgMonthly = monthsWithData > 0 ? Math.floor(totalOT / monthsWithData) : 0;

    let html =
      '<div class="wt-card wt-summary" style="grid-template-columns:repeat(5,minmax(90px,1fr));margin-bottom:10px">' +
      '<div class="wt-cell"><span class="wt-v ' + (totalOT > 0 ? "pos" : "zero") + '" style="font-size:22px">' + secsToHM(totalOT) + '</span><span class="wt-k">近 12 月累计加班</span></div>' +
      '<div class="wt-cell"><span class="wt-v">' + secsToHM(totalPresent) + '</span><span class="wt-k">近 12 月在岗</span></div>' +
      '<div class="wt-cell"><span class="wt-v">' + totalDays + '</span><span class="wt-k">出勤天数</span></div>' +
      '<div class="wt-cell"><span class="wt-v">' + secsToHM(avgMonthly) + '</span><span class="wt-k">月均加班</span></div>' +
      '<div class="wt-cell"><span class="wt-v">' + monthsWithData + '</span><span class="wt-k">有记录月份</span></div>' +
      '</div>';

    html += '<table class="wt-day-table"><thead><tr>' +
      '<th>月份</th><th style="text-align:right">加班</th><th style="text-align:right">在岗</th>' +
      '<th style="text-align:right">出勤</th><th style="text-align:right">日均加班</th>' +
      '<th style="text-align:right">周均加班</th><th>状态</th>' +
      '</tr></thead><tbody>';
    for (let i = 0; i < reports.length; i++) {
      const m = months[i];
      const rep = reports[i];
      if (!rep) {
        html += '<tr class="day-row" data-month="' + esc(m) + '">' +
          '<td class="mac">' + esc(m) + '</td>' +
          '<td colspan="6" class="zero" style="text-align:center">加载失败</td>' +
          '</tr>';
        continue;
      }
      const otSecs = rep.overtime_secs || 0;
      const otClass = otSecs > 0 ? "pos" : "zero";
      let tags = '';
      if (rep.ot_days > 0)           tags += '<span class="pill change" style="font-size:9px">加班日 ' + rep.ot_days + '</span> ';
      if (rep.late_days > 0)         tags += '<span class="pill offline" style="font-size:9px">迟到 ' + rep.late_days + '</span> ';
      if (rep.missed_in_days > 0)    tags += '<span class="pill offline" style="font-size:9px">漏刷卡 ' + rep.missed_in_days + '</span> ';
      if (rep.early_leave_days > 0)  tags += '<span class="pill offline" style="font-size:9px">早退 ' + rep.early_leave_days + '</span> ';
      html += '<tr class="day-row" data-month="' + esc(m) + '">' +
        '<td class="mac">' + esc(m) + '</td>' +
        '<td style="text-align:right" class="' + otClass + '">' + secsToHM(otSecs) + '</td>' +
        '<td style="text-align:right">' + secsToHM(rep.present_secs || 0) + '</td>' +
        '<td style="text-align:right">' + (rep.worked_days || 0) + '</td>' +
        '<td style="text-align:right">' + secsToHM(rep.avg_daily_ot_secs || 0) + '</td>' +
        '<td style="text-align:right">' + secsToHM(rep.avg_weekly_ot_secs || 0) + '</td>' +
        '<td>' + tags + '</td>' +
        '</tr>';
    }
    html += '</tbody></table>' +
      '<div class="wt-empty" style="font-style:normal">点击某月可跳转到"工作时长"查看详情</div>';
    body.innerHTML = html;

    body.addEventListener("click", ev => {
      const row = ev.target.closest("[data-month]");
      if (!row) return;
      if (onPickMonth) onPickMonth(row.dataset.month);
    });
  }

  // renderNotifyPane shows a per-device form for webhook + ntfy
  // settings, plus a list of recent ntfy res-topic messages. The form
  // POSTs the whole config back; backend trims unknown fields.
  async function renderNotifyPane(body, mac) {
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

  // openOverrideModal pops a small dialog to set/clear the manual
  // in/out time for a specific (mac, date). Prefills with any
  // existing override so the user can tweak instead of re-enter.
  async function openOverrideModal(mac, date, onSaved) {
    let existing = { in: "", out: "" };
    try {
      const r = await fetch("/api/worktime/override?mac=" + encodeURIComponent(mac) +
                            "&date=" + encodeURIComponent(date), { cache: "no-store" });
      if (r.ok) {
        const b = await r.json();
        if (b.exists && b.override) existing = b.override;
      }
    } catch (_) { /* best-effort */ }

    const overlay = document.createElement("div");
    overlay.className = "modal-overlay";
    overlay.innerHTML =
      '<div class="modal">' +
      '<div class="modal-title">手动编辑工时</div>' +
      '<div class="modal-sub">MAC <code>' + esc(mac) + '</code> · 日期 <code>' + esc(date) + '</code></div>' +
      '<label class="modal-field">' +
      '  <span>上班时间(首次上线,空=使用自动记录)</span>' +
      '  <input type="time" step="1" class="ov-in" value="' + esc(existing.in || "") + '">' +
      '</label>' +
      '<label class="modal-field">' +
      '  <span>下班时间(末次下线,空=使用自动记录)</span>' +
      '  <input type="time" step="1" class="ov-out" value="' + esc(existing.out || "") + '">' +
      '</label>' +
      '<div class="modal-hint">手动记录会覆盖系统自动探测的上/下班边界,用于补录 WiFi 漏检或忘带手机的日子。</div>' +
      '<div class="modal-actions">' +
      (existing.in || existing.out ? '<button data-act="remove" class="btn-danger">移除</button>' : '') +
      '<button data-act="cancel">取消</button>' +
      '<button data-act="save" class="btn-primary">保存</button>' +
      '</div>' +
      '</div>';
    document.body.appendChild(overlay);
    const close = () => overlay.remove();
    const inEl = overlay.querySelector(".ov-in");
    const outEl = overlay.querySelector(".ov-out");
    const doSave = async () => {
      try {
        const r = await fetch("/api/worktime/override", {
          method: "POST", headers: {"Content-Type": "application/json"},
          body: JSON.stringify({ mac, date, in: inEl.value, out: outEl.value }),
        });
        if (!r.ok) {
          const b = await r.json().catch(() => ({}));
          alert("保存失败: " + (b.error || r.status));
          return;
        }
        close();
        if (onSaved) onSaved();
      } catch (e) { alert("网络错误: " + e.message); }
    };
    const doDelete = async () => {
      try {
        const r = await fetch("/api/worktime/override?mac=" + encodeURIComponent(mac) +
                              "&date=" + encodeURIComponent(date), { method: "DELETE" });
        if (!r.ok) {
          const b = await r.json().catch(() => ({}));
          alert("移除失败: " + (b.error || r.status));
          return;
        }
        close();
        if (onSaved) onSaved();
      } catch (e) { alert("网络错误: " + e.message); }
    };
    overlay.addEventListener("click", ev => {
      if (ev.target === overlay) { close(); return; }
      const b = ev.target.closest("button");
      if (!b) return;
      if (b.dataset.act === "cancel") close();
      if (b.dataset.act === "save") doSave();
      if (b.dataset.act === "remove") doDelete();
    });
    overlay.addEventListener("keydown", ev => {
      if (ev.key === "Escape") close();
      if (ev.key === "Enter" && ev.target.tagName === "INPUT") doSave();
    });
    inEl.focus();
  }

  function renderWorktimeCard(rep) {
    const presentH = secsToHM(rep.present_secs);
    const stdH = secsToHM(rep.standard_secs);
    const otH = secsToHM(rep.overtime_secs);
    const earlyH = secsToHM(rep.early_ot_secs);
    const lateH = secsToHM(rep.late_ot_secs);
    const otClass = rep.overtime_secs > 0 ? "pos" : "zero";
    const first = fmtClock(rep.first_seen_ms);
    const last = fmtClock(rep.last_seen_ms);
    // Arrival status decorates "首次上线" with a colored badge.
    // "missed_in" wins over "late" because anyone arriving after
    // end-of-day almost certainly forgot to check in.
    let firstTag = "";
    let firstStyle = "font-size:13px;color:var(--muted)";
    if (rep.arrival_status === "missed_in") {
      firstTag = ' <span class="pill offline" style="font-size:10px">上班漏刷卡</span>';
      firstStyle = "font-size:13px;color:var(--offline);font-weight:500";
    } else if (rep.arrival_status === "late") {
      firstTag = ' <span class="pill offline" style="font-size:10px">迟到</span>';
      firstStyle = "font-size:13px;color:var(--offline);font-weight:500";
    }
    // Departure status: only when the device has actually left
    // before standard end-of-day (still-online doesn't qualify).
    let lastTag = "";
    let lastStyle = "font-size:13px;color:var(--muted)";
    if (rep.departure_status === "early_leave") {
      lastTag = ' <span class="pill offline" style="font-size:10px">早退</span>';
      lastStyle = "font-size:13px;color:var(--offline);font-weight:500";
    }
    const breakdown =
      (rep.early_ot_secs > 0 || rep.late_ot_secs > 0)
        ? ' <span style="font-size:11px;color:var(--muted)">(早到 ' + earlyH + ' + 晚走 ' + lateH + ')</span>'
        : '';
    const manualTag = rep.manual
      ? ' <span class="pill change" style="font-size:10px">手动修正</span>'
      : '';
    let dayKindBanner = '';
    if (rep.missing_out) {
      dayKindBanner =
        '<div class="wt-empty" style="font-style:normal;background:rgba(240,113,120,0.10);border-left:2px solid var(--offline);padding:6px 10px;margin-bottom:8px;color:var(--fg)">' +
        '<b style="color:var(--offline)">下班时间缺失</b> — 这一天只记到了上班时间,没有下班记录(可能路由器当时没运行,或手动记录只填了上班)。' +
        '请点下方"手动编辑"补上下班时间,加班时长才会正确计算。' +
        '</div>';
    } else if (rep.day_kind === "holiday") {
      dayKindBanner =
        '<div class="wt-empty" style="font-style:normal;background:rgba(78,201,176,0.08);border-left:2px solid var(--accent);padding:6px 10px;margin-bottom:8px;color:var(--fg)">' +
        '<b style="color:var(--accent)">法定节假日</b> — 今日为带薪休假,不计算加班。如果你今天上班是调休/加班,点下方"设为工作日"或"设为加班日"按钮。' +
        '</div>';
    } else if (rep.day_kind === "otday") {
      dayKindBanner =
        '<div class="wt-empty" style="font-style:normal;background:rgba(229,192,123,0.14);border-left:2px solid var(--change);padding:6px 10px;margin-bottom:8px;color:var(--fg)">' +
        '<b style="color:var(--change)">加班日(手动标记)</b> — 当天在线时长全部计入加班;迟到/早退/漏刷卡不适用。' +
        '</div>';
    } else if (rep.ot_day) {
      dayKindBanner =
        '<div class="wt-empty" style="font-style:normal;background:rgba(229,192,123,0.10);border-left:2px solid var(--change);padding:6px 10px;margin-bottom:8px;color:var(--fg)">' +
        '<b style="color:var(--change)">周末加班日</b> — 当天在线全部计入加班;迟到/早退/漏刷卡不适用。' +
        '</div>';
    } else if (rep.day_kind === "makeup") {
      dayKindBanner =
        '<div class="wt-empty" style="font-style:normal;background:rgba(138,143,152,0.08);border-left:2px solid var(--muted);padding:6px 10px;margin-bottom:8px;color:var(--muted)">' +
        '调休上班日(法定工作日) — 按工作日统计,支持迟到/早退/漏刷卡判定。' +
        '</div>';
    }
    let html =
      dayKindBanner +
      '<div class="wt-card">' +
      '<div class="wt-cell"><span class="wt-v">' + presentH + '</span><span class="wt-k">在岗时长' + manualTag + '</span></div>' +
      '<div class="wt-cell"><span class="wt-v ' + otClass + '">' + otH + '</span><span class="wt-k">加班时长</span></div>' +
      '<div class="wt-cell"><span class="wt-v" style="' + firstStyle + '">' + esc(first) + '</span><span class="wt-k">首次上线' + firstTag + '</span></div>' +
      '<div class="wt-cell"><span class="wt-v" style="' + lastStyle + '">' + esc(last) + '</span><span class="wt-k">' + (rep.open_at_end ? "当前仍在线" : "末次离线") + lastTag + '</span></div>' +
      '</div>' +
      '<div class="wt-empty" style="font-style:normal">标准工时 ' + stdH + '(' + esc(rep.start) + '→' + esc(rep.end) + ')' + breakdown + ',段数 ' + (rep.sessions || 0) + '。' +
      (rep.open_at_end ? ' <span class="wt-open">(当前仍在线,数值会持续变化)</span>' : '') +
      '</div>';
    if (rep.intervals && rep.intervals.length) {
      html += '<ul class="hist-list" style="max-height:160px">';
      for (const iv of rep.intervals) {
        const s = new Date(iv.start_ms).toLocaleTimeString([], {hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false});
        const e = new Date(iv.end_ms).toLocaleTimeString([], {hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false});
        html += '<li>' +
          '<span class="h-ts">' + esc(s) + ' → ' + esc(e) + '</span>' +
          '<span class="h-kind online">' + secsToHM(iv.secs) + '</span>' +
          '<span class="h-extra">在线段</span>' +
          '</li>';
      }
      html += '</ul>';
    }
    return html;
  }

  // deleteDayManual clears any user-set manual in/out and any
  // user-set day_kind for (mac, date). Fires both deletes — each is
  // idempotent, so calling on a day with only one type of manual
  // data cleans that one and no-ops the other. Calls onDone only
  // after both settle (or on failure).
  async function deleteDayManual(mac, date, onDone) {
    const urlOverride = "/api/worktime/override?mac=" + encodeURIComponent(mac) +
      "&date=" + encodeURIComponent(date);
    const urlKind = "/api/holidays?date=" + encodeURIComponent(date);
    try {
      const [r1, r2] = await Promise.all([
        fetch(urlOverride, { method: "DELETE" }),
        fetch(urlKind,     { method: "DELETE" }),
      ]);
      if (!r1.ok && r1.status !== 404) {
        const b = await r1.json().catch(() => ({}));
        alert("删除手动时间失败: " + (b.error || r1.status));
        return;
      }
      if (!r2.ok && r2.status !== 404 && r2.status !== 503) {
        const b = await r2.json().catch(() => ({}));
        alert("删除日期类型失败: " + (b.error || r2.status));
        return;
      }
      if (onDone) onDone();
    } catch (e) {
      alert("网络错误: " + e.message);
    }
  }

  // fmtClock renders a unix-ms timestamp as HH:MM:SS. Returns "—"
  // when ms is falsy (0 / undefined), so a missing in/out shows as
  // a dash instead of the epoch or the day-start midnight.
  function fmtClock(ms) {
    if (!ms) return "—";
    return new Date(ms).toLocaleTimeString([], {hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false});
  }

  function secsToHM(secs) {
    if (!secs || secs <= 0) return "0时0分0秒";
    secs = Math.floor(secs);
    const h = Math.floor(secs / 3600);
    const m = Math.floor((secs % 3600) / 60);
    const s = secs % 60;
    return h + "时" + m + "分" + s + "秒";
  }

  // YYYY-MM-DD ↔ Date helpers. Parse via constructor with explicit
  // Y/M/D so Safari / older Android don't interpret the string as UTC
  // (new Date("2026-05-12") → UTC midnight, which can flip dates in
  // GMT+8); we want local midnight.
  function parseYMD(s) {
    if (!s) return null;
    const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s.trim());
    if (!m) return null;
    return new Date(+m[1], +m[2] - 1, +m[3]);
  }
  function fmtYMD(d) {
    const y = d.getFullYear();
    const m = String(d.getMonth() + 1).padStart(2, "0");
    const da = String(d.getDate()).padStart(2, "0");
    return y + "-" + m + "-" + da;
  }

  // openStaticIPModal shows a modal asking for an IP to reserve.
  function openStaticIPModal(mac) {
    const leases = window._staticLeases || {};
    const existing = leases[mac];
    const row = document.querySelector('[data-mac="' + cssEsc(mac) + '"]');
    const currentIP = (() => {
      if (existing) return existing.ip;
      const ipCell = row && row.querySelector(".ip-cell");
      if (!ipCell) return "";
      const m = ipCell.textContent.match(/\d+\.\d+\.\d+\.\d+/);
      return m ? m[0] : "";
    })();

    const overlay = document.createElement("div");
    overlay.className = "modal-overlay";
    overlay.innerHTML =
      '<div class="modal">' +
      '<div class="modal-title">静态 IP</div>' +
      '<div class="modal-sub">MAC: <code>' + esc(mac) + '</code></div>' +
      '<label class="modal-field">' +
      '  <span>IP 地址</span>' +
      '  <input type="text" id="modal-ip" value="' + esc(currentIP) + '" placeholder="192.168.10.50" />' +
      '</label>' +
      '<label class="modal-field">' +
      '  <span>名称(可选)</span>' +
      '  <input type="text" id="modal-name" maxlength="63" placeholder="argus-auto" value="' + esc(existing ? existing.name : "") + '" />' +
      '</label>' +
      '<label class="modal-field modal-checkbox">' +
      '  <input type="checkbox" id="modal-restart-wifi" />' +
      '  <span class="cb-label">' +
      '    <b>立即生效 (重启 WiFi)</b><br>' +
      '    <small>勾选后会 <code>wifi reload</code> / 重启 ahsapd, ' +
      '全部 WiFi 客户端会瞬断 3~5 秒后自动重连,新 IP 立刻生效。' +
      '<br>不勾选则只写配置 + 尝试踢该设备; 若厂商固件不支持单机踢 ' +
      '(如 MTK C-Life), 需用户手动在设备上关开一次 WiFi 才能生效。</small>' +
      '  </span>' +
      '</label>' +
      '<div class="modal-hint">保存后网关会重载 dnsmasq。租约类型: 永久 (infinite)。</div>' +
      '<div class="modal-actions">' +
      (existing ? '<button data-act="remove" class="btn-danger">移除</button>' : '') +
      '<button data-act="cancel">取消</button>' +
      '<button data-act="save" class="btn-primary">保存</button>' +
      '</div>' +
      '</div>';
    document.body.appendChild(overlay);

    const close = () => overlay.remove();
    const ipIn        = overlay.querySelector("#modal-ip");
    const nameIn      = overlay.querySelector("#modal-name");
    const restartChk  = overlay.querySelector("#modal-restart-wifi");
    ipIn.focus();
    ipIn.select();

    // apiURL appends ?restart_wifi=1 when the user has opted into the
    // nuclear option. The server treats its absence as "just write config
    // + try per-station kick" (Plan A), and presence as "also run
    // wifi reload / ahsapd restart" (Plan C).
    const apiURL = () =>
      "/api/dhcp" + (restartChk && restartChk.checked ? "?restart_wifi=1" : "");

    const doPost = async (body) => {
      try {
        const r = await fetch(apiURL(), {
          method: "POST", headers: {"Content-Type": "application/json"},
          body: JSON.stringify(body),
        });
        const b = await r.json().catch(() => ({}));
        if (r.status === 409 && b.owner_mac) {
          // IP is already reserved for a different MAC. Offer to
          // transfer the reservation: delete the existing owner's
          // reservation, then retry the POST. Cancel leaves both
          // reservations unchanged.
          const ok = confirm(
            "IP 冲突: " + b.ip + " 已被另一台设备占用\n" +
            "占用者 MAC: " + b.owner_mac + "\n\n" +
            "是否替换? 点“确定”会先移除占用者的静态分配,再把此 IP 分配给当前设备。\n" +
            "点“取消”则保持原样。"
          );
          if (!ok) return;
          const delR = await fetch("/api/dhcp?mac=" + encodeURIComponent(b.owner_mac), {method: "DELETE"});
          if (!delR.ok) {
            const db = await delR.json().catch(() => ({}));
            alert("移除原占用者失败: " + (db.error || delR.status));
            return;
          }
          const r2 = await fetch(apiURL(), {
            method: "POST", headers: {"Content-Type": "application/json"},
            body: JSON.stringify(body),
          });
          const b2 = await r2.json().catch(() => ({}));
          if (!r2.ok) {
            alert("替换后保存失败: " + (b2.error || r2.status));
            return;
          }
          close();
          reloadDevices();
          showApplyToast(b2.apply || {}, "已替换");
          return;
        }
        if (!r.ok) {
          alert("操作失败: " + (b.error || r.status));
          return;
        }
        close();
        reloadDevices();
        showApplyToast(b.apply || {}, "保存成功");
      } catch (e) {
        alert("网络错误: " + e.message);
      }
    };
    const doDelete = async () => {
      try {
        const r = await fetch("/api/dhcp?mac=" + encodeURIComponent(mac) +
                              (restartChk && restartChk.checked ? "&restart_wifi=1" : ""),
                              {method: "DELETE"});
        const b = await r.json().catch(() => ({}));
        if (!r.ok) {
          alert("移除失败: " + (b.error || r.status));
          return;
        }
        close();
        reloadDevices();
        showApplyToast(b.apply || {}, "已移除");
      } catch (e) {
        alert("网络错误: " + e.message);
      }
    };

    overlay.addEventListener("click", ev => {
      if (ev.target === overlay) { close(); return; }
      const b = ev.target.closest("button");
      if (!b) return;
      if (b.dataset.act === "cancel") close();
      if (b.dataset.act === "save")   doPost({mac, ip: ipIn.value.trim(), name: nameIn.value.trim()});
      if (b.dataset.act === "remove") doDelete();
    });
    overlay.addEventListener("keydown", ev => {
      if (ev.key === "Escape") close();
      if (ev.key === "Enter" && ev.target.tagName === "INPUT") {
        doPost({mac, ip: ipIn.value.trim(), name: nameIn.value.trim()});
      }
    });
  }

  function cssEsc(s) { return String(s).replace(/"/g, '\\"'); }

  // showApplyToast renders a bottom-anchored toast describing what
  // happened after a DHCP change. The server returns an "apply" object
  // with three fields: reloaded[] (init scripts that reloaded),
  // pruned[] (lease files pruned), kicked (non-empty if the station
  // was disconnected). Surfaces all three so the user knows whether
  // the device is getting a fresh IP NOW or needs to renew on its own.
  function showApplyToast(apply, action) {
    const parts = [];
    if (apply.reloaded && apply.reloaded.length) {
      parts.push("已重载: " + apply.reloaded.join(", "));
    } else {
      parts.push("未找到 DHCP 守护进程可重载");
    }
    if (apply.pruned && apply.pruned.length) {
      parts.push("已清除旧租约 (" + apply.pruned.length + " 个)");
    }
    if (apply.arp_flushed) {
      parts.push("已清除 ARP 缓存 (旧 IP " + apply.arp_flushed + ")");
    }
    if (apply.wifi_restarted) {
      parts.push("已重启 WiFi (" + apply.wifi_restarted + "),全部设备将在数秒内重连并拿到新 IP");
    } else if (apply.kicked) {
      parts.push("已踢出该设备 30s,正在重连并重新申请 IP");
    } else if (apply.reloaded && apply.reloaded.length) {
      parts.push("设备需要下次续约后才会拿到新 IP(最长 12 小时)。手动关开 WiFi 可立即生效");
    }
    const t = document.createElement("div");
    t.className = "toast";
    t.innerHTML = '<b>' + esc(action) + '</b><br>' + parts.map(esc).join('<br>');
    document.body.appendChild(t);
    setTimeout(() => t.classList.add("fade"), 5500);
    setTimeout(() => t.remove(), 6000);
  }

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
        reloadDevices();
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

  // relativeTime renders a unix-ms timestamp as a compact Chinese
  // relative string: "刚刚" / "N秒前" / "N分钟前" / "N小时前" / "N天前".
  function relativeTime(ms) {
    if (!ms) return "";
    const delta = Math.max(0, Date.now() - ms);
    const s = Math.floor(delta / 1000);
    if (s < 10) return "刚刚";
    if (s < 60) return s + "秒前";
    const m = Math.floor(s / 60);
    if (m < 60) return m + "分钟前";
    const h = Math.floor(m / 60);
    if (h < 24) return h + "小时前";
    const d = Math.floor(h / 24);
    return d + "天前";
  }

  async function reloadDevices() {
    try {
      const r = await fetch("/api/devices", { cache: "no-store" });
      const body = await r.json();
      window._caps = body.capabilities || {};
      if (window._caps.dhcp) {
        try {
          const lr = await fetch("/api/dhcp", { cache: "no-store" });
          if (lr.ok) {
            const lb = await lr.json();
            window._staticLeases = lb.leases || {};
          }
        } catch (_) { /* non-fatal */ }
      } else {
        window._staticLeases = {};
      }
      if (window._caps.settings && !currentSettings) {
        try {
          const sr = await fetch("/api/settings", { cache: "no-store" });
          if (sr.ok) currentSettings = await sr.json();
        } catch (_) { /* non-fatal */ }
      }
      renderDevices(body.devices || []);
    } catch (err) {
      console.warn("reload failed", err);
    }
  }

  function addEventLine(kind, data) {
    if (!eventsStarted) {
      elEvents.innerHTML = "";
      eventsStarted = true;
    }

    const ts = new Date(data.time).toLocaleTimeString();
    const mac = (data.device && data.device.mac) ? data.device.mac.toUpperCase() : "";

    // Coalesce noisy reconnect bursts.
    const now = Date.now();
    const top = elEvents.firstElementChild;
    if (top && top.dataset && top.dataset.mac === mac &&
        (now - Number(top.dataset.ts || 0)) < 10000) {
      const prevKind = top.dataset.kind;
      let newKind = prevKind;
      if (prevKind === "OFFLINE" && kind === "ONLINE")      newKind = "RECONNECT";
      else if (prevKind === "ONLINE" && kind === "OFFLINE") newKind = "FLAP";
      else if (kind === "CHANGE")                           newKind = prevKind;
      else                                                  newKind = kind;
      renderEventRow(top, newKind, data, ts, mac);
      top.dataset.kind = newKind;
      top.dataset.ts   = String(now);
      return;
    }

    const li = document.createElement("li");
    li.dataset.mac  = mac;
    li.dataset.kind = kind;
    li.dataset.ts   = String(now);
    renderEventRow(li, kind, data, ts, mac);
    elEvents.insertBefore(li, elEvents.firstChild);
    while (elEvents.children.length > MAX_EVENT_LINES) {
      elEvents.removeChild(elEvents.lastChild);
    }
  }

  function renderEventRow(li, kind, data, ts, mac) {
    const pillClass =
      kind === "ONLINE"    ? "online"  :
      kind === "OFFLINE"   ? "offline" :
      kind === "RECONNECT" ? "online"  :
      kind === "FLAP"      ? "offline" :
                              "change";
    const label =
      kind === "ONLINE"    ? "上线"    :
      kind === "OFFLINE"   ? "离线"    :
      kind === "RECONNECT" ? "重连"    :
      kind === "FLAP"      ? "抖动"    :
                              "变更";
    let detail = "";
    if (kind === "CHANGE" && data.changes) {
      detail = data.changes.map(c => c.field + ": " + c.old + "→" + c.new).join(", ");
    } else if (data.device) {
      detail = [data.device.ip, data.device.hostname, data.device.ssid]
        .filter(Boolean).join(" · ");
    }
    li.innerHTML =
      '<span class="ts">' + esc(ts) + '</span>' +
      '<span class="elpill pill ' + pillClass + '">' + esc(label) + '</span>' +
      '<span class="mac">' + esc(mac) + '</span>' +
      '<span class="detail" title="' + esc(detail) + '">' + esc(detail) + '</span>';
  }

  function connectSSE() {
    const es = new EventSource("/api/events", { withCredentials: true });
    es.addEventListener("hello", () => {
      elConn.textContent = "已连接";
      elConn.style.background = "rgba(78,201,176,0.15)";
      elConn.style.color = "var(--online)";
    });
    ["ONLINE", "OFFLINE", "CHANGE"].forEach(kind => {
      es.addEventListener(kind, ev => {
        try {
          const data = JSON.parse(ev.data);
          addEventLine(kind, data);
          reloadDevices();
        } catch (e) { console.warn("parse event failed", e); }
      });
    });
    es.onerror = () => {
      elConn.textContent = "重连中…";
      elConn.style.background = "rgba(240,113,120,0.15)";
      elConn.style.color = "var(--offline)";
    };
  }

  reloadDevices();
  connectSSE();
  setInterval(reloadDevices, 30000);

  // Reboot button: two-step confirmation because this takes the whole
  // LAN offline for 30-60 seconds and kills every in-flight session.
  const rebootBtn = document.getElementById("btn-reboot");
  if (rebootBtn) {
    rebootBtn.addEventListener("click", async () => {
      if (!confirm(
        "确定要重启路由器吗?\n\n" +
        "• 所有设备会瞬断网络\n" +
        "• 路由器约 30-60 秒后恢复\n" +
        "• 本页面会失去连接,需要手动刷新"
      )) return;
      if (!confirm("再次确认: 现在立即重启路由器?")) return;
      rebootBtn.disabled = true;
      rebootBtn.textContent = "重启中…";
      try {
        const r = await fetch("/api/system/reboot", { method: "POST" });
        const b = await r.json().catch(() => ({}));
        if (!r.ok) {
          alert("重启失败: " + (b.error || r.status));
          rebootBtn.disabled = false;
          rebootBtn.textContent = "重启路由器";
          return;
        }
        const t = document.createElement("div");
        t.className = "toast";
        t.innerHTML = "<b>已下发重启指令</b><br>" +
          "路由器将在约 30-60 秒后恢复。<br>" +
          "恢复后手动刷新本页面即可。";
        document.body.appendChild(t);
      } catch (e) {
        alert("网络错误: " + e.message);
        rebootBtn.disabled = false;
        rebootBtn.textContent = "重启路由器";
      }
    });
  }

  // Restart-network button: lighter alternative — reloads /etc/init.d/network,
  // ~5-15s LAN blip, config preserved. Single confirmation is enough
  // because this is recoverable and doesn't persist any state change.
  const restartNetBtn = document.getElementById("btn-restart-net");
  if (restartNetBtn) {
    restartNetBtn.addEventListener("click", async () => {
      if (!confirm(
        "确定要重启网络服务吗?\n\n" +
        "• 所有设备会瞬断 5-15 秒\n" +
        "• 配置保留 (WiFi 密码 / DHCP 预留等)\n" +
        "• 本页面可能短暂失联, 稍等自动重连"
      )) return;
      restartNetBtn.disabled = true;
      restartNetBtn.textContent = "重启中…";
      try {
        const r = await fetch("/api/system/restart-network", { method: "POST" });
        const b = await r.json().catch(() => ({}));
        if (!r.ok) {
          alert("重启网络失败: " + (b.error || r.status));
          restartNetBtn.disabled = false;
          restartNetBtn.textContent = "重启网络";
          return;
        }
        const t = document.createElement("div");
        t.className = "toast";
        t.innerHTML = "<b>已下发重启网络指令</b><br>" +
          "约 5-15 秒后恢复。SSE 会自动重连。";
        document.body.appendChild(t);
        setTimeout(() => t.classList.add("fade"), 8000);
        setTimeout(() => t.remove(), 8500);
      } catch (e) {
        // Network restart drops the very TCP connection we're on; an
        // error here is expected, not a failure.
      } finally {
        setTimeout(() => {
          restartNetBtn.disabled = false;
          restartNetBtn.textContent = "重启网络";
        }, 15000);
      }
    });
  }

  // ---- 登录态管理: 仅在服务端启用了 -credentials 时才暴露登出/改密按钮 ----
  // 服务端没有专门的 /api/whoami; 这里做最经济的探测: 第一次拉
  // /api/devices 时带 X-Requested-With, 401 → 跳登录页, 200 →
  // 顺势把按钮显出来(说明 cookie 有效, 也意味着登录确实开着)。
  // 因为整个 UI 的所有 fetch 都需要鉴权, 这里再装一层全局 401 拦截:
  // 任何接口返回 401 都把人踢回 /login。
  (function installAuthHooks() {
    const origFetch = window.fetch.bind(window);
    window.fetch = async function(input, init) {
      init = init || {};
      // 默认带 cookie (虽然同源 fetch 也带, 但显式更稳)
      if (!init.credentials) init.credentials = "same-origin";
      const headers = new Headers(init.headers || {});
      if (!headers.has("X-Requested-With")) headers.set("X-Requested-With", "fetch");
      init.headers = headers;
      const r = await origFetch(input, init);
      if (r.status === 401) {
        // 异步: 让当前 await r.json() 之类的调用先看到 401
        setTimeout(() => {
          const next = encodeURIComponent(location.pathname + location.search);
          location.href = "/login?next=" + next;
        }, 0);
      }
      return r;
    };

    // 探测一次 /api/devices, 200 才显示设置按钮 (登出 / 改密在 modal 内)
    fetch("/api/devices").then(r => {
      if (r.ok) {
        const sb = document.getElementById("btn-settings");
        if (sb) sb.style.display = "";
      }
    }).catch(() => {});

    document.getElementById("btn-logout").addEventListener("click", async () => {
      try { await fetch("/api/logout", { method: "POST" }); } catch (_) {}
      location.href = "/login";
    });

    document.getElementById("btn-passwd").addEventListener("click", () => {
      const oldP = prompt("请输入当前密码"); if (oldP === null) return;
      const newP = prompt("请输入新密码 (至少 6 位)"); if (newP === null) return;
      const newP2 = prompt("再次输入新密码"); if (newP2 === null) return;
      if (newP !== newP2)   { alert("两次输入的新密码不一致"); return; }
      if (newP.length < 6)  { alert("新密码至少 6 位");        return; }
      if (newP === oldP)    { alert("新密码不能与当前密码相同"); return; }
      fetch("/api/password", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ old_password: oldP, new_password: newP }),
      }).then(async r => {
        if (r.ok) {
          alert("密码已修改, 其他会话已被踢出。");
        } else {
          const b = await r.json().catch(() => ({}));
          alert("修改失败: " + (b.error || r.status));
        }
      });
    });

    // ---- 系统设置 modal ----
    // header 上的 ⚙ 按钮: 打开时拉一次 /api/settings 把全局 webhook 填进
    // 输入框; 子按钮 (重启网络/重启路由器/改密/退出) 通过转发 click 到
    // 仍然挂在 DOM 中的隐藏按钮来复用已绑好的 handler。
    (function wireSettingsModal() {
      const settingsBtn = document.getElementById("btn-settings");
      const sm          = document.getElementById("settings-modal");
      if (!settingsBtn || !sm) return;
      const inWebhook   = document.getElementById("set-global-webhook");
      const inKeyword   = document.getElementById("set-webhook-keyword");
      const saveBtn     = document.getElementById("set-save-webhook");
      const saveMsg     = document.getElementById("set-save-msg");
      const closeBtn    = document.getElementById("set-close");

      function setMsg(text, kind) {
        saveMsg.textContent = text || "";
        saveMsg.className = "set-msg" + (kind ? " " + kind : "");
      }

      async function loadGlobalWebhook() {
        setMsg("");
        try {
          const r = await fetch("/api/settings", { cache: "no-store" });
          if (r.ok) {
            const j = await r.json();
            inWebhook.value = j.global_webhook_url || "";
            if (inKeyword) inKeyword.value = j.webhook_keyword || "";
          }
        } catch (_) { /* best-effort */ }
      }

      function openModal() {
        sm.classList.add("show");
        loadGlobalWebhook();
      }
      function closeModal() {
        sm.classList.remove("show");
      }

      settingsBtn.addEventListener("click", openModal);
      closeBtn.addEventListener("click", closeModal);
      sm.addEventListener("click", (ev) => { if (ev.target === sm) closeModal(); });
      document.addEventListener("keydown", (ev) => {
        if (ev.key === "Escape" && sm.classList.contains("show")) closeModal();
      });

      saveBtn.addEventListener("click", async () => {
        const url = inWebhook.value.trim();
        const keyword = (inKeyword ? inKeyword.value : "").trim();
        if (url && !/^https?:\/\//i.test(url)) {
          setMsg("URL 需以 http:// 或 https:// 开头", "err");
          return;
        }
        saveBtn.disabled = true;
        const orig = saveBtn.textContent;
        saveBtn.textContent = "保存中…";
        try {
          const r = await fetch("/api/settings", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ global_webhook_url: url, webhook_keyword: keyword }),
          });
          if (r.ok) {
            setMsg(url ? "✓ 已保存" : "✓ 已关闭全局 webhook", "ok");
          } else {
            const b = await r.json().catch(() => ({}));
            setMsg("保存失败: " + (b.error || r.status), "err");
          }
        } catch (e) {
          setMsg("网络错误: " + e.message, "err");
        } finally {
          saveBtn.disabled = false;
          saveBtn.textContent = orig;
        }
      });

      // Forward modal sub-buttons → existing hidden handlers.
      const forward = (modalId, hiddenId) => {
        document.getElementById(modalId).addEventListener("click", () => {
          document.getElementById(hiddenId).click();
        });
      };
      forward("set-restart-net", "btn-restart-net");
      forward("set-reboot",      "btn-reboot");
      forward("set-passwd",      "btn-passwd");
      forward("set-logout",      "btn-logout");

      // ---- 备份与恢复 ----
      // 导出: 直接走 GET /api/backup/export. 因为 fetch 包装器加了
      // X-Requested-With, 用它会把 cookie 跑出去但浏览器不会触发
      // 下载. 这里用一个临时 <a download> 模拟点击, 等价于
      // location.href 但能控制文件名而不污染历史记录.
      const exportBtn  = document.getElementById("set-backup-export");
      const importPick = document.getElementById("set-backup-import-pick");
      const fileInput  = document.getElementById("set-backup-file");
      const backupMsg  = document.getElementById("set-backup-msg");

      function setBackupMsg(text, kind) {
        backupMsg.textContent = text || "";
        backupMsg.className = "set-msg" + (kind ? " " + kind : "");
      }

      exportBtn.addEventListener("click", () => {
        setBackupMsg("正在生成备份…");
        // 使用隐藏 <a> 触发下载; 服务端响应自带
        // Content-Disposition, 浏览器按那里的 filename 保存.
        const a = document.createElement("a");
        a.href = "/api/backup/export";
        a.style.display = "none";
        document.body.appendChild(a);
        a.click();
        setTimeout(() => { a.remove(); setBackupMsg("✓ 已开始下载", "ok"); }, 500);
      });

      // 导入: 点 picker → file input → 选完文件后跳到二级 modal 让
      // 用户最终确认 + 选择是否恢复凭据. 这避免 "误点恢复" 直接
      // 覆盖数据.
      const im        = document.getElementById("import-modal");
      const imInfo    = document.getElementById("im-file-info");
      const imCreds   = document.getElementById("im-restore-creds");
      const imConfirm = document.getElementById("im-confirm");
      const imCancel  = document.getElementById("im-cancel");
      let pendingFile = null;

      importPick.addEventListener("click", () => fileInput.click());
      fileInput.addEventListener("change", () => {
        if (!fileInput.files || !fileInput.files[0]) return;
        pendingFile = fileInput.files[0];
        const sizeKB = (pendingFile.size / 1024).toFixed(1);
        imInfo.textContent = "文件: " + pendingFile.name + "  ·  " + sizeKB + " KB";
        imCreds.checked = true;
        im.classList.add("show");
      });
      function closeImport() {
        im.classList.remove("show");
        pendingFile = null;
        // 重置 input 让用户能再次选同一个文件
        fileInput.value = "";
      }
      imCancel.addEventListener("click", closeImport);
      im.addEventListener("click", (ev) => { if (ev.target === im) closeImport(); });

      imConfirm.addEventListener("click", async () => {
        if (!pendingFile) return;
        imConfirm.disabled = true;
        const orig = imConfirm.textContent;
        imConfirm.textContent = "导入中…";
        setBackupMsg("正在导入…");
        try {
          const fd = new FormData();
          fd.append("file", pendingFile);
          fd.append("restore_credentials", imCreds.checked ? "true" : "false");
          const r = await fetch("/api/backup/import", { method: "POST", body: fd });
          const b = await r.json().catch(() => ({}));
          if (!r.ok) {
            setBackupMsg("导入失败: " + (b.error || r.status), "err");
            return;
          }
          const nRestored = (b.restored || []).length;
          const nSkipped  = (b.skipped  || []).length;
          let msg = "✓ 已恢复 " + nRestored + " 个文件";
          if (nSkipped > 0) msg += ", 跳过 " + nSkipped;
          setBackupMsg(msg, "ok");
          closeImport();
          if (b.session_revoked) {
            alert("已恢复账户与凭据。所有会话被强制登出, 请用新凭据重新登录。");
            location.href = "/login";
          } else {
            // Reload so the new aliases/settings/etc. appear without a manual refresh.
            setTimeout(() => location.reload(), 1500);
          }
        } catch (e) {
          setBackupMsg("网络错误: " + e.message, "err");
        } finally {
          imConfirm.disabled = false;
          imConfirm.textContent = orig;
        }
      });
    })();

    // ---- 版本号显示 + 在线检测升级 ----
    // header 上的版本徽章: 启动时立刻拉一次 /api/version 显示当前版本,
    // 然后异步 /api/version/check 询问 GitHub. 有新版 → 徽章变橙 +
    // 加 🆙 前缀提示. 点击弹 modal 显示 release notes + 一键升级按钮.
    const versionBtn = document.getElementById("btn-version");
    const vmModal    = document.getElementById("version-modal");
    const vmTitle    = document.getElementById("vm-title");
    const vmMeta     = document.getElementById("vm-meta");
    const vmNotes    = document.getElementById("vm-notes");
    const vmUpgrade  = document.getElementById("vm-upgrade");
    const vmRelease  = document.getElementById("vm-release");
    const vmRecheck  = document.getElementById("vm-recheck");
    const vmClose    = document.getElementById("vm-close");
    // 防御: 如果 modal 节点没渲染上来 (比如老 HTML 还在浏览器缓存里),
    // 就早 return, 让 logout/passwd 等其它按钮还能正常工作。
    if (!versionBtn || !vmModal) return;

    let versionState = {
      current: "", upgradeOpen: false,
      latest: "", hasUpdate: false, releaseURL: "", notes: "", checked: false,
    };

    function renderVersionPill() {
      const cur = versionState.current || "?";
      versionBtn.textContent = (versionState.hasUpdate ? "🆙 " : "v") +
                               cur.replace(/^v/, "");
      versionBtn.classList.toggle("hdr-update", versionState.hasUpdate);
      versionBtn.style.display = "";
      if (versionState.hasUpdate) {
        versionBtn.title = "发现新版本 " + versionState.latest + ",点击查看";
      } else if (versionState.checked) {
        versionBtn.title = "当前已是最新 (" + cur + "),点击查看 release notes";
      } else {
        versionBtn.title = "当前版本 " + cur + ",点击检查更新";
      }
    }

    async function loadVersion() {
      try {
        const r = await fetch("/api/version");
        if (!r.ok) return;
        const v = await r.json();
        versionState.current = v.version || "dev";
        versionState.upgradeOpen = !!v.upgrade_open;
        renderVersionPill();
      } catch (_) { /* best-effort */ }
    }

    async function checkUpdate(force) {
      if (!versionState.upgradeOpen) {
        vmTitle.textContent = "版本信息";
        vmMeta.textContent = "当前版本: " + (versionState.current || "?");
        vmNotes.textContent = "升级检测未启用 (开发版本或离线部署)。";
        vmUpgrade.style.display = "none";
        vmRelease.style.display = "none";
        return;
      }
      vmTitle.textContent = "检查更新中…";
      vmMeta.textContent = "";
      vmNotes.textContent = "正在询问 GitHub…";
      vmUpgrade.style.display = "none";
      vmRelease.style.display = "none";
      try {
        const url = "/api/version/check" + (force ? "?force=1" : "");
        const r = await fetch(url);
        if (!r.ok) {
          const b = await r.json().catch(() => ({}));
          vmTitle.textContent = "检查失败";
          vmNotes.textContent = b.error || ("HTTP " + r.status);
          return;
        }
        const data = await r.json();
        versionState.latest     = data.latest || "";
        versionState.hasUpdate  = !!data.has_update;
        versionState.releaseURL = data.release_url || "";
        versionState.notes      = data.notes || "";
        versionState.checked    = true;
        renderVersionPill();

        if (data.has_update) {
          vmTitle.textContent = "🆙 发现新版本 " + data.latest;
        } else {
          vmTitle.textContent = "当前已是最新版本";
        }
        vmMeta.textContent = "当前: " + (data.current || "?") + "  ·  最新: " + (data.latest || "?") +
                             (data.fetched_at ? "  ·  查询于 " + data.fetched_at.replace("T", " ").replace("Z", " UTC") : "");
        vmNotes.textContent = data.notes || "(此版本没有 release notes)";
        vmRelease.style.display = data.release_url ? "" : "none";
        vmUpgrade.style.display = data.has_update ? "" : "none";
      } catch (e) {
        vmTitle.textContent = "网络错误";
        vmNotes.textContent = e.message;
      }
    }

    versionBtn.addEventListener("click", () => {
      vmModal.classList.add("show");
      checkUpdate(false);
    });
    vmClose.addEventListener("click", () => vmModal.classList.remove("show"));
    vmModal.addEventListener("click", (ev) => {
      if (ev.target === vmModal) vmModal.classList.remove("show");
    });
    vmRecheck.addEventListener("click", () => checkUpdate(true));
    vmRelease.addEventListener("click", () => {
      if (versionState.releaseURL) window.open(versionState.releaseURL, "_blank", "noopener");
    });
    vmUpgrade.addEventListener("click", async () => {
      const target = versionState.latest || "";
      if (!confirm("确认升级到 " + target + " ?\n升级期间服务会重启 30-60 秒,期间页面短暂不可用。")) return;
      vmUpgrade.disabled = true;
      vmUpgrade.textContent = "已发起升级,稍候…";
      try {
        const r = await fetch("/api/upgrade", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ version: target }),
        });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) {
          alert("升级启动失败: " + (data.error || r.status));
          vmUpgrade.disabled = false;
          vmUpgrade.textContent = "立即升级";
          return;
        }
        vmNotes.textContent =
          "升级已在后台启动 → " + (data.target || target) + "\n" +
          "日志:" + (data.log || "/tmp/argus-upgrade.log") + "\n" +
          (data.hint || "服务将在 30-60 秒内重启,完成后请刷新页面") + "\n\n" +
          "页面会在 60 秒后自动刷新。";
        setTimeout(() => location.reload(), 60_000);
      } catch (e) {
        alert("网络错误: " + e.message);
        vmUpgrade.disabled = false;
        vmUpgrade.textContent = "立即升级";
      }
    });

    // 启动: 拉一次当前版本立刻显示, 然后后台静默检查一次
    loadVersion().then(() => {
      // 静默自动 check 一次, 拿到 has_update 让徽章变橙
      if (versionState.upgradeOpen) {
        fetch("/api/version/check").then(r => r.ok ? r.json() : null).then(data => {
          if (!data) return;
          versionState.latest    = data.latest || "";
          versionState.hasUpdate = !!data.has_update;
          versionState.releaseURL = data.release_url || "";
          versionState.notes     = data.notes || "";
          versionState.checked   = true;
          renderVersionPill();
        }).catch(() => {});
      }
    });
  })();
})();
