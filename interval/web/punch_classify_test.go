package web

import (
	"testing"
	"time"

	"github.com/xxl6097/argus-app/interval/store/history"
	"github.com/xxl6097/argus-app/interval/store/settings"
	argus "github.com/xxl6097/argusd"
)

// classifyPunchEvent has four code paths:
//
//   1. not a punch device → punchEventNotPunch
//   2. ONLINE, no prior ONLINE today → punchEventCheckIn
//   3. ONLINE, prior ONLINE recorded today → punchEventTransient
//   4. OFFLINE before WorkEnd → punchEventTransient
//   5. OFFLINE at/after WorkEnd → punchEventCheckOut
//
// The history Query is mac-keyed and writes to a temp dir, so each
// test gets an isolated store.

const (
	tcMac       = "aa:bb:cc:dd:ee:ff"
	tcWorkStart = "09:00"
	tcWorkEnd   = "18:30"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	hist := history.New(dir)
	sett := settings.New("") // in-memory
	if err := sett.Update(settings.Settings{WorkStart: tcWorkStart, WorkEnd: tcWorkEnd}); err != nil {
		t.Fatalf("settings update: %v", err)
	}
	if err := sett.AddPunch(tcMac); err != nil {
		t.Fatalf("AddPunch: %v", err)
	}
	return &Server{history: hist, settings: sett}
}

func mkOnline(when time.Time) argus.Event {
	return argus.Event{
		Time:   when,
		Kind:   argus.EventOnline,
		Device: argus.Device{MAC: tcMac},
	}
}
func mkOffline(when time.Time) argus.Event {
	return argus.Event{
		Time:   when,
		Kind:   argus.EventOffline,
		Device: argus.Device{MAC: tcMac},
	}
}

// TestClassifyPunchEvent_NotPunch — non-punch device should always
// return NotPunch regardless of timing.
func TestClassifyPunchEvent_NotPunch(t *testing.T) {
	s := newTestServer(t)
	when := time.Date(2026, 5, 19, 10, 0, 0, 0, time.Local)
	got := s.classifyPunchEvent(mkOnline(when), false /*isPunch*/, when)
	if got != punchEventNotPunch {
		t.Errorf("non-punch device: got %v, want punchEventNotPunch", got)
	}
}

// TestClassifyPunchEvent_FirstOnline — first ONLINE of the day should
// be classified as a real check-in.
func TestClassifyPunchEvent_FirstOnline(t *testing.T) {
	s := newTestServer(t)
	when := time.Date(2026, 5, 19, 9, 5, 0, 0, time.Local)
	got := s.classifyPunchEvent(mkOnline(when), true, when)
	if got != punchEventCheckIn {
		t.Errorf("first ONLINE today: got %v, want punchEventCheckIn", got)
	}
}

// TestClassifyPunchEvent_RepeatOnline — a second ONLINE the same day
// (after a brief OFFLINE) is transient.
func TestClassifyPunchEvent_RepeatOnline(t *testing.T) {
	s := newTestServer(t)
	day := time.Date(2026, 5, 19, 0, 0, 0, 0, time.Local)
	// Record a prior ONLINE+OFFLINE earlier today.
	s.history.Record(mkOnline(day.Add(9*time.Hour+5*time.Minute)), "test")
	s.history.Record(mkOffline(day.Add(12*time.Hour)), "test")
	now := day.Add(13 * time.Hour) // lunch return at 13:00
	got := s.classifyPunchEvent(mkOnline(now), true, now)
	if got != punchEventTransient {
		t.Errorf("second ONLINE today: got %v, want punchEventTransient", got)
	}
}

// TestClassifyPunchEvent_OfflineBeforeWorkEnd — OFFLINE in the middle
// of the day (lunch break, brief drop) is transient.
func TestClassifyPunchEvent_OfflineBeforeWorkEnd(t *testing.T) {
	s := newTestServer(t)
	day := time.Date(2026, 5, 19, 0, 0, 0, 0, time.Local)
	// Have an ONLINE earlier so a real OFFLINE makes sense.
	s.history.Record(mkOnline(day.Add(9*time.Hour)), "test")
	when := day.Add(12*time.Hour + 30*time.Minute) // lunch drop
	got := s.classifyPunchEvent(mkOffline(when), true, when)
	if got != punchEventTransient {
		t.Errorf("OFFLINE 12:30 (before 18:30): got %v, want punchEventTransient", got)
	}
}

// TestClassifyPunchEvent_OfflineAtWorkEnd — OFFLINE exactly at WorkEnd
// counts as a real check-out (boundary inclusive).
func TestClassifyPunchEvent_OfflineAtWorkEnd(t *testing.T) {
	s := newTestServer(t)
	day := time.Date(2026, 5, 19, 0, 0, 0, 0, time.Local)
	when := day.Add(18*time.Hour + 30*time.Minute) // 18:30 sharp
	got := s.classifyPunchEvent(mkOffline(when), true, when)
	if got != punchEventCheckOut {
		t.Errorf("OFFLINE at 18:30: got %v, want punchEventCheckOut", got)
	}
}

// TestClassifyPunchEvent_OfflineAfterWorkEnd — OFFLINE after WorkEnd
// is the canonical end-of-day check-out.
func TestClassifyPunchEvent_OfflineAfterWorkEnd(t *testing.T) {
	s := newTestServer(t)
	day := time.Date(2026, 5, 19, 0, 0, 0, 0, time.Local)
	when := day.Add(20 * time.Hour) // 20:00, well past WorkEnd
	got := s.classifyPunchEvent(mkOffline(when), true, when)
	if got != punchEventCheckOut {
		t.Errorf("OFFLINE 20:00: got %v, want punchEventCheckOut", got)
	}
}

// TestClassifyPunchEvent_NoStores — when history or settings are nil
// the classifier degrades to NotPunch instead of panicking. Some test
// setups omit those stores and still call dispatchNotify.
func TestClassifyPunchEvent_NoStores(t *testing.T) {
	s := &Server{} // no history, no settings
	when := time.Date(2026, 5, 19, 10, 0, 0, 0, time.Local)
	got := s.classifyPunchEvent(mkOnline(when), true, when)
	if got != punchEventNotPunch {
		t.Errorf("no stores: got %v, want punchEventNotPunch", got)
	}
}

// TestClassifyPunchEvent_PriorDayOnlineDoesntCount — a prior-day
// ONLINE (e.g. left router on overnight) should not turn today's first
// ONLINE into transient.
func TestClassifyPunchEvent_PriorDayOnlineDoesntCount(t *testing.T) {
	s := newTestServer(t)
	yesterday := time.Date(2026, 5, 18, 14, 0, 0, 0, time.Local)
	s.history.Record(mkOnline(yesterday), "test")
	today := time.Date(2026, 5, 19, 9, 5, 0, 0, time.Local)
	got := s.classifyPunchEvent(mkOnline(today), true, today)
	if got != punchEventCheckIn {
		t.Errorf("today's first ONLINE (with prior-day ONLINE only): got %v, want punchEventCheckIn", got)
	}
}
