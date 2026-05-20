package web

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/xxl6097/argus-app/interval/store/override"
	"github.com/xxl6097/argus-app/interval/store/settings"
	argus "github.com/xxl6097/argusd"
)

// recordPunchCheckout has these branches:
//
//   1. non-OFFLINE event       → no-op
//   2. non-punch MAC           → no-op
//   3. nil overrides/settings  → no-op
//   4. OFFLINE before WorkEnd  → no-op
//   5. OFFLINE at WorkEnd      → write "out" = HH:MM
//   6. OFFLINE after WorkEnd   → write "out", preserve existing "in"
//   7. multiple after-end      → last-write-wins overwrites "out"

const checkoutMac = "aa:bb:cc:dd:ee:01"

func newCheckoutServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	overridesPath := filepath.Join(dir, "overrides.json")
	sett := settings.New("")
	if err := sett.Update(settings.Settings{WorkStart: "09:00", WorkEnd: "18:30"}); err != nil {
		t.Fatalf("settings update: %v", err)
	}
	if err := sett.AddPunch(checkoutMac); err != nil {
		t.Fatalf("AddPunch: %v", err)
	}
	overrides := override.New(overridesPath, nil)
	return &Server{settings: sett, overrides: overrides}, overridesPath
}

func mkOfflineCheckout(when time.Time, mac string) argus.Event {
	return argus.Event{
		Time:   when,
		Kind:   argus.EventOffline,
		Device: argus.Device{MAC: mac},
	}
}
func mkOnlineCheckout(when time.Time, mac string) argus.Event {
	return argus.Event{
		Time:   when,
		Kind:   argus.EventOnline,
		Device: argus.Device{MAC: mac},
	}
}

// 1. Non-OFFLINE event must not write.
func TestRecordPunchCheckout_OnlineIsNoop(t *testing.T) {
	s, _ := newCheckoutServer(t)
	when := time.Date(2026, 5, 19, 19, 0, 0, 0, time.Local) // after WorkEnd
	s.recordPunchCheckout(mkOnlineCheckout(when, checkoutMac))
	if _, ok := s.overrides.Lookup(checkoutMac, "2026-05-19"); ok {
		t.Fatalf("ONLINE event should not have written an override")
	}
}

// 2. Non-punch device must not write.
func TestRecordPunchCheckout_NonPunchIsNoop(t *testing.T) {
	s, _ := newCheckoutServer(t)
	other := "ff:ff:ff:00:00:01"
	when := time.Date(2026, 5, 19, 19, 0, 0, 0, time.Local)
	s.recordPunchCheckout(mkOfflineCheckout(when, other))
	if _, ok := s.overrides.Lookup(other, "2026-05-19"); ok {
		t.Fatalf("non-punch device should not have written an override")
	}
}

// 3. nil overrides → silent no-op (must not panic).
func TestRecordPunchCheckout_NoOverridesStore(t *testing.T) {
	sett := settings.New("")
	sett.Update(settings.Settings{WorkStart: "09:00", WorkEnd: "18:30"})
	sett.AddPunch(checkoutMac)
	s := &Server{settings: sett} // overrides == nil
	when := time.Date(2026, 5, 19, 19, 0, 0, 0, time.Local)
	s.recordPunchCheckout(mkOfflineCheckout(when, checkoutMac))
	// Test passes if no panic.
}

// 4. OFFLINE before WorkEnd must not write.
func TestRecordPunchCheckout_BeforeWorkEnd(t *testing.T) {
	s, _ := newCheckoutServer(t)
	when := time.Date(2026, 5, 19, 12, 30, 0, 0, time.Local) // 12:30 lunch
	s.recordPunchCheckout(mkOfflineCheckout(when, checkoutMac))
	if _, ok := s.overrides.Lookup(checkoutMac, "2026-05-19"); ok {
		t.Fatalf("OFFLINE before WorkEnd should not have written an override")
	}
}

// 5. OFFLINE at exactly WorkEnd writes the override.
func TestRecordPunchCheckout_AtWorkEnd(t *testing.T) {
	s, _ := newCheckoutServer(t)
	when := time.Date(2026, 5, 19, 18, 30, 0, 0, time.Local) // 18:30 sharp
	s.recordPunchCheckout(mkOfflineCheckout(when, checkoutMac))
	o, ok := s.overrides.Lookup(checkoutMac, "2026-05-19")
	if !ok {
		t.Fatalf("expected override to exist after at-WorkEnd checkout")
	}
	if o.Out != "18:30" {
		t.Errorf("Out = %q, want %q", o.Out, "18:30")
	}
}

// 6. OFFLINE after WorkEnd preserves an existing "in".
func TestRecordPunchCheckout_PreservesIn(t *testing.T) {
	s, _ := newCheckoutServer(t)
	// User filed an arrival manually earlier today.
	if err := s.overrides.Set(checkoutMac, "2026-05-19", override.Override{In: "08:45", Out: ""}); err != nil {
		t.Fatalf("seed In: %v", err)
	}
	when := time.Date(2026, 5, 19, 19, 15, 0, 0, time.Local)
	s.recordPunchCheckout(mkOfflineCheckout(when, checkoutMac))
	o, ok := s.overrides.Lookup(checkoutMac, "2026-05-19")
	if !ok {
		t.Fatalf("expected override to exist")
	}
	if o.In != "08:45" {
		t.Errorf("In should be preserved: got %q, want %q", o.In, "08:45")
	}
	if o.Out != "19:15" {
		t.Errorf("Out should be 19:15, got %q", o.Out)
	}
}

// 7. Subsequent after-end OFFLINE events overwrite "out" (last-write-wins).
func TestRecordPunchCheckout_LastWriteWins(t *testing.T) {
	s, _ := newCheckoutServer(t)
	first := time.Date(2026, 5, 19, 19, 0, 0, 0, time.Local)
	second := time.Date(2026, 5, 19, 20, 30, 0, 0, time.Local)
	s.recordPunchCheckout(mkOfflineCheckout(first, checkoutMac))
	s.recordPunchCheckout(mkOfflineCheckout(second, checkoutMac))
	o, ok := s.overrides.Lookup(checkoutMac, "2026-05-19")
	if !ok {
		t.Fatalf("expected override to exist")
	}
	if o.Out != "20:30" {
		t.Errorf("Out = %q, want last write 20:30", o.Out)
	}
}
