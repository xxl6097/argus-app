// Package util holds tiny dependency-free helpers shared by every other
// package under interval/. Keep its surface narrow — anything that
// needs more than a stdlib import probably belongs somewhere else.
package util

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NormalizeMAC lowercases and trims a MAC string. Returns "" for empty
// input. Doesn't validate format — callers that need that should use
// store/notify or wire/regex helpers.
func NormalizeMAC(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// NonZeroTime returns t when non-zero, time.Now() otherwise. Used by
// event-driven stores so a missing event timestamp degrades to "now"
// instead of 0001-01-01.
func NonZeroTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

// ParseClock parses an HH:MM or HH:MM:SS string and returns
// seconds-since-midnight. Returns (0, false) on malformed input.
// Accepts hour=24 (used as "end of day" sentinel by some callers).
func ParseClock(v string) (int, bool) {
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

// WriteJSONAtomic writes content to path via tmp file + rename, with
// the given file mode. Creates parent directories with 0755 if missing.
// Used by every JSON store so a crash mid-write never produces a
// half-baked file.
func WriteJSONAtomic(path string, body []byte, mode os.FileMode) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
