package web

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Settings holds dashboard-level user preferences persisted to JSON.
//
// Currently:
//   - MeMAC       the MAC tagged as "my phone" for worktime tracking
//   - WorkStart   standard workday start, HH:MM (default "09:00")
//   - WorkEnd     standard workday end, HH:MM (default "18:30")
type Settings struct {
	MeMAC     string `json:"me_mac,omitempty"`
	WorkStart string `json:"work_start,omitempty"`
	WorkEnd   string `json:"work_end,omitempty"`
}

// SettingsStore is a tiny JSON-file-backed settings store, mirroring
// the atomic-write pattern from AliasStore. Zero-dep by policy.
type SettingsStore struct {
	path string

	mu   sync.RWMutex
	data Settings
}

// NewSettingsStore constructs a settings store backed by path. Pass an
// empty path for in-memory (testing). Missing or corrupt files are
// treated as empty defaults.
func NewSettingsStore(path string) *SettingsStore {
	s := &SettingsStore{
		path: path,
		data: Settings{WorkStart: "09:00", WorkEnd: "18:30"},
	}
	s.load()
	return s
}

func (s *SettingsStore) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var d Settings
	if err := json.Unmarshal(b, &d); err != nil {
		return
	}
	if d.WorkStart == "" {
		d.WorkStart = "09:00"
	}
	if d.WorkEnd == "" {
		d.WorkEnd = "18:30"
	}
	d.MeMAC = normalizeMAC(d.MeMAC)
	s.mu.Lock()
	s.data = d
	s.mu.Unlock()
}

// Get returns a snapshot of current settings.
func (s *SettingsStore) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data
}

// Update applies a partial update. Fields with zero values are left
// untouched unless the caller explicitly wants to clear MeMAC via the
// dedicated ClearMe path (/api/settings DELETE me). WorkStart / WorkEnd
// validate as HH:MM and must satisfy start < end.
func (s *SettingsStore) Update(in Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data
	if in.MeMAC != "" {
		next.MeMAC = normalizeMAC(in.MeMAC)
	}
	if in.WorkStart != "" {
		if _, ok := parseHHMM(in.WorkStart); !ok {
			return errors.New("web: work_start must be HH:MM")
		}
		next.WorkStart = in.WorkStart
	}
	if in.WorkEnd != "" {
		if _, ok := parseHHMM(in.WorkEnd); !ok {
			return errors.New("web: work_end must be HH:MM")
		}
		next.WorkEnd = in.WorkEnd
	}
	sMin, _ := parseHHMM(next.WorkStart)
	eMin, _ := parseHHMM(next.WorkEnd)
	if eMin <= sMin {
		return errors.New("web: work_end must be after work_start")
	}
	s.data = next
	return s.persistLocked()
}

// ClearMe removes the "my phone" tag without touching work hours.
func (s *SettingsStore) ClearMe() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.MeMAC = ""
	return s.persistLocked()
}

func (s *SettingsStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s.data, "", "  ")
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
