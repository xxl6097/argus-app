// settings.js — ⚙ system settings modal + backup export/import.
//
// The modal opens on "btn-settings" click and exposes:
//   - global webhook URL + dingtalk keyword (POST /api/settings)
//   - app branding (POST /api/settings + reload)
//   - account: forward clicks to hidden btn-passwd / btn-logout
//   - system: forward clicks to hidden btn-restart-net / btn-reboot
//   - openid whitelist (POST /api/openids; section hidden when caps.openids
//     reports false — i.e. credentials disabled or store not attached)
//   - backup: download tar.gz / upload + import (with import confirm modal)
//
// Hidden DOM buttons are wired in auth.js (passwd/logout) and system.js
// (reboot/restart-net) — this module is purely the settings-modal UX.

import { state, esc } from "./state.js";

export function initSettings() {
  const settingsBtn = document.getElementById("btn-settings");
  const sm          = document.getElementById("settings-modal");
  if (!settingsBtn || !sm) return;
  const inWebhook   = document.getElementById("set-global-webhook");
  const inKeyword   = document.getElementById("set-webhook-keyword");
  const saveBtn     = document.getElementById("set-save-webhook");
  const saveMsg     = document.getElementById("set-save-msg");
  const inBrandTitle    = document.getElementById("set-brand-title");
  const inBrandSubtitle = document.getElementById("set-brand-subtitle");
  const saveBrandBtn    = document.getElementById("set-save-brand");
  const brandMsg        = document.getElementById("set-brand-msg");
  const closeBtn    = document.getElementById("set-close");

  function setMsg(text, kind) {
    saveMsg.textContent = text || "";
    saveMsg.className = "set-msg" + (kind ? " " + kind : "");
  }

  function setBrandMsg(text, kind) {
    if (!brandMsg) return;
    brandMsg.textContent = text || "";
    brandMsg.className = "set-msg" + (kind ? " " + kind : "");
  }

  async function loadGlobalWebhook() {
    setMsg("");
    setBrandMsg("");
    try {
      const r = await fetch("/api/settings", { cache: "no-store" });
      if (r.ok) {
        const j = await r.json();
        inWebhook.value = j.global_webhook_url || "";
        if (inKeyword) inKeyword.value = j.webhook_keyword || "";
        if (inBrandTitle)    inBrandTitle.value    = j.brand_title    || "";
        if (inBrandSubtitle) inBrandSubtitle.value = j.brand_subtitle || "";
      }
    } catch (_) { /* best-effort */ }
  }

  function openModal() {
    sm.classList.add("show");
    loadGlobalWebhook();
    loadOpenIDs();
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

  // Brand title / subtitle: full reload after save so the header,
  // page <title>, and login page all reflect the new strings without
  // a hard refresh from the user.
  if (saveBrandBtn) {
    saveBrandBtn.addEventListener("click", async () => {
      const title    = inBrandTitle    ? inBrandTitle.value.trim()    : "";
      const subtitle = inBrandSubtitle ? inBrandSubtitle.value.trim() : "";
      saveBrandBtn.disabled = true;
      const orig = saveBrandBtn.textContent;
      saveBrandBtn.textContent = "保存中…";
      try {
        const r = await fetch("/api/settings", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ brand_title: title, brand_subtitle: subtitle }),
        });
        if (r.ok) {
          setBrandMsg("✓ 已保存, 即将刷新…", "ok");
          setTimeout(() => location.reload(), 600);
        } else {
          const b = await r.json().catch(() => ({}));
          setBrandMsg("保存失败: " + (b.error || r.status), "err");
        }
      } catch (e) {
        setBrandMsg("网络错误: " + e.message, "err");
      } finally {
        saveBrandBtn.disabled = false;
        saveBrandBtn.textContent = orig;
      }
    });
  }

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

  // ---- OpenID 免登录白名单 ----
  // Section visibility is keyed off caps.openids so binaries built without
  // the openid store (or with credentials disabled) silently hide it.
  const oidSec   = document.getElementById("set-openid-sec");
  const oidIn    = document.getElementById("set-openid-input");
  const oidAdd   = document.getElementById("set-openid-add");
  const oidList  = document.getElementById("set-openid-list");
  const oidMsg   = document.getElementById("set-openid-msg");

  function setOidMsg(text, kind) {
    if (!oidMsg) return;
    oidMsg.textContent = text || "";
    oidMsg.className = "set-msg" + (kind ? " " + kind : "");
  }

  function renderOpenIDs(list) {
    if (!oidList) return;
    if (!list || list.length === 0) {
      oidList.innerHTML = '<li style="color:var(--muted);padding:6px 0">白名单为空</li>';
      return;
    }
    oidList.innerHTML = list.map(oid =>
      '<li style="display:flex;align-items:center;gap:8px;padding:4px 0;border-bottom:1px solid var(--line)">' +
        '<code style="flex:1;min-width:0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap" title="' + esc(oid) + '">' + esc(oid) + '</code>' +
        '<button class="hdr-btn" data-oid-del="' + esc(oid) + '" style="padding:2px 8px;font-size:12px">删除</button>' +
      '</li>'
    ).join("");
  }

  async function loadOpenIDs() {
    if (!oidSec) return;
    // caps may not have arrived yet on the very first modal open; in that
    // case fall back to probing /api/openids and treating 503 as "feature off".
    if (state.caps && state.caps.openids === false) {
      oidSec.style.display = "none";
      return;
    }
    setOidMsg("");
    try {
      const r = await fetch("/api/openids", { cache: "no-store" });
      if (r.status === 503) {
        oidSec.style.display = "none";
        return;
      }
      if (!r.ok) {
        oidSec.style.display = "";
        setOidMsg("加载失败: " + r.status, "err");
        return;
      }
      const j = await r.json();
      oidSec.style.display = "";
      renderOpenIDs(j.openids || []);
    } catch (e) {
      setOidMsg("网络错误: " + e.message, "err");
    }
  }

  async function addOpenID() {
    if (!oidIn) return;
    const v = oidIn.value.trim();
    if (!v) { setOidMsg("请输入 OpenID", "err"); return; }
    oidAdd.disabled = true;
    try {
      const r = await fetch("/api/openids", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ openid: v }),
      });
      const b = await r.json().catch(() => ({}));
      if (!r.ok) {
        setOidMsg("添加失败: " + (b.error || r.status), "err");
        return;
      }
      oidIn.value = "";
      renderOpenIDs(b.openids || []);
      setOidMsg("✓ 已添加", "ok");
    } catch (e) {
      setOidMsg("网络错误: " + e.message, "err");
    } finally {
      oidAdd.disabled = false;
    }
  }

  if (oidAdd) oidAdd.addEventListener("click", addOpenID);
  if (oidIn)  oidIn.addEventListener("keydown", (ev) => {
    if (ev.key === "Enter") { ev.preventDefault(); addOpenID(); }
  });
  if (oidList) oidList.addEventListener("click", async (ev) => {
    const btn = ev.target.closest("[data-oid-del]");
    if (!btn) return;
    const oid = btn.getAttribute("data-oid-del");
    if (!confirm('确定删除 OpenID "' + oid + '"?\n该用户将无法再免登录。')) return;
    try {
      const r = await fetch("/api/openids?openid=" + encodeURIComponent(oid), { method: "DELETE" });
      const b = await r.json().catch(() => ({}));
      if (!r.ok) {
        setOidMsg("删除失败: " + (b.error || r.status), "err");
        return;
      }
      renderOpenIDs(b.openids || []);
      setOidMsg("✓ 已删除", "ok");
    } catch (e) {
      setOidMsg("网络错误: " + e.message, "err");
    }
  });
}
