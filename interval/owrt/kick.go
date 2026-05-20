// kick.go — POST /api/devices/kick handler.
//
// Disconnects a single WiFi station so the client has to re-associate
// (and, depending on platform, fall back to mobile data or cycle Wi-Fi).
//
// Kick chain (best-effort, multiple commands may fire):
//
//   1. `ubus call ahsapd.roaming staDisconnect` — vendor band-steering
//      hint. Exits 0 on most MTK firmwares but is ONLY a hint, not a
//      real deauth. We still call it because on some images it actually
//      works, and on the rest it's a no-op (exit 0, no error).
//
//   2. `iwpriv <ra*|rax*> set DisConnectSta=<MAC>` — MTK proprietary
//      kick. Iterates every active ra*/rax* VAP because we don't know
//      which band the station is on. Verified working on MTK7981
//      (clife, OpenWrt 21.02-SNAPSHOT vendor build): produces "无线断开"
//      + "MAC表移除" syslog within ~1s.
//
//   3. (optional, restart_wifi=true) `wifi reload` / `/etc/init.d/ahsapd
//      restart` — nuclear, drops every client on every radio for a few
//      seconds. Only as a last resort when none of the surgical paths
//      moved the needle.
//
// Wired devices are rejected up-front: deauth has no analogue on
// Ethernet, and shutting down a switch port for a single MAC isn't
// something we want to do casually from the dashboard.

package owrt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// KickStation runs the per-station deauth chain (ahsapd hint + iwpriv
// fan-out) and, when restartWiFi is true, the nuclear wifi-restart
// chain too. Returns a small report describing what fired so the UI
// can echo it back.
func KickStation(ctx context.Context, mac string, restartWiFi bool) KickReport {
	var rep KickReport

	// 1. Vendor band-steering hint (no-op on most firmwares but cheap to try).
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
			rep.Kicked = argv[0] + " " + argv[1]
			break
		}
	}

	// 2. MTK iwpriv DisConnectSta — the actual deauth on Ralink/MediaTek
	//    vendor firmwares. We don't know which VAP the client is on,
	//    so we fan out across every active ra*/rax* interface. Each call
	//    is fast (~50ms) and harmless if the station isn't on that VAP.
	if _, err := exec.LookPath("iwpriv"); err == nil {
		for _, iface := range listMTKWiFiVAPs() {
			ctxI, cancel := context.WithTimeout(ctx, 2*time.Second)
			cmd := exec.CommandContext(ctxI, "iwpriv", iface, "set", "DisConnectSta="+mac)
			_, err := cmd.CombinedOutput()
			cancel()
			if err == nil {
				if rep.IwprivKicked == "" {
					rep.IwprivKicked = iface
				} else {
					rep.IwprivKicked += "," + iface
				}
			}
		}
	}

	// 3. Optional wifi-restart (everyone disconnects briefly).
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

// listMTKWiFiVAPs returns names of /sys/class/net interfaces that
// match Ralink/MediaTek WiFi VAP naming (ra* / rax*) AND have a
// non-zero MAC (zero MAC = inactive / unconfigured VAP).
//
// Stable order (sort.Strings) so log output is reproducible.
func listMTKWiFiVAPs() []string {
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return nil
	}
	var ifaces []string
	for _, e := range entries {
		name := e.Name()
		if !(strings.HasPrefix(name, "ra") && len(name) >= 3) {
			continue
		}
		// "ra0", "ra1", ... or "rax0", "rax1", ...
		// Filter out zero-MAC entries (inactive VAPs).
		addr, err := os.ReadFile(filepath.Join("/sys/class/net", name, "address"))
		if err != nil {
			continue
		}
		mac := strings.TrimSpace(string(addr))
		if mac == "" || mac == "00:00:00:00:00:00" {
			continue
		}
		ifaces = append(ifaces, name)
	}
	sort.Strings(ifaces)
	return ifaces
}

type KickReport struct {
	// Kicked is whichever staKickCmds entry succeeded (vendor hint).
	// Often present even when the station didn't actually drop —
	// IwprivKicked is the more authoritative "really deauth'd" signal.
	Kicked        string `json:"kicked,omitempty"`
	IwprivKicked  string `json:"iwpriv_kicked,omitempty"` // comma-separated VAPs that accepted DisConnectSta
	WiFiRestarted string `json:"wifi_restarted,omitempty"`
}

// handleDeviceKick force-disconnects a single WiFi station.
//
//	POST /api/devices/kick
//	  { "mac": "AA:BB:CC:DD:EE:FF",
//	    "restart_wifi": false }    // optional nuclear fallback
//
//	→ 200 {"ok":true,"kicked":"...","iwpriv_kicked":"rax0","wifi_restarted":""}
//	  Empty `kicked` + `iwpriv_kicked` means no command was available
//	  on this host — surfaced to the user as "踢下线指令已尝试, 但未
//	  找到可用方法".
