// state.js — shared DOM refs, mutable state, and small utilities.
//
// Mutable state (eventsStarted, currentSettings, _caps, _staticLeases) is
// exposed via a single `state` object so consumers always read fresh values.
// Plain `let` exports would freeze imports at first read.

// ---- DOM refs ----

export const elBody         = document.getElementById("devices-body");
export const elCards        = document.getElementById("devices-cards");
export const elEvents       = document.getElementById("events-list");
export const elCountOnline  = document.getElementById("count-online");
export const elCountOffline = document.getElementById("count-offline");
export const elConn         = document.getElementById("conn");

// ---- Constants ----

export const MAX_EVENT_LINES = 200;

// ---- Mutable state shared across modules ----
//
// Read via state.foo (always fresh) instead of importing the field
// directly (which would be frozen at import time).
export const state = {
  eventsStarted: false,
  // Set of MACs whose detail row is currently open. Survives reloadDevices()
  // (which blows away <tr>s) so the re-render can re-attach the expanded panel.
  openDetailRows: new Set(),
  // Cache of the last /api/settings snapshot so we can build the worktime
  // panel without an extra roundtrip.
  currentSettings: null,
  // Last /api/devices capabilities map (set by reloadDevices).
  caps: {},
  // Last /api/dhcp lease snapshot (set by reloadDevices when caps.dhcp).
  staticLeases: {},
  // Reference to reloadDevices() — registered by main.js at boot so other
  // modules can trigger a refresh without importing main (which would
  // create a circular dep). Always check state.reloadDevices != null
  // before calling — pre-boot calls (theoretical) would no-op.
  reloadDevices: null,
};

// ---- Pure utilities ----

export function rssiClass(r) {
  if (!r || r === 0) return "";
  if (r >= -55) return "rssi-strong";
  if (r >= -70) return "rssi-medium";
  if (r >= -80) return "rssi-weak";
  return "rssi-vweak";
}

export function esc(s) {
  if (s == null) return "";
  return String(s).replace(/[&<>"']/g, c => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
  })[c]);
}

export function linkText(d) {
  if (d.wired) return "有线";
  return [d.radio, d.ssid && "/" + d.ssid].filter(Boolean).join("");
}

export function rssiText(d) {
  return d.rssi ? d.rssi + " dBm" : "—";
}

export function cssEsc(s) { return String(s).replace(/"/g, '\\"'); }

export function fmtClock(ms) {
  if (!ms) return "—";
  return new Date(ms).toLocaleTimeString([], {hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false});
}

export function secsToHM(secs) {
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
export function parseYMD(s) {
  if (!s) return null;
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(s.trim());
  if (!m) return null;
  return new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]));
}

export function fmtYMD(d) {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const da = String(d.getDate()).padStart(2, "0");
  return y + "-" + m + "-" + da;
}

export function formatLocalDate(d) {
  const y = d.getFullYear();
  const m = String(d.getMonth() + 1).padStart(2, "0");
  const dd = String(d.getDate()).padStart(2, "0");
  return y + "-" + m + "-" + dd;
}

// shiftDate(YYYY-MM-DD, n) → YYYY-MM-DD shifted by n days.
export function shiftDate(s, n) {
  const [y, m, d] = s.split("-").map(Number);
  const dt = new Date(y, m - 1, d);
  dt.setDate(dt.getDate() + n);
  return formatLocalDate(dt);
}

export function relativeTime(ms) {
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
