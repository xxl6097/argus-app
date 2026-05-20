// handlers_kick.go — /api/devices/kick (force-deauth a WiFi station).
package web

import (
	"encoding/json"
	"net/http"

	"context"
	"github.com/xxl6097/argus-app/interval/owrt"
	"strings"
	"time"
)

func (s *Server) handleDeviceKick(w http.ResponseWriter, r *http.Request) {
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
		MAC         string `json:"mac"`
		RestartWiFi bool   `json:"restart_wifi"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	mac, err := owrt.ValidateMAC(in.MAC)
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Look up wired-vs-wireless. The watcher's Known() is the
	// authoritative source for currently-online devices; the offline
	// cache fills in for "just dropped". A wired device should never
	// be deauth'd via hostapd.
	macLower := strings.ToLower(mac)
	for k, d := range s.watcher.Known() {
		if strings.ToLower(k) == macLower {
			if d.Wired() {
				writeJSONErr(w, http.StatusBadRequest, "wired device cannot be kicked offline")
				return
			}
			break
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	rep := owrt.KickStation(ctx, mac, in.RestartWiFi)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":             true,
		"mac":            strings.ToUpper(mac),
		"kicked":         rep.Kicked,
		"iwpriv_kicked":  rep.IwprivKicked,
		"wifi_restarted": rep.WiFiRestarted,
	})
}
