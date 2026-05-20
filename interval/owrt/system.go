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

package owrt

import ()

// Overridable so tests don't actually reboot or reload the host
// running `go test`.
var (
	RebootBinary   = "/sbin/reboot"
	NetRestartArgv = []string{"/etc/init.d/network", "restart"}
)
