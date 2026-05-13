package web

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestParseClock verifies the HH:MM / HH:MM:SS parser handles all
// expected formats plus invalid input.
func TestParseClock(t *testing.T) {
	tests := []struct {
		in   string
		want int // seconds since midnight
		ok   bool
	}{
		{"09:00", 9*3600 + 0*60, true},
		{"18:30", 18*3600 + 30*60, true},
		{"07:52:47", 7*3600 + 52*60 + 47, true},
		{"23:59:59", 23*3600 + 59*60 + 59, true},
		{"00:00", 0, true},
		{"00:00:00", 0, true},
		{"", 0, false},
		{"9:00", 9 * 3600, true},  // single-digit hour is accepted by Sscanf
		{"25:00", 0, false},       // hour out of range
		{"12:60", 0, false},       // minute out of range
		{"12:30:60", 0, false},    // second out of range
		{"12:30:30:30", 0, false}, // too many colons
		{"abc", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseClock(tt.in)
		if tt.ok {
			if !ok {
				t.Errorf("parseClock(%q) unexpected error", tt.in)
				continue
			}
			if got != tt.want {
				t.Errorf("parseClock(%q) = %d, want %d", tt.in, got, tt.want)
			}
		} else {
			if ok {
				t.Errorf("parseClock(%q) expected error, got %d", tt.in, got)
			}
		}
	}
}

// TestComputeWorktime_DayKinds verifies the 5 day_kind branches produce
// correct overtime calculations and arrival/departure status.
func TestComputeWorktime_DayKinds(t *testing.T) {
	loc := time.Local
	// 2026-05-13 is a Wednesday (workday by default).
	// 2026-05-17 is a Saturday (weekend).

	mac := "AA:BB:CC:DD:EE:FF"
	// Seed a workday: first_in=08:00, last_out=19:00 (1h early + 30m late OT)
	workday := time.Date(2026, 5, 13, 0, 0, 0, 0, loc)
	workdayEntries := []HistoryEntry{
		{TimeMs: workday.Add(8 * time.Hour).UnixMilli(), Kind: "ONLINE", IP: "192.168.1.10", Hostname: "test"},
		{TimeMs: workday.Add(19 * time.Hour).UnixMilli(), Kind: "OFFLINE", IP: "192.168.1.10", Hostname: "test"},
	}

	// Seed a weekend: first_in=10:00, last_out=16:00 (6h present, all OT)
	weekend := time.Date(2026, 5, 17, 0, 0, 0, 0, loc)
	weekendEntries := []HistoryEntry{
		{TimeMs: weekend.Add(10 * time.Hour).UnixMilli(), Kind: "ONLINE", IP: "192.168.1.10", Hostname: "test"},
		{TimeMs: weekend.Add(16 * time.Hour).UnixMilli(), Kind: "OFFLINE", IP: "192.168.1.10", Hostname: "test"},
	}

	tests := []struct {
		name              string
		date              time.Time
		entries           []HistoryEntry
		dayKind           DayKind
		wantPresent       int64 // seconds
		wantEarlyOT       int64
		wantLateOT        int64
		wantOvertimeTotal int64
		wantArrival       string
		wantDeparture     string
	}{
		{
			name:              "workday: early+late OT",
			date:              workday,
			entries:           workdayEntries,
			dayKind:           DayKindWorkday,
			wantPresent:       11 * 3600, // 19:00 - 08:00
			wantEarlyOT:       1 * 3600,  // 09:00 - 08:00
			wantLateOT:        30 * 60,   // 19:00 - 18:30
			wantOvertimeTotal: 1*3600 + 30*60,
			wantArrival:       "", // not late
			wantDeparture:     "",
		},
		{
			name:              "weekend: all present = OT",
			date:              weekend,
			entries:           weekendEntries,
			dayKind:           DayKindWeekend,
			wantPresent:       6 * 3600,
			wantEarlyOT:       0,
			wantLateOT:        0,
			wantOvertimeTotal: 6 * 3600, // entire day
			wantArrival:       "",
			wantDeparture:     "",
		},
		{
			name:              "holiday: no OT",
			date:              workday,
			entries:           workdayEntries,
			dayKind:           DayKindLegalHoliday,
			wantPresent:       11 * 3600,
			wantEarlyOT:       0,
			wantLateOT:        0,
			wantOvertimeTotal: 0,
			wantArrival:       "",
			wantDeparture:     "",
		},
		{
			name:              "makeup: same as workday",
			date:              weekend,
			entries:           weekendEntries,
			dayKind:           DayKindMakeupWorkday,
			wantPresent:       7 * 3600, // 16:00 - min(10:00, 09:00) = 16:00 - 09:00 = 7h
			wantEarlyOT:       0,        // 10:00 > 09:00, so late
			wantLateOT:        0,        // 16:00 < 18:30, so early leave
			wantOvertimeTotal: 0,
			wantArrival:       "late",
			wantDeparture:     "early_leave",
		},
		{
			name:              "otday: all present = OT",
			date:              workday,
			entries:           workdayEntries,
			dayKind:           DayKindOTDay,
			wantPresent:       11 * 3600,
			wantEarlyOT:       0,
			wantLateOT:        0,
			wantOvertimeTotal: 11 * 3600,
			wantArrival:       "",
			wantDeparture:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := tt.date.Add(24 * time.Hour) // next day
			got := ComputeWorktime(mac, tt.date, "09:00", "18:30", tt.entries, now, Override{}, tt.dayKind)
			if got.PresentSecs != tt.wantPresent {
				t.Errorf("present_secs = %d, want %d", got.PresentSecs, tt.wantPresent)
			}
			if got.EarlyOTSecs != tt.wantEarlyOT {
				t.Errorf("early_ot_secs = %d, want %d", got.EarlyOTSecs, tt.wantEarlyOT)
			}
			if got.LateOTSecs != tt.wantLateOT {
				t.Errorf("late_ot_secs = %d, want %d", got.LateOTSecs, tt.wantLateOT)
			}
			if got.OvertimeSecs != tt.wantOvertimeTotal {
				t.Errorf("overtime_secs = %d, want %d", got.OvertimeSecs, tt.wantOvertimeTotal)
			}
			if got.ArrivalStatus != tt.wantArrival {
				t.Errorf("arrival_status = %q, want %q", got.ArrivalStatus, tt.wantArrival)
			}
			if got.DepartureStatus != tt.wantDeparture {
				t.Errorf("departure_status = %q, want %q", got.DepartureStatus, tt.wantDeparture)
			}
		})
	}
}

// TestComputeWorktime_ArrivalStatus verifies late / missed_in detection.
func TestComputeWorktime_ArrivalStatus(t *testing.T) {
	loc := time.Local
	mac := "AA:BB:CC:DD:EE:FF"
	date := time.Date(2026, 5, 13, 0, 0, 0, 0, loc)

	tests := []struct {
		name        string
		firstIn     time.Duration // offset from midnight
		wantArrival string
	}{
		{"on time", 9*time.Hour + 0*time.Minute, ""},
		{"early", 8*time.Hour + 30*time.Minute, ""},
		{"late by 1 min", 9*time.Hour + 1*time.Minute, "late"},
		{"late by 1 hour", 10 * time.Hour, "late"},
		{"missed_in (after end)", 19 * time.Hour, "missed_in"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := []HistoryEntry{
				{TimeMs: date.Add(tt.firstIn).UnixMilli(), Kind: "ONLINE", IP: "192.168.1.10", Hostname: "test"},
				{TimeMs: date.Add(20 * time.Hour).UnixMilli(), Kind: "OFFLINE", IP: "192.168.1.10", Hostname: "test"},
			}
			now := date.Add(24 * time.Hour)
			got := ComputeWorktime(mac, date, "09:00", "18:30", entries, now, Override{}, DayKindWorkday)
			if got.ArrivalStatus != tt.wantArrival {
				t.Errorf("arrival_status = %q, want %q", got.ArrivalStatus, tt.wantArrival)
			}
		})
	}
}

// TestComputeWorktime_DepartureStatus verifies early_leave detection.
func TestComputeWorktime_DepartureStatus(t *testing.T) {
	loc := time.Local
	mac := "AA:BB:CC:DD:EE:FF"
	date := time.Date(2026, 5, 13, 0, 0, 0, 0, loc)

	tests := []struct {
		name          string
		lastOut       time.Duration
		wantDeparture string
	}{
		{"on time", 18*time.Hour + 30*time.Minute, ""},
		{"late", 19 * time.Hour, ""},
		{"early by 1 min", 18*time.Hour + 29*time.Minute, "early_leave"},
		{"early by 1 hour", 17*time.Hour + 30*time.Minute, "early_leave"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := []HistoryEntry{
				{TimeMs: date.Add(8 * time.Hour).UnixMilli(), Kind: "ONLINE", IP: "192.168.1.10", Hostname: "test"},
				{TimeMs: date.Add(tt.lastOut).UnixMilli(), Kind: "OFFLINE", IP: "192.168.1.10", Hostname: "test"},
			}
			now := date.Add(24 * time.Hour)
			got := ComputeWorktime(mac, date, "09:00", "18:30", entries, now, Override{}, DayKindWorkday)
			if got.DepartureStatus != tt.wantDeparture {
				t.Errorf("departure_status = %q, want %q", got.DepartureStatus, tt.wantDeparture)
			}
		})
	}
}

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
	store := NewOverrideStore(path, nil)
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
	if !contains(string(raw), `"2026-05"`) {
		t.Errorf("expected nested month key 2026-05 in migrated file, got:\n%s", raw)
	}
}

// TestHolidayStore_Priority verifies manual > system > weekend fallback.
func TestHolidayStore_Priority(t *testing.T) {
	dir := t.TempDir()
	manualPath := filepath.Join(dir, "holidays.json")
	systemPath := filepath.Join(dir, "holidays_system.json")

	// System layer says 2026-05-17 (Saturday) is a makeup workday.
	system := `{"2026-05-17": "workday"}`
	if err := os.WriteFile(systemPath, []byte(system), 0644); err != nil {
		t.Fatal(err)
	}
	// Manual layer overrides it to holiday.
	manual := `{"2026-05-17": "holiday"}`
	if err := os.WriteFile(manualPath, []byte(manual), 0644); err != nil {
		t.Fatal(err)
	}

	store := NewHolidayStoreWithSystem(manualPath, systemPath)
	loc := time.Local
	sat := time.Date(2026, 5, 17, 0, 0, 0, 0, loc)
	got := store.Kind(sat)
	if got != DayKindLegalHoliday {
		t.Errorf("Kind(2026-05-17) = %v, want DayKindLegalHoliday (manual should win)", got)
	}
}

// Helper: check if s contains substr.
func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
