// Package alias persists a MAC → friendly-name map.
//
// It's the dashboard-layer answer to "iOS gives me only the MAC, I
// want to see 'Alice's iPhone'". Watcher and library output is
// unchanged; alias merging happens at the /api/devices rendering
// boundary.
//
// Concurrency: any number of readers; serialised writes via internal
// mutex. Persistence is best-effort with atomic (write-tmp + rename)
// updates so a crash mid-write cannot leave a torn JSON file.
//
// File format (JSON object, MAC keys uppercased for human readability):
//
//	{
//	    "AA:BB:CC:DD:EE:FF": "Alice's iPhone",
//	    "BC:F1:71:EB:AA:64": "GKXM mini"
//	}
//
// MAC matching is case-insensitive.
package alias

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/xxl6097/argus-app/interval/util"
)

// Store is a MAC → friendly-name map persisted to JSON.
type Store struct {
	path string

	mu   sync.RWMutex
	data map[string]string // normalized lowercase MAC -> alias
}

// New constructs a store backed by the given file path. Empty path =
// in-memory only (writes survive in the process but not across restarts;
// useful for tests). Missing / corrupt files are treated as empty —
// the next successful Set() replaces them.
func New(path string) *Store {
	s := &Store{path: path, data: make(map[string]string)}
	s.load()
	return s
}

// Reload re-reads the file from disk, replacing the in-memory map.
// Used after backup import overwrites the JSON file.
func (s *Store) Reload() {
	s.mu.Lock()
	s.data = make(map[string]string)
	s.mu.Unlock()
	s.load()
}

func (s *Store) load() {
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
		out[util.NormalizeMAC(k)] = v
	}
	s.mu.Lock()
	s.data = out
	s.mu.Unlock()
}

// Lookup returns the friendly name for a MAC, or "" if no alias is
// set. MAC matching is case-insensitive.
func (s *Store) Lookup(mac string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data[util.NormalizeMAC(mac)]
}

// All returns a snapshot of every alias as MAC(uppercase) -> name.
// The returned map is independent of the store's internal state.
func (s *Store) All() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.data))
	for k, v := range s.data {
		out[strings.ToUpper(k)] = v
	}
	return out
}

// Set stores or updates an alias for a MAC. An empty or whitespace-only
// name removes the alias entirely. Returns an error if MAC is empty or
// persistence fails. Persistence is atomic.
func (s *Store) Set(mac, name string) error {
	mac = util.NormalizeMAC(mac)
	if mac == "" {
		return errors.New("alias: mac required")
	}
	name = strings.TrimSpace(name)
	if len(name) > 64 {
		return fmt.Errorf("alias: name too long (%d > 64)", len(name))
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

func (s *Store) persistLocked() error {
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
	return util.WriteJSONAtomic(s.path, b, 0o644)
}
