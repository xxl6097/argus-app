// handlers_worktime.go — read endpoints + override CRUD for the
// punch-device worktime feature.
//
// /api/history serves the raw transition log; /api/worktime computes
// a single-day report; /api/worktime/month aggregates a calendar
// month; /api/worktime/override CRUDs the manual-entry layer that
// fills in days the watcher missed.

package web

import (
	"encoding/json"
	"github.com/xxl6097/argus-app/interval/store/history"
	"github.com/xxl6097/argus-app/interval/store/override"
	"github.com/xxl6097/argus-app/interval/util"
	"net/http"
	"strings"
	"time"
)

// handleHistory serves GET /api/history?mac=XX&from=YYYY-MM-DD&to=YYYY-MM-DD
// Returns the online/offline transition log for a MAC. from/to are
// optional; defaults to "last 30 days" when omitted.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "history not enabled")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	mac := r.URL.Query().Get("mac")
	if mac == "" {
		writeJSONErr(w, http.StatusBadRequest, "mac query parameter required")
		return
	}
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")
	var from, to time.Time
	if fromStr != "" {
		var err error
		from, err = time.ParseInLocation("2006-01-02", fromStr, time.Local)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "from must be YYYY-MM-DD")
			return
		}
	} else {
		from = time.Now().Add(-history.Retention)
	}
	if toStr != "" {
		var err error
		to, err = time.ParseInLocation("2006-01-02", toStr, time.Local)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "to must be YYYY-MM-DD")
			return
		}
		to = to.Add(24 * time.Hour) // include the entire day
	}
	entries, err := s.history.Query(mac, from, to)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"mac":     strings.ToUpper(util.NormalizeMAC(mac)),
		"entries": entries,
		"count":   len(entries),
	})
}

// handleWorktime serves GET /api/worktime?mac=XX&date=YYYY-MM-DD&start=HH:MM&end=HH:MM
// Returns a computed worktime report for the given MAC on the given
// date. start/end override the stored settings when provided.
func (s *Server) handleWorktime(w http.ResponseWriter, r *http.Request) {
	if s.history == nil || s.settings == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "worktime not enabled")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	mac := r.URL.Query().Get("mac")
	if mac == "" {
		writeJSONErr(w, http.StatusBadRequest, "mac query parameter required")
		return
	}
	dateStr := r.URL.Query().Get("date")
	if dateStr == "" {
		dateStr = time.Now().Format("2006-01-02")
	}
	// Parse the date in the server's local time zone, not UTC: "today"
	// means local midnight 00:00 to 24:00, so standard-hour overtime
	// comparisons line up with the user's wall clock.
	date, err := time.ParseInLocation("2006-01-02", dateStr, time.Local)
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}
	cfg := s.settings.Get()
	startHHMM := r.URL.Query().Get("start")
	if startHHMM == "" {
		startHHMM = cfg.WorkStart
	}
	endHHMM := r.URL.Query().Get("end")
	if endHHMM == "" {
		endHHMM = cfg.WorkEnd
	}
	// Fetch entries covering [date-1d, date+1d] so we can see the
	// transition that opened a segment before 00:00.
	from := date.Add(-24 * time.Hour)
	to := date.Add(48 * time.Hour)
	entries, err := s.history.Query(mac, from, to)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var override override.Override
	if s.overrides != nil {
		if o, ok := s.overrides.Lookup(mac, date.Format("2006-01-02")); ok {
			override = o
		}
	}
	rep := history.ComputeWorktime(mac, date, startHHMM, endHHMM, entries, time.Now(), override, s.dayKindFor(date))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
}

// handleWorktimeMonth serves GET /api/worktime/month?mac=XX&month=YYYY-MM&start=HH:MM&end=HH:MM
// Aggregates daily worktime reports across the month. Zero-present
// days are omitted. Overrides are applied per-day, same as the
// single-day endpoint.
func (s *Server) handleWorktimeMonth(w http.ResponseWriter, r *http.Request) {
	if s.history == nil || s.settings == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "worktime not enabled")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	mac := r.URL.Query().Get("mac")
	if mac == "" {
		writeJSONErr(w, http.StatusBadRequest, "mac query parameter required")
		return
	}
	monthStr := r.URL.Query().Get("month")
	if monthStr == "" {
		monthStr = time.Now().Format("2006-01")
	}
	monthStart, err := time.ParseInLocation("2006-01", monthStr, time.Local)
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, "month must be YYYY-MM")
		return
	}
	cfg := s.settings.Get()
	startHHMM := r.URL.Query().Get("start")
	if startHHMM == "" {
		startHHMM = cfg.WorkStart
	}
	endHHMM := r.URL.Query().Get("end")
	if endHHMM == "" {
		endHHMM = cfg.WorkEnd
	}

	// Single history read covering the whole month (+1 day padding on
	// each side for "was online at 00:00" detection).
	monthEnd := monthStart.AddDate(0, 1, 0)
	from := monthStart.Add(-24 * time.Hour)
	to := monthEnd.Add(24 * time.Hour)
	entries, err := s.history.Query(mac, from, to)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	rep := history.MonthlyReport{
		MAC:       strings.ToUpper(util.NormalizeMAC(mac)),
		Month:     monthStart.Format("2006-01"),
		StartHHMM: startHHMM,
		EndHHMM:   endHHMM,
		Days:      []history.DayWorktime{},
	}
	now := time.Now()
	// Cap iteration at "today" so future days of the current month
	// don't create fake zero-present entries.
	lastDay := monthEnd
	if now.Before(lastDay) {
		lastDay = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).Add(24 * time.Hour)
	}
	for day := monthStart; day.Before(lastDay); day = day.AddDate(0, 0, 1) {
		var override override.Override
		if s.overrides != nil {
			if o, ok := s.overrides.Lookup(mac, day.Format("2006-01-02")); ok {
				override = o
			}
		}
		dr := history.ComputeWorktime(mac, day, startHHMM, endHHMM, entries, now, override, s.dayKindFor(day))
		if dr.PresentSecs <= 0 && !dr.Manual {
			continue // skip days the device never showed up (and wasn't manually filled)
		}
		rep.Days = append(rep.Days, history.DayWorktime{
			Date:            dr.Date,
			PresentSecs:     dr.PresentSecs,
			OvertimeSecs:    dr.OvertimeSecs,
			EarlyOTSecs:     dr.EarlyOTSecs,
			LateOTSecs:      dr.LateOTSecs,
			FirstSeenMs:     dr.FirstSeenMs,
			LastSeenMs:      dr.LastSeenMs,
			ArrivalStatus:   dr.ArrivalStatus,
			DepartureStatus: dr.DepartureStatus,
			DayKind:         dr.DayKind,
			OTDay:           dr.OTDay,
			Manual:          dr.Manual,
			MissingOut:      dr.MissingOut,
			OpenAtEnd:       dr.OpenAtEnd,
		})
		rep.PresentSecs += dr.PresentSecs
		rep.OvertimeSecs += dr.OvertimeSecs
		rep.EarlyOTSecs += dr.EarlyOTSecs
		rep.LateOTSecs += dr.LateOTSecs
		rep.WorkedDays++
		if dr.OTDay {
			rep.OTDays++
		}
		switch dr.ArrivalStatus {
		case "late":
			rep.LateDays++
		case "missed_in":
			rep.MissedInDays++
		}
		if dr.DepartureStatus == "early_leave" {
			rep.EarlyLeaveDays++
		}
	}

	if rep.WorkedDays > 0 {
		rep.AvgDailyOTSecs = rep.OvertimeSecs / int64(rep.WorkedDays)
		// Weekly avg prorates the daily figure to a 5-day work-week.
		// Using 5 (not 7) matches how people typically interpret
		// "周均" for office hours — 5 workdays, not every calendar day.
		rep.AvgWeeklyOTSecs = rep.AvgDailyOTSecs * 5
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
}

// handleWorktimeOverride manages per-(mac, date) manual in/out records.
//
//	GET    /api/worktime/override?mac=XX&date=YYYY-MM-DD
//	POST   /api/worktime/override  {mac, date, in, out}
//	DELETE /api/worktime/override?mac=XX&date=YYYY-MM-DD
//
// POST / DELETE are gated by writeAuth.
func (s *Server) handleWorktimeOverride(w http.ResponseWriter, r *http.Request) {
	if s.overrides == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "overrides not enabled")
		return
	}
	switch r.Method {
	case http.MethodGet:
		mac := r.URL.Query().Get("mac")
		date := r.URL.Query().Get("date")
		if mac == "" || date == "" {
			writeJSONErr(w, http.StatusBadRequest, "mac and date query parameters required")
			return
		}
		o, ok := s.overrides.Lookup(mac, date)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mac":      strings.ToUpper(util.NormalizeMAC(mac)),
			"date":     date,
			"exists":   ok,
			"override": o,
		})
	case http.MethodPost:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		var in struct {
			MAC  string `json:"mac"`
			Date string `json:"date"`
			In   string `json:"in"`
			Out  string `json:"out"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := s.overrides.Set(in.MAC, in.Date, override.Override{In: in.In, Out: in.Out}); err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	case http.MethodDelete:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		mac := r.URL.Query().Get("mac")
		date := r.URL.Query().Get("date")
		if mac == "" || date == "" {
			writeJSONErr(w, http.StatusBadRequest, "mac and date query parameters required")
			return
		}
		if err := s.overrides.Delete(mac, date); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
