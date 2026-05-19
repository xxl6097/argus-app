package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DayKind classifies a calendar date for worktime purposes.
//
// Four accounting branches:
//   - Workday / MakeupWorkday → standard workday rules apply
//     (overtime = early + late vs. standard hours, supports 迟到/早退).
//   - Weekend → treated as an overtime day (every minute on duty = OT).
//   - OTDay → user explicitly flagged this weekday as overtime
//     (e.g. 项目紧急加班的周三). Same semantics as Weekend.
//   - LegalHoliday → NOT overtime; no attendance expected. Present
//     time is recorded but overtime stays zero.
type DayKind int

const (
	DayKindWorkday DayKind = iota + 1
	DayKindLegalHoliday
	DayKindWeekend
	DayKindMakeupWorkday
	DayKindOTDay
)

// IsOvertimeDay returns true if every minute on duty that day counts
// as overtime. True for Weekend and user-marked OTDay.
func (k DayKind) IsOvertimeDay() bool {
	return k == DayKindWeekend || k == DayKindOTDay
}

// String returns a short lowercase tag, used as the JSON wire value
// so the front-end can switch on it without remembering int codes.
func (k DayKind) String() string {
	switch k {
	case DayKindWorkday:
		return "workday"
	case DayKindLegalHoliday:
		return "holiday"
	case DayKindWeekend:
		return "weekend"
	case DayKindMakeupWorkday:
		return "makeup"
	case DayKindOTDay:
		return "otday"
	}
	return ""
}

// HolidayStore is a JSON-file-backed map of YYYY-MM-DD -> "holiday"
// or "workday". It now has two layers:
//
//  1. Manual (data): user-set via UI, persisted to path, never auto-cleared.
//  2. System (systemData): fetched from timor.tech API, persisted to
//     systemPath, refreshed daily at 03:00.
//
// Query priority: manual > system > weekday fallback.
//
// Days not in either store default to:
//   - Saturday/Sunday => Weekend (OT)
//   - Monday-Friday   => Workday
type HolidayStore struct {
	path       string
	systemPath string

	mu         sync.RWMutex
	data       map[string]string // manual: UI-set entries
	systemData map[string]string // auto-fetched from API

	// Refresh control: ticker fires daily at 03:00, calls fetchSystem.
	stopRefresh chan struct{}
	wg          sync.WaitGroup
}

// NewHolidayStore constructs a store backed by path (manual entries
// only). Use NewHolidayStoreWithSystem to enable auto-fetch too.
// Empty path = in-memory only. Missing / corrupt file = empty defaults.
func NewHolidayStore(path string) *HolidayStore {
	return NewHolidayStoreWithSystem(path, "")
}

// NewHolidayStoreWithSystem adds an auto-fetched system layer on top
// of the manual layer. systemPath is where the API response cache is
// persisted — on empty string the system layer stays in-memory.
// Call StartAutoRefresh to kick off daily updates.
func NewHolidayStoreWithSystem(path, systemPath string) *HolidayStore {
	s := &HolidayStore{
		path:       path,
		systemPath: systemPath,
		data:       make(map[string]string),
		systemData: make(map[string]string),
	}
	s.load()
	s.loadSystem()
	return s
}

// Reload re-reads holidays.json + holidays_system.json from disk.
// Used after backup import overwrites the JSON files.
func (s *HolidayStore) Reload() {
	s.mu.Lock()
	s.data = make(map[string]string)
	s.systemData = make(map[string]string)
	s.mu.Unlock()
	s.load()
	s.loadSystem()
}

// SeedDefaultsIfEmpty seeds the given defaults when the store is
// currently empty (first-run convenience). Best-effort; errors are
// silently dropped — the user can always edit the file by hand
// afterwards.
func (s *HolidayStore) SeedDefaultsIfEmpty(defaults map[string]string) {
	s.mu.Lock()
	empty := len(s.data) == 0
	if empty {
		for k, v := range defaults {
			s.data[k] = v
		}
	}
	needSave := empty && s.path != ""
	var err error
	if needSave {
		err = s.persistLocked()
	}
	s.mu.Unlock()
	_ = err
}

// CN2026Holidays returns the 2026 State Council holiday / 调休
// calendar as of November 2025. The wire keys are "YYYY-MM-DD"; values
// are "holiday" (paid day off) or "workday" (调休 makeup workday —
// normally a Saturday/Sunday "flipped" into a workday to bridge a
// holiday).
//
// Source: 国务院办公厅关于2026年部分节假日安排的通知. Update this table
// every year when the new notice comes out.
func CN2026Holidays() map[string]string {
	return map[string]string{
		// 元旦: 1/1(四) - 1/3(六)
		"2026-01-01": "holiday",
		"2026-01-02": "holiday",
		"2026-01-03": "holiday",
		// 春节: 2/17(二) - 2/23(一); 调休: 2/15(日), 2/28(六) 上班
		"2026-02-15": "workday",
		"2026-02-17": "holiday",
		"2026-02-18": "holiday",
		"2026-02-19": "holiday",
		"2026-02-20": "holiday",
		"2026-02-21": "holiday",
		"2026-02-22": "holiday",
		"2026-02-23": "holiday",
		"2026-02-28": "workday",
		// 清明: 4/4(六) - 4/6(一)
		"2026-04-04": "holiday",
		"2026-04-05": "holiday",
		"2026-04-06": "holiday",
		// 劳动节: 5/1(五) - 5/5(二); 调休: 4/26(日), 5/9(六) 上班
		"2026-04-26": "workday",
		"2026-05-01": "holiday",
		"2026-05-02": "holiday",
		"2026-05-03": "holiday",
		"2026-05-04": "holiday",
		"2026-05-05": "holiday",
		"2026-05-09": "workday",
		// 端午: 6/19(五) - 6/21(日)
		"2026-06-19": "holiday",
		"2026-06-20": "holiday",
		"2026-06-21": "holiday",
		// 中秋 + 国庆拼假: 9/25(五) - 10/6(二); 调休: 9/26(六), 10/10(六) 上班
		"2026-09-25": "holiday",
		"2026-09-26": "workday",
		"2026-09-27": "holiday", // 中秋
		"2026-10-01": "holiday",
		"2026-10-02": "holiday",
		"2026-10-03": "holiday",
		"2026-10-04": "holiday",
		"2026-10-05": "holiday",
		"2026-10-06": "holiday",
		"2026-10-10": "workday",
	}
}

func (s *HolidayStore) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		v = strings.TrimSpace(strings.ToLower(v))
		if v != "holiday" && v != "workday" && v != "otday" {
			continue
		}
		out[strings.TrimSpace(k)] = v
	}
	s.mu.Lock()
	s.data = out
	s.mu.Unlock()
}

// Kind returns the DayKind for t in t's location. Looks up the
// manual layer first (so user intent always wins), falls back to the
// auto-fetched system layer, and finally to weekday/weekend detection.
func (s *HolidayStore) Kind(t time.Time) DayKind {
	key := t.Format("2006-01-02")
	s.mu.RLock()
	v, ok := s.data[key]
	if !ok {
		v, ok = s.systemData[key]
	}
	s.mu.RUnlock()
	if ok {
		switch v {
		case "holiday":
			return DayKindLegalHoliday
		case "workday":
			// Explicit 调休 workday: even on a Saturday, this is a
			// normal workday.
			return DayKindMakeupWorkday
		case "otday":
			// User-marked overtime day: even on a Monday, treat the
			// whole day as overtime (project crunch, weekend crunch
			// that spilled past Sunday, etc.).
			return DayKindOTDay
		}
	}
	wd := t.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return DayKindWeekend
	}
	return DayKindWorkday
}

// All returns a snapshot of both layers merged (manual wins on
// conflict). Primarily used by /api/holidays GET — the wire format
// stays flat for backward compat.
func (s *HolidayStore) All() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.data)+len(s.systemData))
	for k, v := range s.systemData {
		out[k] = v
	}
	for k, v := range s.data {
		out[k] = v // manual wins
	}
	return out
}

// AllWithSource returns both layers separately so callers (e.g. the
// dashboard) can distinguish user-set entries from auto-fetched ones.
func (s *HolidayStore) AllWithSource() (manual, system map[string]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	manual = make(map[string]string, len(s.data))
	for k, v := range s.data {
		manual[k] = v
	}
	system = make(map[string]string, len(s.systemData))
	for k, v := range s.systemData {
		system[k] = v
	}
	return
}

// Set writes or clears a date entry. kind must be "holiday", "workday",
// or "" (remove). date must be YYYY-MM-DD.
func (s *HolidayStore) Set(date, kind string) error {
	date = strings.TrimSpace(date)
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return errors.New("web: holiday date must be YYYY-MM-DD")
	}
	kind = strings.TrimSpace(strings.ToLower(kind))
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == "" {
		delete(s.data, date)
	} else if kind == "holiday" || kind == "workday" || kind == "otday" {
		s.data[date] = kind
	} else {
		return errors.New("web: kind must be 'holiday', 'workday', 'otday', or empty")
	}
	return s.persistLocked()
}

func (s *HolidayStore) persistLocked() error {
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

// ---------- System (auto-fetched) layer ----------

// loadSystem reads systemPath if present; missing / corrupt file is
// silently treated as empty. The next FetchSystem() call will repopulate.
func (s *HolidayStore) loadSystem() {
	if s.systemPath == "" {
		return
	}
	b, err := os.ReadFile(s.systemPath)
	if err != nil {
		return
	}
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		v = strings.TrimSpace(strings.ToLower(v))
		if v != "holiday" && v != "workday" {
			continue
		}
		out[strings.TrimSpace(k)] = v
	}
	s.mu.Lock()
	s.systemData = out
	s.mu.Unlock()
}

func (s *HolidayStore) persistSystemLocked() error {
	if s.systemPath == "" {
		return nil
	}
	// Sorted keys → stable diffs in git / when watching the file.
	keys := make([]string, 0, len(s.systemData))
	for k := range s.systemData {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make(map[string]string, len(keys))
	for _, k := range keys {
		ordered[k] = s.systemData[k]
	}
	b, err := json.MarshalIndent(ordered, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.systemPath); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := s.systemPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.systemPath)
}

// timorYearResp is the shape of timor.tech/api/holiday/year/YYYY.
// Each holiday key is a YYYY-MM-DD string; .holiday true = paid day off,
// false = 调休 makeup workday. We discard the human-readable name field.
type timorYearResp struct {
	Code    int                          `json:"code"`
	Holiday map[string]timorHolidayEntry `json:"holiday"`
}

type timorHolidayEntry struct {
	Holiday bool   `json:"holiday"` // true = paid day off; false = 调休 workday
	Name    string `json:"name"`
	Date    string `json:"date"` // "MM-DD" — not always present, prefer map key
}

// fetchYear queries timor.tech for one calendar year. Returns a flat
// map["YYYY-MM-DD"] = "holiday"|"workday" on success. Network errors,
// non-200 responses, and code != 0 all return an error so the caller
// can decide whether to keep the previous cache.
func fetchYear(ctx context.Context, client *http.Client, year int) (map[string]string, error) {
	url := fmt.Sprintf("https://timor.tech/api/holiday/year/%d", year)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "argus-app/1.0 (+holidays)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("holidays: %s returned %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return nil, err
	}
	var parsed timorYearResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("holidays: parse %d: %w", year, err)
	}
	if parsed.Code != 0 {
		return nil, fmt.Errorf("holidays: api code=%d for year %d", parsed.Code, year)
	}
	out := make(map[string]string, len(parsed.Holiday))
	prefix := fmt.Sprintf("%d-", year)
	for key, entry := range parsed.Holiday {
		// timor's keys are sometimes "MM-DD" and sometimes "YYYY-MM-DD";
		// normalize to the latter.
		date := strings.TrimSpace(key)
		if len(date) == 5 { // "MM-DD"
			date = prefix + date
		}
		if len(date) != 10 {
			continue
		}
		if entry.Holiday {
			out[date] = "holiday"
		} else {
			out[date] = "workday"
		}
	}
	return out, nil
}

// FetchSystem refreshes the system layer by querying every year from
// the current year to currentYear+yearsAhead-1. Years that succeed
// are merged in; years that fail are skipped (we keep whatever was in
// the cache for that year). Persists on success.
//
// Returns the number of years that fetched successfully and the
// last error (if any).
func (s *HolidayStore) FetchSystem(ctx context.Context, yearsAhead int) (int, error) {
	if yearsAhead <= 0 {
		yearsAhead = 1
	}
	client := &http.Client{Timeout: 12 * time.Second}
	startYear := time.Now().Year()
	merged := make(map[string]string)
	// Seed with everything currently cached so partial failures don't
	// regress already-known years.
	s.mu.RLock()
	for k, v := range s.systemData {
		merged[k] = v
	}
	s.mu.RUnlock()

	successes := 0
	var lastErr error
	for i := 0; i < yearsAhead; i++ {
		y := startYear + i
		// Per-year context with its own deadline so one slow year
		// doesn't sink the rest.
		yearCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		got, err := fetchYear(yearCtx, client, y)
		cancel()
		if err != nil {
			lastErr = err
			continue
		}
		// Drop existing entries for this year first, then overlay the
		// new ones — that way a holiday that got cancelled (rare but
		// possible) actually disappears.
		yearPrefix := fmt.Sprintf("%d-", y)
		for k := range merged {
			if strings.HasPrefix(k, yearPrefix) {
				delete(merged, k)
			}
		}
		for k, v := range got {
			merged[k] = v
		}
		successes++
	}
	if successes == 0 {
		return 0, lastErr
	}
	s.mu.Lock()
	s.systemData = merged
	err := s.persistSystemLocked()
	s.mu.Unlock()
	if err != nil {
		return successes, err
	}
	return successes, lastErr
}

// StartAutoRefresh kicks off a background goroutine that:
//   - immediately attempts one fetch (so first boot has data without
//     waiting for 03:00 the next day);
//   - then fires daily at 03:00 local time to refresh.
//
// yearsAhead controls how many calendar years (current + future) to
// pull on every refresh. Reasonable values: 1-10. The State Council
// typically only publishes the current year + sometimes Q1 of next,
// so any value > 2 will silently no-op for the unpublished years.
//
// Safe to call multiple times; subsequent calls are no-ops once a
// goroutine is already running. Stop with StopAutoRefresh on shutdown.
func (s *HolidayStore) StartAutoRefresh(yearsAhead int) {
	s.mu.Lock()
	if s.stopRefresh != nil {
		s.mu.Unlock()
		return
	}
	s.stopRefresh = make(chan struct{})
	stop := s.stopRefresh
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		// Immediate attempt — non-fatal on failure.
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		_, _ = s.FetchSystem(ctx, yearsAhead)
		cancel()

		for {
			next := nextRefreshAt(time.Now(), 3, 0)
			wait := time.Until(next)
			select {
			case <-stop:
				return
			case <-time.After(wait):
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				_, _ = s.FetchSystem(ctx, yearsAhead)
				cancel()
			}
		}
	}()
}

// StopAutoRefresh signals the refresh goroutine to exit and waits
// for it. Idempotent.
func (s *HolidayStore) StopAutoRefresh() {
	s.mu.Lock()
	stop := s.stopRefresh
	s.stopRefresh = nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	s.wg.Wait()
}

// nextRefreshAt returns the next local-time instant at HH:MM after
// `now`. If now is already past today's HH:MM, it returns tomorrow's.
func nextRefreshAt(now time.Time, hour, minute int) time.Time {
	loc := now.Location()
	candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, loc)
	if !candidate.After(now) {
		candidate = candidate.AddDate(0, 0, 1)
	}
	return candidate
}
