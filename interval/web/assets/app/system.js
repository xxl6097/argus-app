// system.js — wires the reboot + restart-network buttons.
//
// Both buttons are double-confirm because they take down the LAN
// (reboot: 30-60s, restart-net: 5-15s blip). They live in DOM as hidden
// `<button id="btn-reboot">` / `<button id="btn-restart-net">` so the ⚙
// settings modal can forward clicks to them via .click().

export function initSystem() {

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
}
