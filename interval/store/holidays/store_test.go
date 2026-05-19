package holidays

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

	store := NewWithSystem(manualPath, systemPath)
	loc := time.Local
	sat := time.Date(2026, 5, 17, 0, 0, 0, 0, loc)
	got := store.Kind(sat)
	if got != DayKindLegalHoliday {
		t.Errorf("Kind(2026-05-17) = %v, want DayKindLegalHoliday (manual should win)", got)
	}
}
