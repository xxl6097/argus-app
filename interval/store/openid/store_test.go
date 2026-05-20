package openid

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStoreCRUD covers the full lifecycle: add, dedupe, has, all,
// remove, clear. All assertions read back through the public API.
func TestStoreCRUD(t *testing.T) {
	dir := t.TempDir()
	s := New(filepath.Join(dir, "openids.json"))

	if got := s.All(); len(got) != 0 {
		t.Fatalf("fresh store should be empty, got %v", got)
	}
	if s.Has("anything") {
		t.Fatal("Has on empty store should be false")
	}

	if err := s.Add("wx_aaa"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add("wx_bbb"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add("wx_aaa"); err != nil {
		t.Fatalf("Add (dup) should be no-op, got %v", err)
	}
	if err := s.Add("  wx_ccc  "); err != nil {
		t.Fatalf("Add (whitespace): %v", err)
	}
	if !s.Has("wx_aaa") || !s.Has("wx_ccc") {
		t.Fatalf("Has should hit after Add")
	}
	if s.Has("wx_zzz") {
		t.Fatal("Has should miss for absent")
	}
	if got := s.All(); len(got) != 3 {
		t.Fatalf("All after 3 adds (1 dup) want 3, got %d (%v)", len(got), got)
	}

	// Empty + oversize input rejection.
	if err := s.Add(""); err == nil {
		t.Fatal("Add empty should error")
	}
	long := make([]byte, MaxLen+1)
	for i := range long {
		long[i] = 'a'
	}
	if err := s.Add(string(long)); err == nil {
		t.Fatal("Add oversize should error")
	}

	if err := s.Remove("wx_bbb"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if s.Has("wx_bbb") {
		t.Fatal("Has should miss after Remove")
	}
	if err := s.Remove("wx_bbb"); err != nil {
		t.Fatalf("Remove (absent) should be idempotent, got %v", err)
	}

	if err := s.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if got := s.All(); len(got) != 0 {
		t.Fatalf("All after Clear want 0, got %v", got)
	}
}

// TestStorePersistMode守护安全要求: openids.json 是登录凭据,
// 必须 0600.
func TestStorePersistMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openids.json")
	s := New(path)
	if err := s.Add("wx_secret"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("openids.json mode = %o, want 0600", got)
	}
}

// TestStoreReload survives a backup-style file overwrite.
func TestStoreReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "openids.json")
	s := New(path)
	if err := s.Add("wx_orig"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Simulate an external (backup-import) overwrite.
	if err := os.WriteFile(path, []byte(`["wx_imported_a","wx_imported_b"]`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	s.Reload()
	if s.Has("wx_orig") {
		t.Fatal("Reload should drop pre-overwrite entries")
	}
	if !s.Has("wx_imported_a") || !s.Has("wx_imported_b") {
		t.Fatalf("Reload should pick up imported entries, got %v", s.All())
	}
}
