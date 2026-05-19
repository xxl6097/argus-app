// version.js — version pill + check-for-update modal + self-upgrade trigger.
//
// loadVersion() fetches /api/version and shows the current version in
// the header. checkUpdate() polls /api/version/check (cached 30 min on
// the server side) and turns the pill orange + 🆙 if a newer release
// is on GitHub. Click → modal shows release notes + "立即升级" which
// POSTs /api/upgrade.

export function initVersion() {
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
}
