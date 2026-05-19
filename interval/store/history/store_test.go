package history

import (
	"github.com/xxl6097/argus-app/interval/store/override"
	"github.com/xxl6097/argus-app/interval/store/holidays"
	"testing"
	"time"
	"github.com/xxl6097/argus-app/interval/util"
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
		got, ok := util.ParseClock(tt.in)
		if tt.ok {
			if !ok {
				t.Errorf("util.ParseClock(%q) unexpected error", tt.in)
				continue
			}
			if got != tt.want {
				t.Errorf("util.ParseClock(%q) = %d, want %d", tt.in, got, tt.want)
			}
		} else {
			if ok {
				t.Errorf("util.ParseClock(%q) expected error, got %d", tt.in, got)
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
		dayKind           holidays.DayKind
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
			dayKind:           holidays.DayKindWorkday,
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
			dayKind:           holidays.DayKindWeekend,
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
			dayKind:           holidays.DayKindLegalHoliday,
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
			dayKind:           holidays.DayKindMakeupWorkday,
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
			dayKind:           holidays.DayKindOTDay,
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
			got := ComputeWorktime(mac, tt.date, "09:00", "18:30", tt.entries, now, override.Override{}, tt.dayKind)
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
			got := ComputeWorktime(mac, date, "09:00", "18:30", entries, now, override.Override{}, holidays.DayKindWorkday)
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
			got := ComputeWorktime(mac, date, "09:00", "18:30", entries, now, override.Override{}, holidays.DayKindWorkday)
			if got.DepartureStatus != tt.wantDeparture {
				t.Errorf("departure_status = %q, want %q", got.DepartureStatus, tt.wantDeparture)
			}
		})
	}
}
