package web

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Override is a manual in/out record for (mac, date). Used when the
// Watcher missed transitions (device stayed online across days, user
// didn't bring the phone, etc.) so the user can still file a correct
// worktime report for that day.
//
// Both fields are HH:MM (24h local). Either or both may be empty —
// the compute path treats missing values as "fall back to history"
// which lets a user correct only the arrival time, for example.
type Override struct {
	In  string `json:"in,omitempty"`
	Out string `json:"out,omitempty"`
}

// OverrideStore persists per-device-per-date manual in/out overrides.
// Wire API is always keyed by MAC, but on-disk the top-level key is
// the device's ALIAS (when one is set) so the file is human-readable.
// Unaliased devices fall back to the MAC so the store still works
// for untagged devices.
//
// Aliases can be renamed; Set() persists under the CURRENT alias at
// write time, and Lookup() tries the current alias first, then the
// raw MAC for legacy entries. If a user renames an alias and their
// pre-existing overrides still appear correctly, that's because the
// alias changed but the on-disk row was written under the previous
// name — future writes re-home it.
//
// Single-file JSON with atomic rename, mirroring AliasStore's pattern.
type OverrideStore struct {
	path    string
	aliases *AliasStore // optional; nil means "always key by MAC"

	mu   sync.RWMutex
	data map[string]map[string]Override // key(alias|MAC) -> "YYYY-MM-DD" -> Override
}

// NewOverrideStore constructs a store backed by path. aliases is
// optional: when attached, top-level JSON keys use the alias (if one
// is set for that MAC) instead of the MAC itself. Pass nil to always
// key by MAC.
//
// Empty path = in-memory only (tests). Missing / corrupt file =
// empty defaults.
func NewOverrideStore(path string, aliases *AliasStore) *OverrideStore {
	s := &OverrideStore{
		path:    path,
		aliases: aliases,
		data:    make(map[string]map[string]Override),
	}
	s.load()
	return s
}

func (s *OverrideStore) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	// On-disk shape (current): alias -> "YYYY-MM" -> "YYYY-MM-DD" -> Override.
	// Decode permissively so we also accept the legacy flat shape
	// (alias -> "YYYY-MM-DD" -> Override) and migrate it on next write.
	// json.RawMessage lets us probe each value's shape without a
	// second pass over the bytes.
	var raw map[string]map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return
	}
	out := make(map[string]map[string]Override, len(raw))
	sawLegacy := false
	for alias, byKey := range raw {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		days := make(map[string]Override)
		for key, val := range byKey {
			key = strings.TrimSpace(key)
			if isDateKey(key) {
				// Legacy flat: key is "YYYY-MM-DD".
				sawLegacy = true
				var o Override
				if err := json.Unmarshal(val, &o); err == nil {
					days[key] = o
				}
				continue
			}
			if isMonthKey(key) {
				// Nested: key is "YYYY-MM"; value is map of dates.
				var month map[string]Override
				if err := json.Unmarshal(val, &month); err == nil {
					for d, o := range month {
						d = strings.TrimSpace(d)
						if isDateKey(d) {
							days[d] = o
						}
					}
				}
				continue
			}
			// Unknown key shape — skip silently. Future versions can
			// extend without breaking older binaries.
		}
		if len(days) == 0 {
			continue
		}
		out[alias] = days
	}
	s.mu.Lock()
	s.data = out
	// Rewrite the file in the new nested shape immediately when we
	// find any legacy entries. Keeps the on-disk file tidy instead of
	// waiting for the next user write.
	if sawLegacy {
		_ = s.persistLocked()
	}
	s.mu.Unlock()
}

// isDateKey reports whether s looks like "YYYY-MM-DD".
func isDateKey(s string) bool {
	return len(s) == 10 && s[4] == '-' && s[7] == '-'
}

// isMonthKey reports whether s looks like "YYYY-MM".
func isMonthKey(s string) bool {
	return len(s) == 7 && s[4] == '-'
}

// keyFor resolves a MAC to its storage key: alias when one exists
// and aliases is attached, otherwise normalized MAC. Empty MAC yields
// empty string — callers should guard.
func (s *OverrideStore) keyFor(mac string) string {
	mac = normalizeMAC(mac)
	if mac == "" {
		return ""
	}
	if s.aliases != nil {
		if name := s.aliases.Lookup(mac); name != "" {
			return name
		}
	}
	return mac
}

// Lookup returns the override for (mac, date) and whether one exists.
// Tries the alias key first, then falls back to the raw MAC so entries
// written before an alias was assigned still resolve.
func (s *OverrideStore) Lookup(mac, date string) (Override, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if key := s.keyFor(mac); key != "" {
		if days, ok := s.data[key]; ok {
			if o, ok := days[date]; ok {
				return o, true
			}
		}
	}
	// Legacy fallback: raw MAC key (covers pre-alias overrides and
	// files written by older versions of this binary).
	macKey := normalizeMAC(mac)
	if macKey == "" {
		return Override{}, false
	}
	if days, ok := s.data[macKey]; ok {
		if o, ok := days[date]; ok {
			return o, true
		}
	}
	// Also try uppercase, since older versions uppercased MAC keys on disk.
	if days, ok := s.data[strings.ToUpper(macKey)]; ok {
		if o, ok := days[date]; ok {
			return o, true
		}
	}
	return Override{}, false
}

// Set writes or replaces the override. Clearing both fields removes
// the entry entirely. HH:MM is validated; in must be strictly before
// out when both are present.
//
// Writes use the current alias (if any) as the top-level key. If a
// legacy MAC-keyed row exists for this device, it's migrated to the
// alias key and the MAC entry is removed — so the on-disk file
// converges on alias keys over time.
func (s *OverrideStore) Set(mac, date string, o Override) error {
	macKey := normalizeMAC(mac)
	if macKey == "" {
		return errors.New("web: override mac required")
	}
	date = strings.TrimSpace(date)
	if date == "" {
		return errors.New("web: override date required (YYYY-MM-DD)")
	}
	o.In = strings.TrimSpace(o.In)
	o.Out = strings.TrimSpace(o.Out)
	if o.In == "" && o.Out == "" {
		return s.clear(macKey, date)
	}
	var inSec, outSec int
	if o.In != "" {
		secs, ok := parseClock(o.In)
		if !ok {
			return errors.New("web: in must be HH:MM or HH:MM:SS")
		}
		inSec = secs
	}
	if o.Out != "" {
		secs, ok := parseClock(o.Out)
		if !ok {
			return errors.New("web: out must be HH:MM or HH:MM:SS")
		}
		outSec = secs
	}
	if o.In != "" && o.Out != "" && outSec <= inSec {
		return errors.New("web: out must be after in")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.keyFor(mac)
	if key == "" {
		key = macKey
	}
	days, ok := s.data[key]
	if !ok {
		days = make(map[string]Override)
		s.data[key] = days
	}
	days[date] = o
	// Migrate legacy MAC-keyed rows into the alias key so the file
	// converges. Only do this when key != mac (i.e. an alias was
	// resolved) to avoid spurious lookups on unaliased devices.
	if key != macKey {
		s.migrateLegacyLocked(macKey, key)
	}
	return s.persistLocked()
}

// Delete removes the override for (mac, date). Tries both alias and
// MAC keys to cover legacy rows.
func (s *OverrideStore) Delete(mac, date string) error {
	return s.clear(normalizeMAC(mac), strings.TrimSpace(date))
}

func (s *OverrideStore) clear(macKey, date string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidates := []string{macKey, strings.ToUpper(macKey)}
	if s.aliases != nil {
		if name := s.aliases.Lookup(macKey); name != "" {
			candidates = append([]string{name}, candidates...)
		}
	}
	changed := false
	for _, k := range candidates {
		if days, ok := s.data[k]; ok {
			if _, ok := days[date]; ok {
				delete(days, date)
				changed = true
			}
			if len(days) == 0 {
				delete(s.data, k)
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return s.persistLocked()
}

// migrateLegacyLocked moves any entries keyed under the raw MAC (in
// either case) under the alias key and deletes the MAC rows. Caller
// must hold s.mu. Entries already present under the alias key take
// precedence — we do not overwrite user-intent with stale rows.
func (s *OverrideStore) migrateLegacyLocked(macKey, aliasKey string) {
	for _, legacy := range []string{macKey, strings.ToUpper(macKey)} {
		if legacy == aliasKey {
			continue
		}
		legacyDays, ok := s.data[legacy]
		if !ok {
			continue
		}
		target := s.data[aliasKey]
		if target == nil {
			target = make(map[string]Override)
			s.data[aliasKey] = target
		}
		for date, o := range legacyDays {
			if _, exists := target[date]; exists {
				continue // keep the alias-keyed row (authoritative)
			}
			target[date] = o
		}
		delete(s.data, legacy)
	}
}

func (s *OverrideStore) persistLocked() error {
	if s.path == "" {
		return nil
	}
	// Group dates by their YYYY-MM month so the file reads naturally
	// (one block per month). Empty entries are dropped.
	out := make(map[string]map[string]map[string]Override, len(s.data))
	for alias, days := range s.data {
		if len(days) == 0 {
			continue
		}
		months := make(map[string]map[string]Override)
		for date, o := range days {
			if !isDateKey(date) {
				continue
			}
			month := date[:7]
			m := months[month]
			if m == nil {
				m = make(map[string]Override)
				months[month] = m
			}
			m[date] = o
		}
		if len(months) > 0 {
			out[alias] = months
		}
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
