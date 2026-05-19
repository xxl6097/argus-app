// auth.js — fetch interceptor + login probe + logout/passwd buttons.
//
// Called once at boot from main.js. The fetch wrapper:
//   - injects credentials: same-origin
//   - injects X-Requested-With so the server treats us as XHR (returns 401
//     JSON instead of redirecting to /login)
//   - intercepts 401 responses and redirects the page to /login?next=...
// The probe of /api/devices doubles as login state detection: 200 = logged
// in, show ⚙ settings button; 401 = will be handled by the wrapper.

export function installAuth() {
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
}
