package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// AliasStore is a MAC -> friendly-name map persisted to a JSON file.
//
// It is the dashboard-layer answer to the "iOS gives me only the MAC,
// I want to see 'Alice's iPhone'" use case. The Watcher and library
// surface are unchanged: alias merging happens at the /api/devices
// rendering boundary, so existing JSON consumers see the original
// hostname unchanged plus a new optional `alias` field.
//
// Concurrency: safe for any number of readers; serializes writes via
// an internal mutex. Persistence is best-effort with atomic
// (write-tmp + rename) updates so a crash mid-write cannot leave a
// torn JSON file.
//
// File format (JSON object, MAC keys uppercased for human readability):
//
//	{
//	    "AA:BB:CC:DD:EE:FF": "Alice's iPhone",
//	    "BC:F1:71:EB:AA:64": "GKXM mini"
//	}
//
// The on-disk format is the stable wire shape from v0.14.0 onward
// (see STABILITY.md). MAC normalization is case-insensitive.
type AliasStore struct {
	path string

	mu   sync.RWMutex
	data map[string]string // normalized lowercase MAC -> alias
}

// NewAliasStore constructs a store backed by the given file path.
// If path is empty, the store is in-memory only (writes are kept in
// the process but not persisted across restarts; useful for tests).
//
// The constructor is best-effort: a missing file is treated as an
// empty store; a corrupt file logs nothing (per the package's
// no-third-party-deps policy) and is treated as empty as well —
// the next successful write replaces it.
func NewAliasStore(path string) *AliasStore {
	s := &AliasStore{path: path, data: make(map[string]string)}
	s.load()
	return s
}

// Reload re-reads the alias file from disk, replacing the in-memory
// map. Used after /api/backup/import overwrites the JSON file.
func (s *AliasStore) Reload() {
	s.mu.Lock()
	s.data = make(map[string]string)
	s.mu.Unlock()
	s.load()
}

func (s *AliasStore) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		// Treat corrupt file as empty — next Set() will overwrite it.
		return
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		out[normalizeMAC(k)] = v
	}
	s.mu.Lock()
	s.data = out
	s.mu.Unlock()
}

// Lookup returns the friendly name for a MAC, or "" if no alias is
// set. MAC matching is case-insensitive.
func (s *AliasStore) Lookup(mac string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data[normalizeMAC(mac)]
}

// All returns a snapshot of every alias as MAC(uppercase) -> name.
// The returned map is independent of the store's internal state.
func (s *AliasStore) All() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.data))
	for k, v := range s.data {
		out[strings.ToUpper(k)] = v
	}
	return out
}

// Set stores or updates an alias for a MAC. An empty or
// whitespace-only name removes the alias entirely. Returns an error
// if MAC is empty or persistence fails. Persistence is atomic:
// a successful Set means the new state is durable on disk before
// the call returns (when path is non-empty).
func (s *AliasStore) Set(mac, name string) error {
	mac = normalizeMAC(mac)
	if mac == "" {
		return errors.New("web: alias mac required")
	}
	name = strings.TrimSpace(name)
	if len(name) > 64 {
		return fmt.Errorf("web: alias name too long (%d > 64)", len(name))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		delete(s.data, mac)
	} else {
		s.data[mac] = name
	}
	return s.persistLocked()
}

// persistLocked writes the in-memory map to disk atomically. Caller
// must hold s.mu. No-op when path is empty (in-memory mode).
func (s *AliasStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	out := make(map[string]string, len(s.data))
	for k, v := range s.data {
		out[strings.ToUpper(k)] = v
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
