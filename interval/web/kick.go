// kick.go — POST /api/devices/kick handler.
//
// Disconnects a single WiFi station so the client has to re-associate
// (and, depending on platform, fall back to mobile data or cycle Wi-Fi).
// Reuses the same staKickCmds + wifiRestartCmds tables that DHCP set
// uses internally — there's no second source of truth for "how to
// kick a station on this firmware".
//
// Wired devices are rejected up-front: deauth has no analogue on
// Ethernet, and shutting down a switch port for a single MAC isn't
// something we want to do casually from the dashboard.
//
// The "nuclear" wifi-restart fallback (restart_wifi=true) drops every
// WiFi client on every radio for a few seconds; it's only worth firing
// when per-station kick is a silent no-op on this firmware.

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// kickStation runs the surgical per-station deauth chain (staKickCmds)
// and, when restartWiFi is true, the nuclear wifi-restart chain too.
// Each command in the chain is tried in order; the first that exits 0
// wins (the rest are skipped). Returns a small report describing what
// actually fired so the UI can echo it back.
func kickStation(ctx context.Context, mac string, restartWiFi bool) kickReport {
	var rep kickReport

	// 1. Per-station kick (surgical).
	for _, tmpl := range staKickCmds {
		if len(tmpl) == 0 {
			continue
		}
		if _, err := exec.LookPath(tmpl[0]); err != nil {
			continue
		}
		argv := make([]string, len(tmpl))
		for i, a := range tmpl {
			argv[i] = strings.ReplaceAll(a, "{{MAC}}", mac)
		}
		ctxK, cancel := context.WithTimeout(ctx, 3*time.Second)
		cmd := exec.CommandContext(ctxK, argv[0], argv[1:]...)
		_, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			rep.Kicked = argv[0] + " " + argv[1] // e.g. "ubus call ahsapd.roaming staDisconnect"
			break
		}
	}

	// 2. Optional wifi-restart (everyone disconnects briefly).
	if restartWiFi {
		for _, argv := range wifiRestartCmds {
			if len(argv) == 0 {
				continue
			}
			if _, err := exec.LookPath(argv[0]); err != nil {
				continue
			}
			ctxR, cancel := context.WithTimeout(ctx, 10*time.Second)
			cmd := exec.CommandContext(ctxR, argv[0], argv[1:]...)
			_, err := cmd.CombinedOutput()
			cancel()
			if err == nil {
				rep.WiFiRestarted = argv[0] + " " + strings.Join(argv[1:], " ")
				break
			}
		}
	}
	return rep
}

type kickReport struct {
	Kicked        string `json:"kicked,omitempty"`
	WiFiRestarted string `json:"wifi_restarted,omitempty"`
}

// handleDeviceKick force-disconnects a single WiFi station.
//
//	POST /api/devices/kick
//	  { "mac": "AA:BB:CC:DD:EE:FF",
//	    "restart_wifi": false }    // optional nuclear fallback
//
//	→ 200 {"ok":true,"kicked":"...","wifi_restarted":"..."}
//	  202 ok=true with empty kicked/wifi_restarted means no command
//	      was available on this host — surfaced to the user as
//	      "踢下线指令已尝试, 但未找到可用方法".
//
// Refuses requests for wired devices (we don't currently know the
// switch port and don't want to disable interfaces blindly). Uses
// the offline cache to look up Wired status when the device is no
// longer in the watcher's known set.
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
	mac, err := validateMAC(in.MAC)
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
	rep := kickStation(ctx, mac, in.RestartWiFi)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":             true,
		"mac":            strings.ToUpper(mac),
		"kicked":         rep.Kicked,
		"wifi_restarted": rep.WiFiRestarted,
	})
}
