// main.js — boot sequence: fetch interceptor → first reload + SSE →
// wire system buttons → register settings/auth/version modules.
//
// `state.reloadDevices` is registered up-front so any module imported
// later can call state.reloadDevices() without a circular import.

import { state, elConn, elEvents, MAX_EVENT_LINES, esc } from "./state.js";
import { renderDevices } from "./devices.js";
import { initSystem } from "./system.js";
import { installAuth } from "./auth.js";
import { initSettings } from "./settings.js";
import { initVersion } from "./version.js";

async function reloadDevices() {
  try {
    const r = await fetch("/api/devices", { cache: "no-store" });
    const body = await r.json();
    state.caps = body.capabilities || {};
    if (state.caps.dhcp) {
      try {
        const lr = await fetch("/api/dhcp", { cache: "no-store" });
        if (lr.ok) {
          const lb = await lr.json();
          state.staticLeases = lb.leases || {};
        }
      } catch (_) { /* non-fatal */ }
    } else {
      state.staticLeases = {};
    }
    if (state.caps.settings && !state.currentSettings) {
      try {
        const sr = await fetch("/api/settings", { cache: "no-store" });
        if (sr.ok) state.currentSettings = await sr.json();
      } catch (_) { /* non-fatal */ }
    }
    renderDevices(body.devices || []);
  } catch (err) {
    console.warn("reload failed", err);
  }
}


function addEventLine(kind, data) {
  if (!state.eventsStarted) {
    elEvents.innerHTML = "";
    state.eventsStarted = true;
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

// --- register state.reloadDevices for other modules ---
state.reloadDevices = reloadDevices;

// --- boot ---
reloadDevices();
connectSSE();
setInterval(reloadDevices, 30000);

initSystem();      // wires hidden btn-reboot + btn-restart-net
installAuth();     // fetch interceptor + login probe + logout/passwd
initSettings();   // ⚙ settings modal (depends on the hidden buttons above)
initVersion();    // version pill + upgrade modal
