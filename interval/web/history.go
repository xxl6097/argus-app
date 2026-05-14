package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	argus "github.com/xxl6097/argusd"
)

// HistoryRetention is how long per-MAC online/offline records are kept
// on disk and returned by /api/history. Records older than this are
// dropped on the next compaction pass (append-time, cheap).
const HistoryRetention = 30 * 24 * time.Hour

// historyCompactEvery triggers an in-place rewrite of a MAC's jsonl
// file once it grows past this many lines, to bound disk usage even
// for chatty devices that flap frequently.
const historyCompactEvery = 2000

// HistoryEntry is a single online/offline transition persisted per MAC.
//
// Wire shape is kept minimal on purpose — the history file grows on
// every flap, and we only ever need it to reconstruct presence
// intervals. Device-identifying fields (hostname, ip) are recorded
// for diagnostic display but are otherwise not load-bearing.
type HistoryEntry struct {
	TimeMs   int64  `json:"t"` // unix millis
	Kind     string `json:"k"` // "ONLINE" | "OFFLINE"
	IP       string `json:"ip,omitempty"`
	Hostname string `json:"host,omitempty"`
}

// HistoryStore is an append-only per-MAC event log backed by JSONL
// files under dir/history/<mac>.jsonl.
//
// Only ONLINE / OFFLINE events are recorded (CHANGE is noise for
// presence math). Concurrency: one mutex per MAC via a sharded map;
// read/write to disparate MACs do not contend.
type HistoryStore struct {
	dir string

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-MAC write serialization
}

// NewHistoryStore constructs a history store rooted at dir. Passing
// an empty dir disables persistence entirely (Record becomes a no-op
// and Query returns an empty slice).
//
// The constructor is best-effort: it creates dir if it doesn't
// exist, but doesn't fail on permission errors — subsequent writes
// will surface the problem.
func NewHistoryStore(dir string) *HistoryStore {
	s := &HistoryStore{dir: dir, locks: make(map[string]*sync.Mutex)}
	if dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	return s
}

// macLock returns the write lock for a MAC, creating it on first use.
func (s *HistoryStore) macLock(mac string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.locks[mac]; ok {
		return m
	}
	m := &sync.Mutex{}
	s.locks[mac] = m
	return m
}

// macFile returns the on-disk path for a MAC. Filename uses `-` in
// place of `:` for portability even though Linux handles `:` fine.
func (s *HistoryStore) macFile(mac string) string {
	safe := strings.ReplaceAll(normalizeMAC(mac), ":", "-")
	return filepath.Join(s.dir, safe+".jsonl")
}

// Record appends an Online/Offline transition for the given event.
// Change events are ignored. No-op when the store is disk-less.
func (s *HistoryStore) Record(e argus.Event) {
	if s.dir == "" {
		return
	}
	var kind string
	switch e.Kind {
	case argus.EventOnline:
		kind = "ONLINE"
	case argus.EventOffline:
		kind = "OFFLINE"
	default:
		return
	}
	mac := normalizeMAC(e.Device.MAC)
	if mac == "" {
		return
	}
	ts := nonZeroTime(e.Time)
	entry := HistoryEntry{
		TimeMs:   ts.UnixMilli(),
		Kind:     kind,
		IP:       e.Device.IP,
		Hostname: e.Device.Hostname,
	}

	lk := s.macLock(mac)
	lk.Lock()
	defer lk.Unlock()

	path := s.macFile(mac)
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line = append(line, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(line)
	_ = f.Close()

	// Opportunistic compaction: cheap stat, only rewrite when large.
	if st, err := os.Stat(path); err == nil && st.Size() > int64(historyCompactEvery*120) {
		s.compactLocked(mac, path)
	}
}

// Query returns entries for a MAC between [from, to], oldest first.
// A zero from means "earliest retained"; a zero to means "now".
// Returns an empty slice when the MAC has no history.
func (s *HistoryStore) Query(mac string, from, to time.Time) ([]HistoryEntry, error) {
	if s.dir == "" {
		return nil, nil
	}
	mac = normalizeMAC(mac)
	if mac == "" {
		return nil, errors.New("web: history query requires mac")
	}
	path := s.macFile(mac)
	lk := s.macLock(mac)
	lk.Lock()
	defer lk.Unlock()
	return readEntries(path, from, to)
}

// SeedBaseline appends an ONLINE entry for the given device only if
// the file's last entry isn't already ONLINE. Used on startup to
// anchor the currently-online set without double-counting an
// uninterrupted session across restarts.
//
// Also opportunistically prunes entries older than HistoryRetention,
// so retention changes between versions take effect on next boot
// instead of having to wait for the chatty-device compaction trigger.
func (s *HistoryStore) SeedBaseline(dev argus.Device, at time.Time) {
	if s.dir == "" {
		return
	}
	mac := normalizeMAC(dev.MAC)
	if mac == "" {
		return
	}
	lk := s.macLock(mac)
	lk.Lock()
	defer lk.Unlock()
	path := s.macFile(mac)
	// Prune first, then decide whether to seed. If pruning leaves the
	// file empty-or-OFFLINE-terminated we'll append a fresh ONLINE below.
	s.compactLocked(mac, path)
	entries, err := readEntries(path, time.Time{}, time.Time{})
	if err == nil && len(entries) > 0 && entries[len(entries)-1].Kind == "ONLINE" {
		return // already online in the log, don't duplicate
	}
	entry := HistoryEntry{
		TimeMs:   nonZeroTime(at).UnixMilli(),
		Kind:     "ONLINE",
		IP:       dev.IP,
		Hostname: dev.Hostname,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	line = append(line, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(line)
	_ = f.Close()
}

// compactLocked rewrites a MAC's jsonl dropping entries older than
// HistoryRetention. Caller must hold the per-MAC lock.
func (s *HistoryStore) compactLocked(mac, path string) {
	cutoff := time.Now().Add(-HistoryRetention)
	entries, err := readEntries(path, cutoff, time.Time{})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	enc := json.NewEncoder(f)
	for _, e := range entries {
		_ = enc.Encode(e)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return
	}
	_ = os.Rename(tmp, path)
}

// readEntries returns entries between [from, to] from path, oldest
// first. Silently skips malformed lines.
func readEntries(path string, from, to time.Time) ([]HistoryEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var fromMs, toMs int64
	if !from.IsZero() {
		fromMs = from.UnixMilli()
	}
	if to.IsZero() {
		toMs = time.Now().UnixMilli()
	} else {
		toMs = to.UnixMilli()
	}
	out := make([]HistoryEntry, 0, 64)
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e HistoryEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		if e.TimeMs < fromMs || e.TimeMs > toMs {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TimeMs < out[j].TimeMs })
	return out, nil
}

// WorktimeReport is the computed shape returned by /api/worktime.
//
// Semantics (per user spec):
//   - PresentSecs = LastSeenMs − min(FirstSeenMs, StandardStart).
//     i.e. on-duty is measured from the earlier of "first online" and
//     "standard start time". Coming late does NOT shrink on-duty time;
//     coming early DOES extend it.
//   - EarlyOTSecs = max(0, StandardStart − FirstSeenMs).
//     Coming in before the workday start counts as overtime; being
//     late does NOT produce negative overtime.
//   - LateOTSecs  = max(0, LastSeenMs − StandardEnd).
//     Leaving after workday end counts as overtime; leaving early
//     does NOT produce negative overtime.
//   - OvertimeSecs = EarlyOTSecs + LateOTSecs.
//
// StandardSecs is the configured workday length (end − start in minutes).
type WorktimeReport struct {
	MAC             string     `json:"mac"`
	Date            string     `json:"date"`  // YYYY-MM-DD
	StartHHMM       string     `json:"start"` // e.g. "09:00"
	EndHHMM         string     `json:"end"`   // e.g. "18:30"
	StandardSecs    int64      `json:"standard_secs"`
	PresentSecs     int64      `json:"present_secs"`
	EarlyOTSecs     int64      `json:"early_ot_secs"`
	LateOTSecs      int64      `json:"late_ot_secs"`
	OvertimeSecs    int64      `json:"overtime_secs"`
	FirstSeenMs     int64      `json:"first_seen_ms,omitempty"`
	LastSeenMs      int64      `json:"last_seen_ms,omitempty"`
	Intervals       []Interval `json:"intervals"`
	OpenAtEnd       bool       `json:"open_at_end"`                // true if still online at query time
	Manual          bool       `json:"manual,omitempty"`           // true if an override was applied
	MissingOut      bool       `json:"missing_out,omitempty"`      // firstIn known, but no lastOut/now anchor to close the day
	ArrivalStatus   string     `json:"arrival_status,omitempty"`   // "", "late", "missed_in"
	DepartureStatus string     `json:"departure_status,omitempty"` // "", "early_leave"
	DayKind         string     `json:"day_kind,omitempty"`         // "workday" | "weekend" | "holiday" | "makeup" | "otday"
	OTDay           bool       `json:"ot_day,omitempty"`           // true on legal holidays and normal weekends
	Sessions        int        `json:"sessions"`
}

// Interval is a single online->offline segment, clipped to the day.
type Interval struct {
	StartMs int64 `json:"start_ms"`
	EndMs   int64 `json:"end_ms"`
	Secs    int64 `json:"secs"`
}

// ComputeWorktime derives a worktime report for mac on the given date
// using entries as the authoritative online/offline log, optionally
// overridden by a manual Override for (mac, date).
//
// dayKind selects one of three accounting branches:
//
//   - Weekend (Saturday/Sunday, unless marked as 调休 workday):
//     OvertimeSecs = PresentSecs = LastOut − FirstIn. Every minute
//     on duty is overtime; arrival/departure flags don't apply.
//
//   - LegalHoliday (国务院宣布的节假日, or user-marked holiday):
//     PresentSecs = LastOut − FirstIn, but OvertimeSecs = 0.
//     Holidays are paid days off — showing up still gets tracked
//     under 在岗 time, but we do not claim it as overtime unless the
//     user converts the day to a 调休-workday.
//
//   - Workday / MakeupWorkday: standard workday rules.
//     PresentSecs = LastOut − min(FirstIn, StandardStart).
//     OvertimeSecs = EarlyOT + LateOT.
//     Supports 迟到 / 早退 / 漏刷卡 arrival/departure flags.
//
// Override semantics: Override.In replaces the "first online" anchor
// with `date 00:00 + In`. Override.Out replaces the "last offline"
// anchor with `date 00:00 + Out`. Setting one but not the other
// corrects a single boundary while the other comes from history.
//
// For "was already online at 00:00" the caller should pass entries
// covering a window that extends back to the last transition before
// 00:00 (see Server.handleWorktime).
func ComputeWorktime(mac string, date time.Time, startHHMM, endHHMM string, entries []HistoryEntry, now time.Time, override Override, dayKind DayKind) WorktimeReport {
	loc := date.Location()
	dayStart := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, loc)
	dayEnd := dayStart.Add(24 * time.Hour)

	rep := WorktimeReport{
		MAC:       strings.ToUpper(mac),
		Date:      dayStart.Format("2006-01-02"),
		StartHHMM: startHHMM,
		EndHHMM:   endHHMM,
		Intervals: []Interval{},
	}
	if dayKind == 0 {
		dayKind = DayKindWorkday
	}
	rep.DayKind = dayKind.String()
	rep.OTDay = dayKind.IsOvertimeDay()
	rep.StandardSecs = standardSecs(startHHMM, endHHMM)

	// Resolve HH:MM to absolute times within the day.
	stdStart := hhmmOnDay(dayStart, startHHMM, 9*60)
	stdEnd := hhmmOnDay(dayStart, endHHMM, 18*60+30)

	// Reconstruct presence for the UI timeline and find first/last boundaries.
	var segStart *time.Time
	var priorKind string
	for _, e := range entries {
		et := time.UnixMilli(e.TimeMs).In(loc)
		if et.Before(dayStart) {
			priorKind = e.Kind
			continue
		}
		break
	}
	if priorKind == "ONLINE" {
		t := dayStart
		segStart = &t
	}

	clipEnd := dayEnd
	stillOnline := false
	if now.Before(dayEnd) {
		clipEnd = now
	}

	var firstIn, lastOut *time.Time
	if priorKind == "ONLINE" {
		t := dayStart
		firstIn = &t
	}

	for _, e := range entries {
		et := time.UnixMilli(e.TimeMs).In(loc)
		if et.Before(dayStart) || et.After(dayEnd) {
			continue
		}
		switch e.Kind {
		case "ONLINE":
			if firstIn == nil {
				t := et
				firstIn = &t
			}
			if segStart == nil {
				t := et
				segStart = &t
			}
		case "OFFLINE":
			if segStart != nil {
				addInterval(&rep, *segStart, et)
				segStart = nil
			}
			t := et
			lastOut = &t
		}
	}
	if segStart != nil {
		addInterval(&rep, *segStart, clipEnd)
		stillOnline = true
	}

	// Apply manual overrides on top of the derived boundaries.
	if override.In != "" {
		t := hhmmOnDay(dayStart, override.In, 0)
		firstIn = &t
		rep.Manual = true
	}
	if override.Out != "" {
		t := hhmmOnDay(dayStart, override.Out, 0)
		lastOut = &t
		stillOnline = false
		rep.Manual = true
	}

	rep.Sessions = len(rep.Intervals)
	rep.OpenAtEnd = stillOnline

	end := lastOut
	if stillOnline {
		end = &clipEnd
	}
	// Don't expose synthetic day-boundary times — those happen when
	// the device was already online at 00:00 (firstIn == dayStart)
	// or is still online and we're querying after end-of-day
	// (end == dayEnd). Rendering them naively shows "00:00:00", which
	// looks like a real check-in/out but isn't. UI treats empty as "—".
	if firstIn != nil && !firstIn.Equal(dayStart) {
		rep.FirstSeenMs = firstIn.UnixMilli()
	}
	if end != nil && !end.Equal(dayEnd) {
		rep.LastSeenMs = end.UnixMilli()
	}

	// Partial-day guard: firstIn is known but we have no lastOut and
	// the device isn't currently online (so no now-anchor either).
	// This happens when:
	//   - the user wrote an override with only `in:` set, and
	//   - the watcher never recorded an OFFLINE for that day
	//     (e.g. argusd was off, or the log was trimmed).
	// Don't guess — leave all OT/present/status at zero and let the
	// UI prompt the user to file a manual out-time. Partial numbers
	// would be misleading (we had a bug where early_ot was counted
	// against a nonexistent lastOut).
	if firstIn != nil && end == nil {
		rep.MissingOut = true
		return rep
	}

	if rep.OTDay {
		// Weekend semantics: every minute on duty is overtime.
		// PresentSecs == OvertimeSecs == LastOut − FirstIn. No early/
		// late split (early_ot / late_ot stay zero). No arrival /
		// departure flags either — there's no standard workday to
		// compare against.
		if firstIn != nil && end != nil && end.After(*firstIn) {
			secs := int64(end.Sub(*firstIn).Seconds())
			rep.PresentSecs = secs
			rep.OvertimeSecs = secs
		}
		return rep
	}

	if dayKind == DayKindLegalHoliday {
		// Paid day off: track 在岗 if they did show up, but do not
		// claim overtime. The user can mark the day as "workday"
		// manually (e.g. 调休) to get overtime accounting instead.
		if firstIn != nil && end != nil && end.After(*firstIn) {
			rep.PresentSecs = int64(end.Sub(*firstIn).Seconds())
		}
		return rep
	}

	// Regular workday path (unchanged):
	// PresentSecs = LastOut − min(FirstIn, StandardStart).
	if firstIn != nil && end != nil {
		anchor := *firstIn
		if stdStart.Before(anchor) {
			anchor = stdStart
		}
		if end.After(anchor) {
			rep.PresentSecs = int64(end.Sub(anchor).Seconds())
		}
	}

	// Overtime: early-in + late-out. Neither side can go negative.
	if firstIn != nil && firstIn.Before(stdStart) {
		rep.EarlyOTSecs = int64(stdStart.Sub(*firstIn).Seconds())
	}
	if end != nil && end.After(stdEnd) {
		rep.LateOTSecs = int64(end.Sub(stdEnd).Seconds())
	}
	rep.OvertimeSecs = rep.EarlyOTSecs + rep.LateOTSecs

	// Arrival / departure status flags — workday only.
	if firstIn != nil {
		switch {
		case firstIn.After(stdEnd):
			rep.ArrivalStatus = "missed_in"
		case firstIn.After(stdStart):
			rep.ArrivalStatus = "late"
		}
	}
	if !rep.OpenAtEnd && lastOut != nil && lastOut.Before(stdEnd) && firstIn != nil {
		rep.DepartureStatus = "early_leave"
	}
	return rep
}

// hhmmOnDay resolves an HH:MM or HH:MM:SS string to an absolute
// time on day. Falls back to `defaultMin` minutes past midnight on
// parse failure.
func hhmmOnDay(day time.Time, hhmm string, defaultMin int) time.Time {
	if secs, ok := parseClock(hhmm); ok {
		return day.Add(time.Duration(secs) * time.Second)
	}
	return day.Add(time.Duration(defaultMin) * time.Minute)
}

// MonthlyReport aggregates per-day worktime over a calendar month.
// Zero-present days are omitted from Days to keep the payload tight
// (the UI only cares about days the device actually showed up).
type MonthlyReport struct {
	MAC             string        `json:"mac"`
	Month           string        `json:"month"` // "YYYY-MM"
	StartHHMM       string        `json:"start"`
	EndHHMM         string        `json:"end"`
	PresentSecs     int64         `json:"present_secs"`
	OvertimeSecs    int64         `json:"overtime_secs"`
	EarlyOTSecs     int64         `json:"early_ot_secs"`
	LateOTSecs      int64         `json:"late_ot_secs"`
	WorkedDays      int           `json:"worked_days"`
	OTDays          int           `json:"ot_days"` // # of weekend/holiday OT days with attendance
	LateDays        int           `json:"late_days"`
	MissedInDays    int           `json:"missed_in_days"`
	EarlyLeaveDays  int           `json:"early_leave_days"`
	AvgDailyOTSecs  int64         `json:"avg_daily_ot_secs"`  // OvertimeSecs / WorkedDays
	AvgWeeklyOTSecs int64         `json:"avg_weekly_ot_secs"` // OvertimeSecs * 7 / WorkedDays (prorated)
	Days            []DayWorktime `json:"days"`
}

// DayWorktime is a single-day row in MonthlyReport.Days.
type DayWorktime struct {
	Date            string `json:"date"`
	PresentSecs     int64  `json:"present_secs"`
	OvertimeSecs    int64  `json:"overtime_secs"`
	EarlyOTSecs     int64  `json:"early_ot_secs"`
	LateOTSecs      int64  `json:"late_ot_secs"`
	FirstSeenMs     int64  `json:"first_seen_ms,omitempty"`
	LastSeenMs      int64  `json:"last_seen_ms,omitempty"`
	ArrivalStatus   string `json:"arrival_status,omitempty"`
	DepartureStatus string `json:"departure_status,omitempty"`
	DayKind         string `json:"day_kind,omitempty"`
	OTDay           bool   `json:"ot_day,omitempty"`
	Manual          bool   `json:"manual,omitempty"`
	MissingOut      bool   `json:"missing_out,omitempty"`
	OpenAtEnd       bool   `json:"open_at_end,omitempty"`
}

// addInterval clips [s, e] to positive duration and appends it to rep
// as a UI timeline segment. It does NOT accumulate PresentSecs —
// that is computed at the end as LastSeen − FirstSeen.
func addInterval(rep *WorktimeReport, s, e time.Time) {
	if !e.After(s) {
		return
	}
	secs := int64(e.Sub(s).Seconds())
	rep.Intervals = append(rep.Intervals, Interval{
		StartMs: s.UnixMilli(),
		EndMs:   e.UnixMilli(),
		Secs:    secs,
	})
}

// standardSecs returns the configured workday length in seconds,
// e.g. 09:00→18:30 = 9.5h = 34200s. Falls back to 9h on parse error.
// Accepts HH:MM and HH:MM:SS.
func standardSecs(startHHMM, endHHMM string) int64 {
	s, ok1 := parseClock(startHHMM)
	e, ok2 := parseClock(endHHMM)
	if !ok1 || !ok2 || e <= s {
		return int64(9 * time.Hour / time.Second)
	}
	return int64(e - s)
}

// parseHHMM returns minutes since midnight. Kept for callers that
// only need minute resolution; new callers should use parseClock.
func parseHHMM(v string) (int, bool) {
	secs, ok := parseClock(v)
	if !ok {
		return 0, false
	}
	return secs / 60, true
}

// parseClock returns seconds since midnight. Accepts "HH:MM" and
// "HH:MM:SS" — the latter is what the worktime store now persists,
// so missing seconds aren't silently truncated to :00.
func parseClock(v string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 2 && len(parts) != 3 {
		return 0, false
	}
	var h, m, s int
	if _, err := fmt.Sscanf(parts[0], "%d", &h); err != nil {
		return 0, false
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &m); err != nil {
		return 0, false
	}
	if len(parts) == 3 {
		if _, err := fmt.Sscanf(parts[2], "%d", &s); err != nil {
			return 0, false
		}
	}
	if h < 0 || h > 24 || m < 0 || m > 59 || s < 0 || s > 59 {
		return 0, false
	}
	return h*3600 + m*60 + s, true
}
