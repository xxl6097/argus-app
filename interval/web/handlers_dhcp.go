// handlers_dhcp.go — /api/dhcp CRUD on the owrt.DHCPManager from interval/owrt.
package web

import (
	"encoding/json"
	"net/http"

	"context"
	"errors"
	"github.com/xxl6097/argus-app/interval/owrt"
	"strings"
	"time"
)

func (s *Server) handleDHCP(w http.ResponseWriter, r *http.Request) {
	if s.dhcp == nil {
		http.Error(w, `{"error":"dhcp manager not configured"}`, http.StatusServiceUnavailable)
		return
	}
	// Recovery path: POST with ?purge_argus=1 wipes every argus_-owned
	// reservation without touching LuCI's anonymous entries. Exists for
	// the case where a bad argus-written entry broke DHCP and the user
	// needs to recover without editing /etc/config/dhcp by hand.
	if r.Method == http.MethodPost && r.URL.Query().Get("purge_argus") == "1" {
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		s.handleDHCPPurgeArgus(w, r)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleDHCPGet(w, r)
	case http.MethodPost:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		s.handleDHCPSet(w, r)
	case http.MethodDelete:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		s.handleDHCPDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleDHCPPurgeArgus bulk-removes every argus_-owned reservation.
// Requires a *owrt.UCIDHCPManager; returns 501 for any other owrt.DHCPManager
// implementation (the interface doesn't carry this method).
func (s *Server) handleDHCPPurgeArgus(w http.ResponseWriter, r *http.Request) {
	ucm, ok := s.dhcp.(*owrt.UCIDHCPManager)
	if !ok {
		writeJSONErr(w, http.StatusNotImplemented,
			"purge_argus only supported for owrt.UCIDHCPManager")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	n, err := ucm.PurgeArgusOwned(ctx)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"removed": n,
	})
}

func (s *Server) handleDHCPGet(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	leases, err := s.dhcp.List(ctx)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Strip the internal "#<idx>:" prefix before sending to clients.
	clean := make(map[string]owrt.StaticLease, len(leases))
	for mac, l := range leases {
		l.MAC = strings.ToUpper(mac)
		if i := strings.Index(l.Name, ":"); i >= 0 && strings.HasPrefix(l.Name, "#") {
			l.Name = l.Name[i+1:]
		}
		clean[strings.ToUpper(mac)] = l
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"leases": clean})
}

func (s *Server) handleDHCPSet(w http.ResponseWriter, r *http.Request) {
	var in owrt.StaticLease
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.dhcp.Set(ctx, in); err != nil {
		var conflict *owrt.ErrIPAlreadyReserved
		if errors.As(err, &conflict) {
			// 409 Conflict: the IP is already owned by a different MAC.
			// Surface the existing owner so the UI can say whose IP it is.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":     err.Error(),
				"ip":        conflict.IP,
				"owner_mac": strings.ToUpper(conflict.OwnerMAC),
			})
			return
		}
		writeJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Immediate-effect: reload daemons, prune stale lease, kick station.
	// Only run against the owrt.UCIDHCPManager (real implementation); test
	// stubs don't want side effects on the host.
	// restart_wifi=1 enables the nuclear option (full WiFi restart);
	// the user opts in per-request because it briefly disconnects every
	// WiFi client on the AP.
	restartWiFi := r.URL.Query().Get("restart_wifi") == "1"
	var report owrt.ApplyReport
	if _, ok := s.dhcp.(*owrt.UCIDHCPManager); ok {
		normMAC, _ := owrt.ValidateMAC(in.MAC)
		report = owrt.ApplyDHCPChanges(ctx, normMAC, restartWiFi)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":    true,
		"mac":   strings.ToUpper(in.MAC),
		"ip":    in.IP,
		"apply": report,
	})
}

func (s *Server) handleDHCPDelete(w http.ResponseWriter, r *http.Request) {
	mac := r.URL.Query().Get("mac")
	if mac == "" {
		writeJSONErr(w, http.StatusBadRequest, "mac query parameter required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := s.dhcp.Delete(ctx, mac); err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var report owrt.ApplyReport
	if _, ok := s.dhcp.(*owrt.UCIDHCPManager); ok {
		normMAC, _ := owrt.ValidateMAC(mac)
		report = owrt.ApplyDHCPChanges(ctx, normMAC, r.URL.Query().Get("restart_wifi") == "1")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":    true,
		"apply": report,
	})
}
