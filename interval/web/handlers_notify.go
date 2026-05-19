// handlers_notify.go — /api/notifications CRUD, /api/notifications/test
// (synthetic event for verifying webhook/ntfy wiring),
// /api/notifications/messages (recent ntfy res-topic inbox), and the
// refreshNotifySubs helper called after a config write.

package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	argus "github.com/xxl6097/argusd"
)

// handleNotifications multiplexes GET / POST / DELETE on
// /api/notifications. Keyed by MAC on the wire (we store by alias
// internally for readability; that's transparent to callers).
//
//	GET    /api/notifications?mac=XX   -> {"mac":"XX","config":{...},"exists":bool}
//	POST   /api/notifications  {mac, webhook_url, ntfy_*}  -> {"ok":true}
//	DELETE /api/notifications?mac=XX  -> {"ok":true}  (clears entry)
//
// POST / DELETE are gated by writeAuth. After a mutation the notifier
// rebuilds its subscription set so the new config takes effect within
// one event loop.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	if s.notifyStore == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "notifications not enabled")
		return
	}
	switch r.Method {
	case http.MethodGet:
		mac := r.URL.Query().Get("mac")
		if mac == "" {
			writeJSONErr(w, http.StatusBadRequest, "mac query parameter required")
			return
		}
		cfg, ok := s.notifyStore.Lookup(mac)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mac":    strings.ToUpper(normalizeMAC(mac)),
			"exists": ok,
			"config": cfg,
		})
	case http.MethodPost:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		var in struct {
			MAC string `json:"mac"`
			NotifyConfig
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if in.MAC == "" {
			writeJSONErr(w, http.StatusBadRequest, "mac required")
			return
		}
		if err := s.notifyStore.Set(in.MAC, in.NotifyConfig); err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		s.refreshNotifySubs()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	case http.MethodDelete:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		mac := r.URL.Query().Get("mac")
		if mac == "" {
			writeJSONErr(w, http.StatusBadRequest, "mac query parameter required")
			return
		}
		if err := s.notifyStore.Delete(mac); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.refreshNotifySubs()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleNotificationTest fires a synthetic ONLINE/OFFLINE event for
// the given MAC and dispatches it through the regular pipeline. Used
// to verify webhook/ntfy markdown without waiting for a real flap.
//
//	POST /api/notifications/test  {mac, kind: "ONLINE"|"OFFLINE"}
//
// Gated by writeAuth.
func (s *Server) handleNotificationTest(w http.ResponseWriter, r *http.Request) {
	if s.notifier == nil || s.notifyStore == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "notifications not enabled")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.writeAuth(r) {
		writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
		return
	}
	var in struct {
		MAC  string `json:"mac"`
		Kind string `json:"kind"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if in.MAC == "" {
		writeJSONErr(w, http.StatusBadRequest, "mac required")
		return
	}
	kind := argus.EventOnline
	if strings.ToUpper(in.Kind) == "OFFLINE" {
		kind = argus.EventOffline
	}
	// Pull whatever device snapshot the watcher has; falls back to a
	// minimal stub if the device isn't currently known.
	dev := argus.Device{MAC: strings.ToUpper(normalizeMAC(in.MAC))}
	for mac, d := range s.watcher.Known() {
		if normalizeMAC(mac) == normalizeMAC(in.MAC) {
			dev = d
			break
		}
	}
	s.dispatchNotify(argus.Event{
		Time:   time.Now(),
		Kind:   kind,
		Device: dev,
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "kind": kind.String()})
}

// handleNotificationMessages returns recent ntfy res-topic messages
// for a MAC. Newest first, up to 100.
//
//	GET /api/notifications/messages?mac=XX -> {"mac":"XX","messages":[...]}
func (s *Server) handleNotificationMessages(w http.ResponseWriter, r *http.Request) {
	if s.notifier == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "notifications not enabled")
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
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"mac":      strings.ToUpper(normalizeMAC(mac)),
		"messages": s.notifier.Inbox(mac),
	})
}

// refreshNotifySubs rebuilds ntfy subscriptions after a config change.
// Uses the watcher's known set to resolve alias keys back to MACs.
func (s *Server) refreshNotifySubs() {
	if s.notifier == nil {
		return
	}
	resolver := func(key string) string {
		if s.aliases == nil {
			return ""
		}
		for mac := range s.watcher.Known() {
			if name := s.aliases.Lookup(mac); name == key {
				return mac
			}
		}
		return ""
	}
	s.notifier.EnsureSubscriptions(resolver)
}
