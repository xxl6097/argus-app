package web

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
// or "workday". Entries with "holiday" mark legal holidays (OT if
// worked); "workday" entries flip a weekend/holiday into a normal
// workday (调休).
//
// Days not in the store default to:
//   - Saturday/Sunday => Weekend (OT)
//   - Monday-Friday   => Workday
type HolidayStore struct {
	path string

	mu   sync.RWMutex
	data map[string]string // "YYYY-MM-DD" -> "holiday" | "workday"
}

// NewHolidayStore constructs a store backed by path. Empty path =
// in-memory only. Missing / corrupt file = empty defaults.
func NewHolidayStore(path string) *HolidayStore {
	s := &HolidayStore{path: path, data: make(map[string]string)}
	s.load()
	return s
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

// Kind returns the DayKind for t in t's location. Missing entries
// fall back to weekday-vs-weekend detection.
func (s *HolidayStore) Kind(t time.Time) DayKind {
	key := t.Format("2006-01-02")
	s.mu.RLock()
	v := s.data[key]
	s.mu.RUnlock()
	switch v {
	case "holiday":
		return DayKindLegalHoliday
	case "workday":
		// Explicit 调休 workday: even on a Saturday, this is a normal
		// workday.
		return DayKindMakeupWorkday
	case "otday":
		// User-marked overtime day: even on a Monday, treat the
		// whole day as overtime (project crunch, weekend crunch
		// that spilled past Sunday, etc.).
		return DayKindOTDay
	}
	wd := t.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return DayKindWeekend
	}
	return DayKindWorkday
}

// All returns a snapshot of the raw map.
func (s *HolidayStore) All() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
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
