// dhcp.js — static IP modal + DHCP apply toast.
//
// openStaticIPModal opens a dialog to assign / change / clear a
// device's static reservation. showApplyToast renders a follow-up
// toast that summarises which DHCP daemons reloaded, which lease
// files were pruned, and whether the per-station kick fired.

import { esc, cssEsc, state } from "./state.js";

// openStaticIPModal shows a modal asking for an IP to reserve.
export function openStaticIPModal(mac) {
  const leases = state.staticLeases || {};
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
        state.reloadDevices();
        showApplyToast(b2.apply || {}, "已替换");
        return;
      }
      if (!r.ok) {
        alert("操作失败: " + (b.error || r.status));
        return;
      }
      close();
      state.reloadDevices();
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
      state.reloadDevices();
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

// the device is getting a fresh IP NOW or needs to renew on its own.
export function showApplyToast(apply, action) {
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
