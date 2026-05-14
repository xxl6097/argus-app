package web

import (
	"os"
	"path/filepath"
	"testing"
)

// TestNotifyStoreSetNeverDeletes 守护一条核心不变量:
// notifications.json 只能通过用户在 UI 上的明确删除动作清理 (Delete),
// 任何 Set 调用 — 即便所有字段都是空 — 都必须保留这一行。这避免
// alias 重命名 / 订阅刷新 / 校准等代码路径误删用户配置。
func TestNotifyStoreSetNeverDeletes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notifications.json")
	s := NewNotifyStore(path, nil)

	const mac = "AA:BB:CC:DD:EE:01"
	cfg := NotifyConfig{
		WebhookURL:   "https://example.com/hook",
		NtfyServer:   "https://ntfy.sh",
		NtfyReqTopic: "argus-test-req",
	}
	if err := s.Set(mac, cfg); err != nil {
		t.Fatalf("Set initial: %v", err)
	}
	if _, ok := s.Lookup(mac); !ok {
		t.Fatal("Lookup after Set: row missing")
	}

	// 关键场景: 调用 Set 传入完全空 cfg。旧实现会把这一行删掉,
	// 新实现必须把空 cfg 原样存下来 (也就是说该行依然存在,只是
	// 各字段为空)。
	if err := s.Set(mac, NotifyConfig{}); err != nil {
		t.Fatalf("Set empty: %v", err)
	}
	got, ok := s.Lookup(mac)
	if !ok {
		t.Fatal("Set(empty) erroneously deleted the row")
	}
	if got.WebhookURL != "" || got.NtfyServer != "" {
		t.Fatalf("Set(empty) should clear fields but keep the row, got %+v", got)
	}

	// Delete 是唯一允许移除一行的入口。
	if err := s.Delete(mac); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Lookup(mac); ok {
		t.Fatal("Lookup after Delete: row still present")
	}

	// Delete 对不存在的 MAC 应当幂等。
	if err := s.Delete("FF:FF:FF:FF:FF:FF"); err != nil {
		t.Fatalf("Delete on missing row should be idempotent, got %v", err)
	}
}

// TestNotifyStorePersistMode 守护一个安全要求: notifications.json
// 含 ntfy basic-auth 凭证, 必须以 0600 落盘, 不能让 router 上的
// 非 root 用户能读到。
func TestNotifyStorePersistMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notifications.json")
	s := NewNotifyStore(path, nil)
	if err := s.Set("AA:BB:CC:DD:EE:02", NotifyConfig{WebhookURL: "https://example.com/h"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("notifications.json mode = %o, want 0600", got)
	}
}
