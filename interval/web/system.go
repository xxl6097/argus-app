// system.go — POST /api/system/reboot and POST /api/system/restart-network
// handlers.
//
// Both are exposed via Server.mux in newServer and auth-gated by
// writeAuth. Reboot runs /sbin/reboot in a detached goroutine after a
// short delay so the HTTP response has time to reach the client before
// the kernel tears everything down. Restart-network runs
// /etc/init.d/network restart and returns the command output; it's
// less destructive (5-15 s LAN blip) but still wipes every in-flight
// TCP session.
//
// Both are opt-in from the dashboard (red-bordered double confirmation)
// because they interrupt the very channel the user is connecting over.

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Overridable so tests don't actually reboot or reload the host
// running `go test`.
var (
	rebootBinary   = "/sbin/reboot"
	netRestartArgv = []string{"/etc/init.d/network", "restart"}
)

func (s *Server) handleReboot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.writeAuth(r) {
		writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
		return
	}
	if _, err := exec.LookPath(rebootBinary); err != nil {
		writeJSONErr(w, http.StatusServiceUnavailable,
			"reboot binary not available: "+err.Error())
		return
	}
	// Schedule the reboot a moment in the future so this response can
	// flush cleanly. Detach from the request context; init will kill us
	// anyway once reboot(8) starts.
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = exec.Command(rebootBinary).Run()
	}()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"message": "router rebooting in ~0.5s; will be unreachable for 30-60s",
	})
}

func (s *Server) handleRestartNetwork(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.writeAuth(r) {
		writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
		return
	}
	if len(netRestartArgv) == 0 {
		writeJSONErr(w, http.StatusServiceUnavailable, "restart command not configured")
		return
	}
	if _, err := exec.LookPath(netRestartArgv[0]); err != nil {
		writeJSONErr(w, http.StatusServiceUnavailable,
			netRestartArgv[0]+" not available: "+err.Error())
		return
	}
	// Detach from the request context: /etc/init.d/network restart takes
	// down the very interface this HTTP connection rides on, so the
	// client will lose the response mid-flight. Fire-and-forget with a
	// 20s ceiling; the UI shows a generic "已下发" toast either way.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, netRestartArgv[0], netRestartArgv[1:]...).Run()
	}()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"message": "network restart dispatched: " + strings.Join(netRestartArgv, " "),
	})
}
