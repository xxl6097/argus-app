package web

import (
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Settings holds dashboard-level user preferences persisted to JSON.
//
// Fields:
//   - PunchMACs   MACs tagged as 打卡设备 for worktime tracking (any number)
//   - WorkStart   standard workday start, HH:MM (default "09:00")
//   - WorkEnd     standard workday end, HH:MM (default "18:30")
//
// Legacy field (kept on disk for backward compat):
//   - MeMAC       single-MAC predecessor of PunchMACs. On load, if
//     PunchMACs is empty but MeMAC is set, we fold MeMAC into the list
//     and drop the legacy field on next persist.
type Settings struct {
	PunchMACs        []string `json:"punch_macs,omitempty"`
	MeMAC            string   `json:"me_mac,omitempty"` // legacy; superseded by PunchMACs
	WorkStart        string   `json:"work_start,omitempty"`
	WorkEnd          string   `json:"work_end,omitempty"`
	GlobalWebhookURL string   `json:"global_webhook_url,omitempty"`
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
	// Migrate legacy single MeMAC → PunchMACs list on first load.
	// Normalize every entry and deduplicate, then discard MeMAC so the
	// next persist drops it from disk.
	seen := make(map[string]struct{})
	merged := make([]string, 0, len(d.PunchMACs)+1)
	for _, m := range d.PunchMACs {
		m = normalizeMAC(m)
		if m == "" {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		merged = append(merged, m)
	}
	if legacy := normalizeMAC(d.MeMAC); legacy != "" {
		if _, dup := seen[legacy]; !dup {
			merged = append(merged, legacy)
			seen[legacy] = struct{}{}
		}
	}
	sort.Strings(merged)
	d.PunchMACs = merged
	d.MeMAC = "" // drop legacy
	s.mu.Lock()
	s.data = d
	s.mu.Unlock()
}

// Get returns a snapshot of current settings. The returned slice is
// safe to hold — it's copied.
func (s *SettingsStore) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.data
	if len(s.data.PunchMACs) > 0 {
		out.PunchMACs = append([]string(nil), s.data.PunchMACs...)
	}
	return out
}

// IsPunch reports whether mac is currently tagged as a 打卡设备.
func (s *SettingsStore) IsPunch(mac string) bool {
	mac = normalizeMAC(mac)
	if mac == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.data.PunchMACs {
		if m == mac {
			return true
		}
	}
	return false
}

// AddPunch adds mac to the 打卡设备 set. No-op if already present.
func (s *SettingsStore) AddPunch(mac string) error {
	mac = normalizeMAC(mac)
	if mac == "" {
		return errors.New("web: punch mac required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.data.PunchMACs {
		if m == mac {
			return nil
		}
	}
	s.data.PunchMACs = append(s.data.PunchMACs, mac)
	sort.Strings(s.data.PunchMACs)
	return s.persistLocked()
}

// RemovePunch removes mac from the 打卡设备 set. No-op if absent.
func (s *SettingsStore) RemovePunch(mac string) error {
	mac = normalizeMAC(mac)
	if mac == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.data.PunchMACs[:0]
	for _, m := range s.data.PunchMACs {
		if m != mac {
			out = append(out, m)
		}
	}
	s.data.PunchMACs = append([]string{}, out...)
	return s.persistLocked()
}

// Update applies a partial update to work-hour fields. MAC-set edits
// should go through AddPunch / RemovePunch. WorkStart / WorkEnd
// validate as HH:MM (or HH:MM:SS via parseClock) and must satisfy
// start < end.
func (s *SettingsStore) Update(in Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data
	if in.WorkStart != "" {
		if _, ok := parseClock(in.WorkStart); !ok {
			return errors.New("web: work_start must be HH:MM")
		}
		next.WorkStart = in.WorkStart
	}
	if in.WorkEnd != "" {
		if _, ok := parseClock(in.WorkEnd); !ok {
			return errors.New("web: work_end must be HH:MM")
		}
		next.WorkEnd = in.WorkEnd
	}
	sSec, _ := parseClock(next.WorkStart)
	eSec, _ := parseClock(next.WorkEnd)
	if eSec <= sSec {
		return errors.New("web: work_end must be after work_start")
	}
	s.data = next
	return s.persistLocked()
}

// ClearPunchAll wipes the entire 打卡设备 set.
func (s *SettingsStore) ClearPunchAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.PunchMACs = nil
	return s.persistLocked()
}

// ClearMe is kept for callsite compat (old DELETE /api/settings?clear=me).
// Equivalent to ClearPunchAll.
func (s *SettingsStore) ClearMe() error { return s.ClearPunchAll() }

func (s *SettingsStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	// Strip legacy MeMAC on write so the file converges on the new shape.
	// Keep an empty array out of the JSON when no punch MACs are set.
	out := s.data
	out.MeMAC = ""
	if len(out.PunchMACs) == 0 {
		out.PunchMACs = nil
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

// SetGlobalWebhook sets or clears the global webhook URL. An empty string
// clears it. Non-empty values must pass url.ParseRequestURI.
func (s *SettingsStore) SetGlobalWebhook(raw string) error {
	if raw != "" {
		if _, err := url.ParseRequestURI(raw); err != nil {
			return errors.New("web: global_webhook_url must be a valid URL")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.GlobalWebhookURL = raw
	return s.persistLocked()
}

// PunchMACsUpper returns the 打卡设备 MACs as uppercase strings for
// wire formats that prefer display case (e.g. /api/settings GET).
func (s *SettingsStore) PunchMACsUpper() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.data.PunchMACs))
	for i, m := range s.data.PunchMACs {
		out[i] = strings.ToUpper(m)
	}
	return out
}
