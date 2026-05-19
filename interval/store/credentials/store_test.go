package credentials

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCredentialsSeedDefault 验证首次启动会自动落盘 admin/admin +
// must_change=true, 文件权限 0600。
func TestCredentialsSeedDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	cs := New(path)

	if got := cs.Username(); got != "admin" {
		t.Fatalf("Username = %q, want admin", got)
	}
	if !cs.MustChange() {
		t.Fatal("MustChange should be true on first boot")
	}
	if !cs.Verify("admin", "admin") {
		t.Fatal("default admin/admin should verify")
	}
	if cs.Verify("admin", "wrong") {
		t.Fatal("wrong password must not verify")
	}
	if cs.Verify("root", "admin") {
		t.Fatal("wrong username must not verify")
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("credentials.json mode = %o, want 0600", got)
	}
}

// TestCredentialsChangePassword 验证改密成功后 must_change 清除,
// 旧密码不再可用, 新密码可用; 改密后会落盘并被下次 New
// 读到 (不会再 seed)。
func TestCredentialsChangePassword(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	cs := New(path)

	if err := cs.ChangePassword("wrong-old", "newpass1"); err == nil {
		t.Fatal("wrong old password should error")
	}
	if err := cs.ChangePassword("admin", "abc"); err == nil {
		t.Fatal("too-short new password should error")
	}
	if err := cs.ChangePassword("admin", "newpass1"); err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if cs.MustChange() {
		t.Fatal("MustChange should be false after change")
	}
	if cs.Verify("admin", "admin") {
		t.Fatal("old password must no longer work")
	}
	if !cs.Verify("admin", "newpass1") {
		t.Fatal("new password must work")
	}

	// 重新 load 后应保留新 hash, 不被 seedDefault 覆盖
	cs2 := New(path)
	if cs2.MustChange() {
		t.Fatal("reload should preserve must_change=false")
	}
	if !cs2.Verify("admin", "newpass1") {
		t.Fatal("reload should preserve new password")
	}
}

// TestSessionStoreLifecycle 验证 Issue / Validate / Revoke / 过期。
func TestSessionStoreLifecycle(t *testing.T) {
	ss := NewSessionStore()
	tok, err := ss.Issue("admin")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if tok == "" {
		t.Fatal("token must not be empty")
	}
	if user, ok := ss.Validate(tok); !ok || user != "admin" {
		t.Fatalf("Validate fresh token: ok=%v user=%q", ok, user)
	}

	// Revoke 后 Validate 应失败
	ss.Revoke(tok)
	if _, ok := ss.Validate(tok); ok {
		t.Fatal("Validate after Revoke should fail")
	}

	// 空 token / 未知 token
	if _, ok := ss.Validate(""); ok {
		t.Fatal("empty token must not validate")
	}
	if _, ok := ss.Validate("nonexistent"); ok {
		t.Fatal("unknown token must not validate")
	}

	// 过期 token
	tok2, _ := ss.Issue("admin")
	ss.mu.Lock()
	sess := ss.byTok[tok2]
	sess.expires = time.Now().Add(-time.Minute) // 强制过期
	ss.byTok[tok2] = sess
	ss.mu.Unlock()
	if _, ok := ss.Validate(tok2); ok {
		t.Fatal("expired token must not validate")
	}
}

// TestSessionStoreRevokeAll 改密场景: 所有会话被踢。
func TestSessionStoreRevokeAll(t *testing.T) {
	ss := NewSessionStore()
	t1, _ := ss.Issue("admin")
	t2, _ := ss.Issue("admin")
	ss.RevokeAll()
	if _, ok := ss.Validate(t1); ok {
		t.Fatal("t1 should be revoked")
	}
	if _, ok := ss.Validate(t2); ok {
		t.Fatal("t2 should be revoked")
	}
}
