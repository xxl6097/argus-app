// handlers_settings.go — /api/settings + /api/holidays.
//
// Both expose the same GET/POST/DELETE shape; settings handles the
// punch device set, work hours, global webhook URL and dingtalk
// keyword. Holidays is the merged manual + system view.
//
// Mutating methods are gated by s.writeAuth in addition to requireAuth.

package web

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleSettings multiplexes GET / POST / DELETE on /api/settings.
//
// GET  /api/settings
//
//	-> {"punch_macs": ["AA:..", "BB:.."], "work_start": "09:00", "work_end": "18:30",
//	    "global_webhook_url": "...", "webhook_keyword": "..."}
//
// POST /api/settings
//
//	Body shapes (multiplexed on what fields are present):
//	  - {"punch_mac": "AA:..", "punch": true}    add a 打卡设备
//	  - {"punch_mac": "AA:..", "punch": false}   remove a 打卡设备
//	  - {"work_start": "09:00", "work_end": "18:30"}  update hours
//	  - {"global_webhook_url": "https://..."}    set/clear global webhook
//	  - {"webhook_keyword": "argus"}             set/clear dingtalk keyword
//	Combined bodies are fine; each field is applied if set.
//
// DELETE /api/settings
//   - ?clear=punch        wipe the entire 打卡设备 set
//   - ?clear=me           alias of clear=punch (legacy)
//   - ?mac=AA:..          remove a single mac from the set
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if s.settings == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "settings not enabled")
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg := s.settings.Get()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"punch_macs":         s.settings.PunchMACsUpper(),
			"work_start":         cfg.WorkStart,
			"work_end":           cfg.WorkEnd,
			"global_webhook_url": cfg.GlobalWebhookURL,
			"webhook_keyword":    cfg.WebhookKeyword,
		})
	case http.MethodPost:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		var in struct {
			PunchMAC         *string `json:"punch_mac,omitempty"`
			Punch            *bool   `json:"punch,omitempty"`
			WorkStart        string  `json:"work_start,omitempty"`
			WorkEnd          string  `json:"work_end,omitempty"`
			GlobalWebhookURL *string `json:"global_webhook_url,omitempty"`
			WebhookKeyword   *string `json:"webhook_keyword,omitempty"`
			// Legacy alias: older clients posted {"me_mac": "AA:.."} to
			// set the single punch device. Treat it as add-only.
			MeMAC string `json:"me_mac,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if in.WorkStart != "" || in.WorkEnd != "" {
			if err := s.settings.Update(Settings{WorkStart: in.WorkStart, WorkEnd: in.WorkEnd}); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if in.GlobalWebhookURL != nil {
			if err := s.settings.SetGlobalWebhook(*in.GlobalWebhookURL); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		if in.WebhookKeyword != nil {
			if err := s.settings.SetWebhookKeyword(*in.WebhookKeyword); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		// Punch toggle: punch_mac + explicit true/false flag. When
		// punch is omitted, default to add (true) for convenience.
		if in.PunchMAC != nil && *in.PunchMAC != "" {
			add := true
			if in.Punch != nil {
				add = *in.Punch
			}
			if add {
				if err := s.settings.AddPunch(*in.PunchMAC); err != nil {
					writeJSONErr(w, http.StatusBadRequest, err.Error())
					return
				}
			} else {
				if err := s.settings.RemovePunch(*in.PunchMAC); err != nil {
					writeJSONErr(w, http.StatusInternalServerError, err.Error())
					return
				}
			}
		}
		// Legacy me_mac: add to the set (don't replace existing).
		if in.MeMAC != "" {
			if err := s.settings.AddPunch(in.MeMAC); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":         true,
			"punch_macs": s.settings.PunchMACsUpper(),
		})
	case http.MethodDelete:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		if mac := r.URL.Query().Get("mac"); mac != "" {
			if err := s.settings.RemovePunch(mac); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		clear := strings.TrimSpace(r.URL.Query().Get("clear"))
		if clear == "me" || clear == "punch" {
			if err := s.settings.ClearPunchAll(); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		writeJSONErr(w, http.StatusBadRequest, "provide ?mac=... or ?clear=punch")
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleHolidays multiplexes GET / POST / DELETE on /api/holidays.
//
//	GET    /api/holidays                  -> {"holidays": {"YYYY-MM-DD": "holiday"|"workday"}}
//	POST   /api/holidays  {date, kind}    -> {"ok": true}   kind ∈ {"holiday","workday",""}
//	DELETE /api/holidays?date=YYYY-MM-DD  -> {"ok": true}
func (s *Server) handleHolidays(w http.ResponseWriter, r *http.Request) {
	if s.holidays == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "holidays not enabled")
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"holidays": s.holidays.All()})
	case http.MethodPost:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		var in struct {
			Date string `json:"date"`
			Kind string `json:"kind"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := s.holidays.Set(in.Date, in.Kind); err != nil {
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
		date := r.URL.Query().Get("date")
		if err := s.holidays.Set(date, ""); err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}
