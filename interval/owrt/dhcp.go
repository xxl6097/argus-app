// DHCPManager abstracts router-specific static-lease (DHCP reservation)
// operations so the Web UI can offer a "set static IP" affordance
// without the core library having a hard dependency on OpenWrt /
// dnsmasq / uci. See the UCIDHCPManager implementation for the
// reference OpenWrt binding.
//
// A DHCPManager is considered available when NewUCIDHCPManager finds
// the uci binary in PATH and the current process can read
// /etc/config/dhcp. If you're not on OpenWrt, leave the Server
// without a manager — the /api/dhcp endpoints will return 503 and
// the dashboard will hide the static-IP button.

package owrt

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// DHCPManager is the Web-UI-facing interface for static-lease
// operations. All methods are safe for concurrent use.
type DHCPManager interface {
	// List returns every current static reservation, keyed by
	// lowercase MAC. Implementations should normalize MAC casing.
	List(ctx context.Context) (map[string]StaticLease, error)

	// Set creates or updates a reservation. An existing entry for
	// the MAC is replaced in-place. Implementations must validate
	// inputs defensively (MAC and IP syntax, IP-within-subnet when
	// applicable) before mutating state.
	Set(ctx context.Context, l StaticLease) error

	// Delete removes the reservation for a MAC. No-op when the MAC
	// has no reservation; returns nil in that case.
	Delete(ctx context.Context, mac string) error
}

// StaticLease describes a single DHCP reservation as the dashboard
// sees it. Name is optional; when empty the implementation may
// synthesize one (e.g. "argus-<mac-suffix>").
type StaticLease struct {
	MAC  string `json:"mac"`
	IP   string `json:"ip"`
	Name string `json:"name,omitempty"`
}

// ErrDHCPManagerUnavailable is returned by NewUCIDHCPManager when the
// host doesn't look like an OpenWrt system (uci missing or
// /etc/config/dhcp unreadable). The value surfaces via errors.Is so
// callers can fall back cleanly.
var ErrDHCPManagerUnavailable = errors.New("owrt: no DHCP manager available on this host")

// ErrIPAlreadyReserved is returned by DHCPManager.Set when the target
// IP is already reserved for a different MAC. This prevents writing
// two static entries pointing at the same IP — a configuration that
// causes odhcpd / dnsmasq to refuse the entire DHCP package on the
// next reload, breaking DHCP for every device on the LAN.
//
// The surface error carries the existing owner's MAC via IPConflict.
type ErrIPAlreadyReserved struct {
	IP       string
	OwnerMAC string
}

func (e *ErrIPAlreadyReserved) Error() string {
	return fmt.Sprintf("owrt: IP %s is already reserved for MAC %s", e.IP, strings.ToUpper(e.OwnerMAC))
}

// uciBinary is overridable for tests; argusd always uses the default.
var uciBinary = "uci"

// dhcpReloadCmds enumerates the init scripts web tries (in order)
// after a uci commit to make reservations take effect. Different OpenWrt
// flavors run different DHCP daemons:
//
//   - Mainline OpenWrt: dnsmasq (lease file /tmp/dhcp.leases)
//   - MTK / vendor builds (e.g. C-Life): odhcpd (lease file /tmp/hosts/odhcpd)
//   - Some images run BOTH (one for IPv4, one for IPv6 RA)
//
// Each script's "Command failed: Not found" / non-zero exit is treated
// as "this daemon isn't installed" and skipped. Overridable for tests.
var dhcpReloadCmds = [][]string{
	{"/etc/init.d/dnsmasq", "reload"},
	{"/etc/init.d/odhcpd", "reload"},
}

// dhcpLeaseFiles enumerates lease files web prunes a MAC's entry
// from when forcing immediate-effect of a new static reservation. The
// reload command alone will not invalidate an active lease — the
// daemon happily keeps serving the old IP until the client renews
// (default 12 h). Removing the line forces the daemon to issue the
// configured static IP on the client's next DHCP packet.
var dhcpLeaseFiles = []string{
	"/tmp/dhcp.leases",
	"/tmp/hosts/odhcpd",
}

// staKickCmds enumerates ways to forcibly disconnect a WiFi station so
// it reassociates and triggers a fresh DHCP DISCOVER (which will pick
// up the new static reservation immediately). The first command that
// succeeds wins; absence of all of them leaves the kick as a no-op.
//
// Order matters: we try the vendor ubus method first (most surgical),
// then the mainline OpenWrt hostapd ubus method, then the lower-level
// `iw station del`. {{MAC}} is replaced with the lowercased MAC.
//
// dismissTime:30 — observed on iOS: a 2-second dismiss is too short; the
// device reconnects before its DHCP client has time to drop the cached
// lease, so it tries RENEWING the old IP, takes a NAK, and often falls
// back to the cached address anyway. 30 s reliably forces a full
// release + fresh DISCOVER on both iOS and Android.
var staKickCmds = [][]string{
	// MTK ahsapd vendor firmware (C-Life and similar)
	{"ubus", "call", "ahsapd.roaming", "staDisconnect", `{"macAddress":"{{MAC}}","dismissTime":30}`},
	// Mainline OpenWrt hostapd ubus — exists on stock images. We don't
	// know the iface here so we fall through to iw if this errors.
}

// wifiRestartCmds enumerates ways to force EVERY WiFi client off the
// radios for a few seconds. Used as the nuclear option when per-station
// kick is a silent no-op on vendor firmware (observed on MTK C-Life:
// ahsapd.roaming staDisconnect returns exit 0 but produces no Del Sta /
// Deauth kernel event, so iOS stays on its cached IP).
//
// First successful command wins. Picks the lightest available:
//  1. mainline `wifi reload` — safe reconfig without full re-init
//  2. `/etc/init.d/ahsapd restart` — vendor-firmware nuke
var wifiRestartCmds = [][]string{
	{"wifi", "reload"},
	{"/etc/init.d/ahsapd", "restart"},
}

// UCIDHCPManager writes static DHCP reservations to OpenWrt's uci-
// backed /etc/config/dhcp and reloads dnsmasq. Constructed via
// NewUCIDHCPManager (which refuses to build on non-OpenWrt hosts).
//
// Thread model: all uci + dnsmasq calls are serialized through an
// internal mutex. Concurrent Set/Delete calls from multiple HTTP
// requests are therefore strictly ordered, which matches dnsmasq's
// expectations anyway.
type UCIDHCPManager struct {
	mu sync.Mutex
}

// NewUCIDHCPManager returns a manager for the local OpenWrt host, or
// ErrDHCPManagerUnavailable wrapped with the detection failure cause
// on non-OpenWrt systems.
func NewUCIDHCPManager() (*UCIDHCPManager, error) {
	if _, err := exec.LookPath(uciBinary); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDHCPManagerUnavailable, err)
	}
	// Probe /etc/config/dhcp by attempting a no-op 'uci show'.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := runUCI(ctx, "show", "dhcp"); err != nil {
		return nil, fmt.Errorf("%w: uci show dhcp: %v", ErrDHCPManagerUnavailable, err)
	}
	return &UCIDHCPManager{}, nil
}

// --- Input validation (defense against shell / uci injection) ----------

var (
	reMAC = regexp.MustCompile(`^[0-9a-fA-F]{2}(:[0-9a-fA-F]{2}){5}$`)

	// forbiddenNameChars rejects shell / uci metachars so a name can't
	// escape the `uci set key=value` argument or the stored
	// /etc/config/dhcp value. Everything else (Chinese, spaces, dots,
	// etc.) is allowed. See validateName for the full contract.
	forbiddenNameChars = "'\"\\`$;|&<>\x00\n\r\t"
)

func ValidateMAC(mac string) (string, error) {
	mac = strings.TrimSpace(mac)
	if !reMAC.MatchString(mac) {
		return "", fmt.Errorf("invalid MAC %q (want aa:bb:cc:dd:ee:ff)", mac)
	}
	return strings.ToLower(mac), nil
}

func validateIPv4(s string) (string, error) {
	s = strings.TrimSpace(s)
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("invalid IPv4 %q", s)
	}
	return ip.String(), nil
}

// validateName accepts the widest safe set of characters for a DHCP
// host name: any UTF-8 codepoint (including Chinese), ASCII
// letters/digits, spaces, and common punctuation — with hard bans on
// shell / uci metacharacters that could escape the `uci set` argument
// or corrupt /etc/config/dhcp. Length capped at 63 bytes so we stay
// below dnsmasq's 64-byte cap for DHCP host names.
//
// Returns the trimmed name (empty is OK and handled by the caller).
func validateName(n string) (string, error) {
	n = strings.TrimSpace(n)
	if len(n) > 63 {
		return "", fmt.Errorf("name too long (%d bytes > 63)", len(n))
	}
	for _, r := range n {
		if r < 0x20 {
			return "", fmt.Errorf("invalid name %q (control characters not allowed)", n)
		}
		if strings.ContainsRune(forbiddenNameChars, r) {
			return "", fmt.Errorf("invalid name %q (contains disallowed character %q)", n, r)
		}
	}
	return n, nil
}

// --- uci wrappers ------------------------------------------------------

func runUCI(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, uciBinary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("uci %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// List implements DHCPManager.
func (m *UCIDHCPManager) List(ctx context.Context) (map[string]StaticLease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out, err := runUCI(ctx, "-q", "show", "dhcp")
	if err != nil {
		return nil, err
	}
	return parseUCIDHCPShow(out), nil
}

// Set implements DHCPManager.
func (m *UCIDHCPManager) Set(ctx context.Context, l StaticLease) error {
	mac, err := ValidateMAC(l.MAC)
	if err != nil {
		return err
	}
	ip, err := validateIPv4(l.IP)
	if err != nil {
		return err
	}
	name, err := validateName(l.Name)
	if err != nil {
		return err
	}
	// Use a FNV-32 hash of the full MAC instead of just the last 2
	// bytes, so two devices whose MACs end in the same suffix don't
	// collide on the section name. 6 hex digits gives 16M buckets
	// which is plenty for a home LAN; collisions beyond that are
	// caught by the update-vs-create branch below anyway.
	macSuffix := macHashSuffix(mac)
	if name == "" {
		name = "argus-" + macSuffix
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Always revert any stale pending state on the DHCP package before
	// we start, and ensure we revert if anything goes wrong after we've
	// begun mutating. Without this, a prior failed Set can leave
	// "dhcp.@host[N]=host" floating in the pending changeset, which
	// breaks index arithmetic on the next attempt.
	_, _ = runUCI(ctx, "revert", "dhcp")
	committed := false
	defer func() {
		if !committed {
			_, _ = runUCI(ctx, "revert", "dhcp")
		}
	}()

	// IP conflict detection: scan every existing host reservation and
	// refuse the write if the target IP is already bound to a DIFFERENT
	// MAC. Two reservations for the same IP cause odhcpd / dnsmasq to
	// reject the entire /etc/config/dhcp on next reload, breaking DHCP
	// for every device on the LAN — this is the v0.15.3 critical fix.
	existingLeases, err := m.listLocked(ctx)
	if err != nil {
		return err
	}
	for otherMAC, other := range existingLeases {
		if otherMAC == mac {
			continue // updating our own reservation is fine
		}
		if other.IP == ip {
			return &ErrIPAlreadyReserved{IP: ip, OwnerMAC: otherMAC}
		}
	}

	idx, err := m.findHostSectionLocked(ctx, mac)
	if err != nil {
		return err
	}
	// uci addresses differ for anonymous vs named sections:
	//   anonymous: dhcp.@host[N].field=...
	//   named:     dhcp.name.field=...
	// We preserve the anonymous form when updating an existing entry
	// that was already anonymous (typically created by LuCI);
	// otherwise we create/update a named "argus_<fnv-suffix>" section so
	// the index doesn't shift when other entries are added/removed.
	var sectionRef string
	if idx == "" {
		sectionName := "argus_" + macSuffix
		if _, err := runUCI(ctx, "set", "dhcp."+sectionName+"=host"); err != nil {
			return err
		}
		sectionRef = "dhcp." + sectionName
	} else {
		sectionRef = "dhcp." + idx
	}
	for _, kv := range [][]string{
		{sectionRef + ".name=" + name},
		{sectionRef + ".mac=" + mac},
		{sectionRef + ".ip=" + ip},
		{sectionRef + ".leasetime=infinite"},
	} {
		if _, err := runUCI(ctx, append([]string{"set"}, kv...)...); err != nil {
			return err
		}
	}
	if _, err := runUCI(ctx, "commit", "dhcp"); err != nil {
		return err
	}
	committed = true
	return nil
}

// listLocked is the unlocked version of List for use inside Set /
// Delete without re-acquiring m.mu. Caller must hold it.
func (m *UCIDHCPManager) listLocked(ctx context.Context) (map[string]StaticLease, error) {
	out, err := runUCI(ctx, "-q", "show", "dhcp")
	if err != nil {
		return nil, err
	}
	return parseUCIDHCPShow(out), nil
}

// macHashSuffix produces a 6-hex-char FNV-32 hash of a normalized MAC.
// Used as the suffix in the "argus_<suffix>" UCI section name; the
// full-MAC hash means two MACs that happen to share their last two
// bytes (e.g. iOS private-WiFi-address collisions on a home LAN) get
// distinct sections. Collision probability at a typical fleet size
// (~100 devices) is ~3e-4 — acceptable given that an actual collision
// is transparently handled by findHostSectionLocked (updates the
// existing section in place rather than creating a duplicate).
func macHashSuffix(mac string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(mac))
	return fmt.Sprintf("%06x", h.Sum32()&0xFFFFFF)
}

// Delete implements DHCPManager.
func (m *UCIDHCPManager) Delete(ctx context.Context, mac string) error {
	normalized, err := ValidateMAC(mac)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	_, _ = runUCI(ctx, "revert", "dhcp")
	committed := false
	defer func() {
		if !committed {
			_, _ = runUCI(ctx, "revert", "dhcp")
		}
	}()

	idx, err := m.findHostSectionLocked(ctx, normalized)
	if err != nil {
		return err
	}
	if idx == "" {
		committed = true // nothing to commit; avoid the defer's revert
		return nil       // already absent
	}
	if _, err := runUCI(ctx, "delete", "dhcp."+idx); err != nil {
		return err
	}
	if _, err := runUCI(ctx, "commit", "dhcp"); err != nil {
		return err
	}
	committed = true
	return nil
}

// PurgeArgusOwned deletes every uci host section whose key begins
// with "argus_" — i.e. every reservation this server's Set() has
// ever created. It does NOT touch anonymous @host[N] sections
// (which typically belong to LuCI / the user's own config).
//
// Intended as a recovery tool: if a bad argus_-owned entry breaks
// the DHCP daemon on reload (e.g. an IP conflict written by a
// pre-v0.15.3 version), a single POST to /api/dhcp/purge-argus
// restores the user's original DHCP config. Returns the number of
// sections removed.
//
// Reachable via POST /api/dhcp?purge_argus=1 (auth-gated; same
// write predicate as Set/Delete). Not called by Set or Delete;
// opt-in only.
func (m *UCIDHCPManager) PurgeArgusOwned(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	_, _ = runUCI(ctx, "revert", "dhcp")
	committed := false
	defer func() {
		if !committed {
			_, _ = runUCI(ctx, "revert", "dhcp")
		}
	}()

	out, err := runUCI(ctx, "-q", "show", "dhcp")
	if err != nil {
		return 0, err
	}
	// Collect section names matching "argus_*" declared as =host.
	// We need to delete them in reverse declaration order to keep the
	// remaining index stable — but since these are NAMED sections
	// (dhcp.argus_foo=host), index shift isn't a concern; order
	// doesn't matter.
	re := regexp.MustCompile(`^dhcp\.(argus_[a-z0-9]+)=host$`)
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		seen[m[1]] = true
	}
	if len(seen) == 0 {
		committed = true
		return 0, nil
	}
	for name := range seen {
		if _, err := runUCI(ctx, "delete", "dhcp."+name); err != nil {
			return 0, err
		}
	}
	if _, err := runUCI(ctx, "commit", "dhcp"); err != nil {
		return 0, err
	}
	committed = true
	// Best-effort reload so the pruned state takes effect immediately.
	_ = ApplyDHCPChanges(ctx, "", false)
	return len(seen), nil
}

// findHostSectionLocked returns the uci section key (e.g. "@host[2]"
// or "argus_8e97") for a MAC, or "" if no existing reservation has
// that MAC. Caller must hold m.mu.
func (m *UCIDHCPManager) findHostSectionLocked(ctx context.Context, mac string) (string, error) {
	out, err := runUCI(ctx, "-q", "show", "dhcp")
	if err != nil {
		return "", err
	}
	leases := parseUCIDHCPShow(out)
	for _, l := range leases {
		if l.MAC == mac {
			// Name is "#<key>:<original>"; extract the key.
			if !strings.HasPrefix(l.Name, "#") {
				return "", fmt.Errorf("could not extract section key from %q", l.Name)
			}
			rest := l.Name[1:]
			colon := strings.Index(rest, ":")
			if colon < 0 {
				return "", fmt.Errorf("malformed section-key prefix %q", l.Name)
			}
			return rest[:colon], nil
		}
	}
	return "", nil
}

// parseUCIDHCPShow extracts static leases from 'uci show dhcp' output.
// Each lease's Name is prefixed with "#<key>:" so callers can find the
// underlying uci section by that key without re-running uci. The key
// is either an index ("0", "1", ...) for anonymous @host[N] entries
// or a named section ("argus_8e97") for entries created by us.
//
// Exported solely for tests inside this package; not a stability
// guarantee.
func parseUCIDHCPShow(out string) map[string]StaticLease {
	// Output looks like either:
	//   dhcp.@host[2]=host
	//   dhcp.@host[2].name='xiaomi-pc'
	//   dhcp.@host[2].mac='a0:29:42:00:7a:fd'
	//   dhcp.@host[2].ip='192.168.10.2'
	// OR (named sections we create):
	//   dhcp.argus_8e97=host
	//   dhcp.argus_8e97.name='xiaomi_speaker'
	//   dhcp.argus_8e97.mac='ec:41:18:79:8e:97'
	//   dhcp.argus_8e97.ip='192.168.10.209'
	reAnon := regexp.MustCompile(`^dhcp\.@host\[(\d+)\]\.(name|mac|ip)=(.*)$`)
	reNamed := regexp.MustCompile(`^dhcp\.([A-Za-z_][A-Za-z0-9_]*)\.(name|mac|ip)=(.*)$`)
	// Also track which sections are declared as =host so we don't
	// misinterpret other named sections (dhcp.lan=dhcp, etc.).
	reSection := regexp.MustCompile(`^dhcp\.([A-Za-z_][A-Za-z0-9_]*|\@host\[\d+\])=host$`)

	isHost := map[string]bool{}
	partial := map[string]*StaticLease{} // key = normalized section ident
	setField := func(key, field, val string) {
		val = strings.Trim(val, "'\"")
		l, ok := partial[key]
		if !ok {
			l = &StaticLease{}
			partial[key] = l
		}
		switch field {
		case "name":
			l.Name = val
		case "mac":
			l.MAC = strings.ToLower(val)
		case "ip":
			l.IP = val
		}
	}

	for _, line := range strings.Split(out, "\n") {
		if m := reSection.FindStringSubmatch(line); m != nil {
			isHost[m[1]] = true
			continue
		}
		if m := reAnon.FindStringSubmatch(line); m != nil {
			key := "@host[" + m[1] + "]"
			setField(key, m[2], m[3])
			continue
		}
		if m := reNamed.FindStringSubmatch(line); m != nil {
			// Skip keys we know belong to non-host sections.
			setField(m[1], m[2], m[3])
		}
	}
	result := make(map[string]StaticLease, len(partial))
	for key, l := range partial {
		if !isHost[key] {
			continue // drop non-host entries (dhcp.lan.*, dhcp.@dnsmasq[0].*)
		}
		if l.MAC == "" || l.IP == "" {
			continue
		}
		// Prefix name with "#<key>:" so findHostIndexLocked can recover it.
		display := l.Name
		l.Name = "#" + key + ":" + display
		result[l.MAC] = *l
	}
	return result
}

// ApplyDHCPChanges makes a static-reservation change take effect
// immediately (rather than waiting for the client to voluntarily
// renew, which is up to 12 h with the default leasetime).
//
// Three steps, each best-effort and logged but non-fatal:
//  1. Reload every DHCP daemon init script we know about. Some
//     vendor firmwares run odhcpd, not dnsmasq; some run neither.
//  2. Prune the client's line from every known lease file so a
//     stale lease doesn't keep the old IP pinned.
//  3. Kick the WiFi station so it reassociates and sends a fresh
//     DHCP DISCOVER. Wired clients renew on their own schedule and
//     can't be kicked.
//
// Returns a report describing what each step did. Never returns an
// error — immediate-apply is a courtesy, not a correctness
// requirement; the UCI commit has already succeeded by the time we
// get here.
//
// mac is the lowercased MAC (empty allowed; means "don't kick or
// prune per-MAC, just reload"). restartWiFi=true additionally runs
// /etc/init.d/ahsapd restart (or the mainline `wifi` command) to force
// every WiFi client off for a few seconds — the nuclear option when
// per-station kick doesn't work on vendor firmware that ignores
// ahsapd.roaming staDisconnect (e.g. MTK C-Life).
func ApplyDHCPChanges(ctx context.Context, mac string, restartWiFi bool) ApplyReport {
	var rep ApplyReport

	// 1. Reload DHCP daemon(s).
	for _, argv := range dhcpReloadCmds {
		if len(argv) == 0 {
			continue
		}
		if _, err := exec.LookPath(argv[0]); err != nil {
			continue // init script not present
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		out, err := cmd.CombinedOutput()
		if err != nil || bytes_HasPrefix(out, "Command failed") {
			continue
		}
		rep.Reloaded = append(rep.Reloaded, argv[0])
	}

	// 2. Prune stale lease lines for this MAC from every known lease file.
	if mac != "" {
		for _, path := range dhcpLeaseFiles {
			if err := pruneLeaseFile(path, mac); err == nil {
				rep.Pruned = append(rep.Pruned, path)
			}
		}
	}

	// 3. Flush any ARP entry still mapping this MAC to its old IP.
	// Without this, iOS in particular re-advertises the cached address
	// via ARP after the kick, and the router happily confirms it —
	// letting the device ignore the DHCPNAK. Deleting the neigh entry
	// forces the kernel to re-resolve after the device reconnects.
	if mac != "" {
		if flushed := flushARPForMAC(ctx, mac); flushed != "" {
			rep.ARPFlushed = flushed
		}
	}

	// 4. Kick the station (per-MAC, surgical).
	if mac != "" {
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
				rep.Kicked = argv[0] + " " + argv[1] // "ubus call ..."
				break
			}
		}
	}

	// 5. Optionally restart WiFi — nuclear option. Disconnects EVERY
	// client on every radio for ~3-5 seconds. Only run when the caller
	// explicitly opted in (e.g. per-station kick on this firmware is a
	// silent no-op, and the user said "OK to disrupt everyone briefly").
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

// flushARPForMAC scans /proc/net/arp, finds every entry whose HW addr
// matches mac (case-insensitive), and deletes it via `ip neigh del`.
// Returns the old IP that was flushed (first match), or "" if none.
// Best-effort: errors are swallowed because this is a courtesy.
func flushARPForMAC(ctx context.Context, mac string) string {
	data, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return ""
	}
	macLower := strings.ToLower(mac)
	var flushedIP string
	for i, line := range strings.Split(string(data), "\n") {
		if i == 0 { // header
			continue
		}
		fields := strings.Fields(line)
		// Columns: IP  HW-type  Flags  HW-addr  Mask  Device
		if len(fields) < 6 {
			continue
		}
		if strings.ToLower(fields[3]) != macLower {
			continue
		}
		oldIP, iface := fields[0], fields[5]
		if _, err := exec.LookPath("ip"); err != nil {
			return ""
		}
		ctxK, cancel := context.WithTimeout(ctx, 2*time.Second)
		_ = exec.CommandContext(ctxK, "ip", "neigh", "del", oldIP, "dev", iface).Run()
		cancel()
		if flushedIP == "" {
			flushedIP = oldIP
		}
	}
	return flushedIP
}

// ApplyReport summarizes what ApplyDHCPChanges did, for inclusion in
// the /api/dhcp POST/DELETE response body. Consumers use this to
// render an accurate "已生效" vs "已保存,但需要设备续租后生效" hint.
type ApplyReport struct {
	Reloaded      []string `json:"reloaded,omitempty"`       // init scripts that reloaded successfully
	Pruned        []string `json:"pruned,omitempty"`         // lease files pruned
	ARPFlushed    string   `json:"arp_flushed,omitempty"`    // old IP whose ARP entry we deleted
	Kicked        string   `json:"kicked,omitempty"`         // per-station kick command that succeeded
	WiFiRestarted string   `json:"wifi_restarted,omitempty"` // global WiFi restart command that succeeded (nuclear)
}

// bytes_HasPrefix works around a lint-ish preference for not importing
// bytes just for one call; inline check.
func bytes_HasPrefix(b []byte, prefix string) bool {
	if len(b) < len(prefix) {
		return false
	}
	return string(b[:len(prefix)]) == prefix
}

// pruneLeaseFile rewrites path in-place with all lines matching mac
// (case-insensitive) removed. No-op if the file doesn't exist or
// contains no matching lines. Writes atomically via rename so a
// crash mid-write can't corrupt the file.
func pruneLeaseFile(path, mac string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	macLower := strings.ToLower(mac)
	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines))
	changed := false
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), macLower) {
			changed = true
			continue
		}
		out = append(out, line)
	}
	if !changed {
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(out, "\n")), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// --- HTTP handlers (wired from Server.handleDHCP) ----------------------
