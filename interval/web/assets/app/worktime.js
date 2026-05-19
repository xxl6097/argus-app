// worktime.js — 📊 工作时长 tab + 📅 月统计 tab.
//
// renderWorktimePane builds the day-view (month navigator + per-day list +
// drill-down + manual edit). renderMonthlyStatsPane builds the 12-month
// rolling summary. Helpers (openOverrideModal, deleteDayManual,
// renderWorktimeCard, renderMonthSummary, renderMonthDays) stay internal
// to this module.

import { esc, secsToHM, fmtClock, parseYMD, fmtYMD, state } from "./state.js";

export async function renderWorktimePane(body, mac, initialMonth) {
  // Ensure we have the current settings (worktime window).
  if (!state.currentSettings) {
    try {
      const r = await fetch("/api/settings", { cache: "no-store" });
      if (r.ok) state.currentSettings = await r.json();
    } catch (_) { /* best-effort */ }
  }
  const cfg = state.currentSettings || { work_start: "09:00", work_end: "18:30" };
  const defaultMonth = initialMonth || new Date().toISOString().slice(0, 7);
  const caps = state.caps || {};
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
      state.currentSettings = { ...(state.currentSettings || {}), work_start: startIn.value, work_end: endIn.value };
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

export async function renderMonthlyStatsPane(body, mac, onPickMonth) {
  body.innerHTML = '<div class="wt-empty">加载中…</div>';
  const cfg = state.currentSettings || { work_start: "09:00", work_end: "18:30" };
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
