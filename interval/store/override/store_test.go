package override

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOverrideStore_Migration verifies the flat → nested migration on load.
func TestOverrideStore_Migration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overrides.json")
	// Write old flat format: {alias: {YYYY-MM-DD: {in, out}}}
	flat := `{
  "iphone17": {
    "2026-05-13": {"in": "08:40", "out": "19:15"},
    "2026-05-14": {"in": "09:00", "out": "18:30"}
  }
}`
	if err := os.WriteFile(path, []byte(flat), 0644); err != nil {
		t.Fatal(err)
	}
	store := New(path, nil)
	// After load, it should auto-migrate to nested.
	got, ok := store.Lookup("iphone17", "2026-05-13")
	if !ok {
		t.Fatal("expected override for 2026-05-13")
	}
	if got.In != "08:40" || got.Out != "19:15" {
		t.Errorf("got {%q, %q}, want {08:40, 19:15}", got.In, got.Out)
	}
	// Check the on-disk format is now nested.
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"2026-05"`) {
		t.Errorf("expected nested month key 2026-05 in migrated file, got:\n%s", raw)
	}
}
