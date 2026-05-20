package settings

import (
	"encoding/json"
	"errors"
	"github.com/xxl6097/argus-app/interval/util"
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
	// WebhookKeyword is appended to every webhook body (markdown text
	// + title) so dingtalk/feishu robots that have keyword filters
	// configured will let the message through. Empty = no append.
	WebhookKeyword string `json:"webhook_keyword,omitempty"`
	// WebhookMACs is the per-device opt-in list for the global webhook.
	// Only events from MACs in this set fire the global webhook URL —
	// independent of whether the device has its own per-device notify
	// config. Toggled from the per-device 上下线记录 tab.
	WebhookMACs []string `json:"webhook_macs,omitempty"`
	// BrandTitle / BrandSubtitle replace the hard-coded "WiFi 考勤" /
	// "工时统计" strings in the dashboard + login HTML and the page
	// <title>. Empty falls back to DefaultBrandTitle / DefaultBrandSubtitle.
	BrandTitle    string `json:"brand_title,omitempty"`
	BrandSubtitle string `json:"brand_subtitle,omitempty"`
}

// Default branding strings used when the user hasn't customised them.
const (
	DefaultBrandTitle    = "WiFi 考勤"
	DefaultBrandSubtitle = "工时统计"
)

// Store is a tiny JSON-file-backed settings store, mirroring
// the atomic-write pattern from AliasStore. Zero-dep by policy.
type Store struct {
	path string

	mu   sync.RWMutex
	data Settings
}

// New constructs a settings store backed by path. Pass an
// empty path for in-memory (testing). Missing or corrupt files are
// treated as empty defaults.
func New(path string) *Store {
	s := &Store{
		path: path,
		data: Settings{WorkStart: "09:00", WorkEnd: "18:30"},
	}
	s.load()
	return s
}

// Reload re-reads the settings file from disk. Used after backup
// import overwrites the JSON file.
func (s *Store) Reload() {
	s.mu.Lock()
	s.data = Settings{WorkStart: "09:00", WorkEnd: "18:30"}
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
		m = util.NormalizeMAC(m)
		if m == "" {
			continue
		}
		if _, dup := seen[m]; dup {
			continue
		}
		seen[m] = struct{}{}
		merged = append(merged, m)
	}
	if legacy := util.NormalizeMAC(d.MeMAC); legacy != "" {
		if _, dup := seen[legacy]; !dup {
			merged = append(merged, legacy)
			seen[legacy] = struct{}{}
		}
	}
	sort.Strings(merged)
	d.PunchMACs = merged
	d.MeMAC = "" // drop legacy
	// Same normalisation pass for WebhookMACs (dedupe, lowercase, drop empties).
	whSeen := make(map[string]struct{})
	whMerged := make([]string, 0, len(d.WebhookMACs))
	for _, m := range d.WebhookMACs {
		m = util.NormalizeMAC(m)
		if m == "" {
			continue
		}
		if _, dup := whSeen[m]; dup {
			continue
		}
		whSeen[m] = struct{}{}
		whMerged = append(whMerged, m)
	}
	sort.Strings(whMerged)
	d.WebhookMACs = whMerged
	s.mu.Lock()
	s.data = d
	s.mu.Unlock()
}

// Get returns a snapshot of current settings. The returned slice is
// safe to hold — it's copied.
func (s *Store) Get() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := s.data
	if len(s.data.PunchMACs) > 0 {
		out.PunchMACs = append([]string(nil), s.data.PunchMACs...)
	}
	if len(s.data.WebhookMACs) > 0 {
		out.WebhookMACs = append([]string(nil), s.data.WebhookMACs...)
	}
	return out
}

// IsPunch reports whether mac is currently tagged as a 打卡设备.
func (s *Store) IsPunch(mac string) bool {
	mac = util.NormalizeMAC(mac)
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
func (s *Store) AddPunch(mac string) error {
	mac = util.NormalizeMAC(mac)
	if mac == "" {
		return errors.New("settings: punch mac required")
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
func (s *Store) RemovePunch(mac string) error {
	mac = util.NormalizeMAC(mac)
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
// validate as HH:MM (or HH:MM:SS via util.ParseClock) and must satisfy
// start < end.
func (s *Store) Update(in Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.data
	if in.WorkStart != "" {
		if _, ok := util.ParseClock(in.WorkStart); !ok {
			return errors.New("settings: work_start must be HH:MM")
		}
		next.WorkStart = in.WorkStart
	}
	if in.WorkEnd != "" {
		if _, ok := util.ParseClock(in.WorkEnd); !ok {
			return errors.New("settings: work_end must be HH:MM")
		}
		next.WorkEnd = in.WorkEnd
	}
	sSec, _ := util.ParseClock(next.WorkStart)
	eSec, _ := util.ParseClock(next.WorkEnd)
	if eSec <= sSec {
		return errors.New("settings: work_end must be after work_start")
	}
	s.data = next
	return s.persistLocked()
}

// ClearPunchAll wipes the entire 打卡设备 set.
func (s *Store) ClearPunchAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.PunchMACs = nil
	return s.persistLocked()
}

// ClearMe is kept for callsite compat (old DELETE /api/settings?clear=me).
// Equivalent to ClearPunchAll.
func (s *Store) ClearMe() error { return s.ClearPunchAll() }

func (s *Store) persistLocked() error {
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
	if len(out.WebhookMACs) == 0 {
		out.WebhookMACs = nil
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
func (s *Store) SetGlobalWebhook(raw string) error {
	if raw != "" {
		if _, err := url.ParseRequestURI(raw); err != nil {
			return errors.New("settings: global_webhook_url must be a valid URL")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.GlobalWebhookURL = raw
	return s.persistLocked()
}

// SetWebhookKeyword sets or clears the keyword appended to every
// webhook body. Used to satisfy dingtalk/feishu keyword-filter
// security policies. Empty = no append. Length capped at 64 to keep
// the markdown footer compact.
func (s *Store) SetWebhookKeyword(raw string) error {
	raw = strings.TrimSpace(raw)
	if len(raw) > 64 {
		return errors.New("settings: webhook_keyword too long (max 64 chars)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.WebhookKeyword = raw
	return s.persistLocked()
}

// SetBranding updates the dashboard's brand title/subtitle. Either or
// both may be set in one call. Empty value resets the field, which
// causes [Brand] to fall back to the defaults. Length capped at 64
// per field so the page header stays presentable.
func (s *Store) SetBranding(title, subtitle string) error {
	title = strings.TrimSpace(title)
	subtitle = strings.TrimSpace(subtitle)
	if len(title) > 64 {
		return errors.New("settings: brand_title too long (max 64 chars)")
	}
	if len(subtitle) > 64 {
		return errors.New("settings: brand_subtitle too long (max 64 chars)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.BrandTitle = title
	s.data.BrandSubtitle = subtitle
	return s.persistLocked()
}

// Brand returns the effective branding pair, applying defaults when a
// field is empty. Always usable directly in templates.
func (s *Store) Brand() (title, subtitle string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	title = s.data.BrandTitle
	subtitle = s.data.BrandSubtitle
	if title == "" {
		title = DefaultBrandTitle
	}
	if subtitle == "" {
		subtitle = DefaultBrandSubtitle
	}
	return
}

// PunchMACsUpper returns the 打卡设备 MACs as uppercase strings for
// wire formats that prefer display case (e.g. /api/settings GET).
func (s *Store) PunchMACsUpper() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.data.PunchMACs))
	for i, m := range s.data.PunchMACs {
		out[i] = strings.ToUpper(m)
	}
	return out
}

// IsWebhook reports whether mac is opted into the global webhook.
func (s *Store) IsWebhook(mac string) bool {
	mac = util.NormalizeMAC(mac)
	if mac == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.data.WebhookMACs {
		if m == mac {
			return true
		}
	}
	return false
}

// AddWebhook adds mac to the global-webhook opt-in set. No-op if already present.
func (s *Store) AddWebhook(mac string) error {
	mac = util.NormalizeMAC(mac)
	if mac == "" {
		return errors.New("settings: webhook mac required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.data.WebhookMACs {
		if m == mac {
			return nil
		}
	}
	s.data.WebhookMACs = append(s.data.WebhookMACs, mac)
	sort.Strings(s.data.WebhookMACs)
	return s.persistLocked()
}

// RemoveWebhook removes mac from the global-webhook opt-in set. No-op if absent.
func (s *Store) RemoveWebhook(mac string) error {
	mac = util.NormalizeMAC(mac)
	if mac == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.data.WebhookMACs[:0]
	for _, m := range s.data.WebhookMACs {
		if m != mac {
			out = append(out, m)
		}
	}
	s.data.WebhookMACs = append([]string{}, out...)
	return s.persistLocked()
}

// WebhookMACsUpper returns the global-webhook opt-in MACs as uppercase
// strings for wire formats that prefer display case.
func (s *Store) WebhookMACsUpper() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.data.WebhookMACs))
	for i, m := range s.data.WebhookMACs {
		out[i] = strings.ToUpper(m)
	}
	return out
}
