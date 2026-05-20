// handlers_system.go — /api/system/{reboot,restart-network}.
package web

import (
	"encoding/json"
	"net/http"

	"context"
	"github.com/xxl6097/argus-app/interval/owrt"
	"os/exec"
	"strings"
	"time"
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
	if _, err := exec.LookPath(owrt.RebootBinary); err != nil {
		writeJSONErr(w, http.StatusServiceUnavailable,
			"reboot binary not available: "+err.Error())
		return
	}
	// Schedule the reboot a moment in the future so this response can
	// flush cleanly. Detach from the request context; init will kill us
	// anyway once reboot(8) starts.
	go func() {
		time.Sleep(500 * time.Millisecond)
		_ = exec.Command(owrt.RebootBinary).Run()
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
	if len(owrt.NetRestartArgv) == 0 {
		writeJSONErr(w, http.StatusServiceUnavailable, "restart command not configured")
		return
	}
	if _, err := exec.LookPath(owrt.NetRestartArgv[0]); err != nil {
		writeJSONErr(w, http.StatusServiceUnavailable,
			owrt.NetRestartArgv[0]+" not available: "+err.Error())
		return
	}
	// Detach from the request context: /etc/init.d/network restart takes
	// down the very interface this HTTP connection rides on, so the
	// client will lose the response mid-flight. Fire-and-forget with a
	// 20s ceiling; the UI shows a generic "已下发" toast either way.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, owrt.NetRestartArgv[0], owrt.NetRestartArgv[1:]...).Run()
	}()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"message": "network restart dispatched: " + strings.Join(owrt.NetRestartArgv, " "),
	})
}
