// Package web exposes a built-in HTTP + Server-Sent Events (SSE)
// dashboard on top of an argus.Watcher.
//
// It is intentionally zero-dependency: the rendering is a single
// embedded HTML file with vanilla JS + EventSource; no build step, no
// framework, no external CDN. The handler set is three endpoints:
//
//   - GET /             — embedded dashboard page
//   - GET /api/devices  — JSON snapshot (online devices from Watcher.Known
//     merged with recently-offline devices cached here)
//   - GET /api/events   — SSE stream of Online/Offline/Change events
//
// The package is opt-in (no code in the core library changes); typical
// wiring in argusd:
//
//	srv := web.NewServer(w)
//	http.ListenAndServe("127.0.0.1:9099", srv)
//
// Scope (intentional non-goals for v0.13.0):
//   - No authentication. Bind to 127.0.0.1 or put a reverse proxy in front.
//   - No write API. The dashboard is read-only; Argus's Config is
//     reloaded via SIGHUP on the host process, not via HTTP.
//   - No HTTPS. Terminate TLS at the reverse proxy if needed.
//
// # Chinese · 中文
//
// web 子包提供基于 HTTP + SSE 的只读仪表板, 零依赖, 单文件 HTML,
// 直接嵌入二进制。默认监听 127.0.0.1:9099, 不带鉴权 (如需外网访问请
// 在反向代理层加认证)。
package web

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	argus "github.com/xxl6097/argusd"
)

//go:embed assets/dashboard.html
var dashboardHTML []byte

//go:embed assets/favicon.ico
var faviconICO []byte

//go:embed assets/login.html
var loginHTML []byte

// dashboardETag is computed once at process start from the embedded
// HTML. It changes between releases (because the file content changes)
// but is stable within a single binary's lifetime, so browsers cache
// the page across navigations and only re-download after a redeploy.
var dashboardETag = computeETag(dashboardHTML)

func computeETag(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// Defaults for the offline device cache. Override via Option at
// construction time (see NewServer).
const (
	defaultOfflineRetention = 7 * 24 * time.Hour
	defaultOfflineMax       = 512
)

// Option configures a Server at construction time.
type Option func(*Server)

// WithDataDir tells the server which directory holds the JSON stores
// + history/ subdir, for the /api/backup/export and /api/backup/import
// endpoints. When empty, those endpoints return 503. The default per-
// store paths used by NewServer's callers all live inside this dir;
// the server treats it as authoritative for "pack everything up".
func WithDataDir(dir string) Option {
	return func(s *Server) { s.dataDir = dir }
}

// WithOfflineRetention sets how long a device remains in the /api/devices
// response after it goes offline. Zero or negative disables retention
// entirely (offline devices drop out of the list immediately on
// EventOffline). Default: 7 days.
func WithOfflineRetention(d time.Duration) Option {
	return func(s *Server) { s.offlineTTL = d }
}

// WithOfflineMax caps the number of offline devices retained in the
// dashboard cache. When the cap is reached, the oldest entry (by offline
// timestamp) is evicted to make room. Zero or negative disables the cap.
// Default: 512.
func WithOfflineMax(n int) Option {
	return func(s *Server) { s.offlineMax = n }
}

// WithAliases attaches a persistent alias store. When set, `/api/devices`
// rows carry an `alias` field (when present) and the dashboard prefers
// the alias over hostname for display. Writes go through
// `POST /api/aliases`, which is gated by the auth predicate (see
// [WithWriteAuth]).
//
// Passing nil is a no-op (equivalent to the default). The store itself
// is safe for concurrent use.
func WithAliases(store *AliasStore) Option {
	return func(s *Server) { s.aliases = store }
}

// AuthCheck decides whether an incoming HTTP request may mutate state
// (currently: the alias write APIs). Return true to allow, false to
// reject with 403. See [WithWriteAuth] for the default policy.
type AuthCheck func(r *http.Request) bool

// WithWriteAuth replaces the default write-API auth predicate. The
// default allows requests whose remote address is loopback or an
// RFC1918 private-network address — appropriate for a Web UI exposed
// on a home LAN via `-listen=0.0.0.0:9099`. For more restrictive
// deployments (reverse proxy with shared-secret header, Basic Auth,
// etc.), provide a custom AuthCheck.
//
// The predicate is NOT applied to read endpoints (`GET /`, `GET
// /api/devices`, `GET /api/events`, `GET /api/aliases`).
func WithWriteAuth(check AuthCheck) Option {
	return func(s *Server) { s.writeAuth = check }
}

// WithDHCPManager attaches a router-specific DHCP manager (typically
// a *UCIDHCPManager produced by NewUCIDHCPManager). When set,
// `/api/dhcp` is enabled and the dashboard surfaces a "set static IP"
// button on every device row. Passing nil is a no-op (equivalent to
// the default — the endpoint returns 503 and the dashboard hides
// the button).
func WithDHCPManager(m DHCPManager) Option {
	return func(s *Server) { s.dhcp = m }
}

// WithHistory attaches a per-MAC online/offline history store. When
// set, /api/history and /api/worktime are enabled and OnEvent records
// Online/Offline transitions. nil is the default (feature disabled).
func WithHistory(h *HistoryStore) Option {
	return func(s *Server) { s.history = h }
}

// WithSettings attaches a persistent settings store (worktime window,
// "me" MAC). Enables /api/settings when non-nil.
func WithSettings(st *SettingsStore) Option {
	return func(s *Server) { s.settings = st }
}

// WithOverrides attaches a per-(mac, date) manual in/out override
// store. Used for worktime days where the Watcher missed transitions.
// Enables /api/worktime/override when non-nil.
func WithOverrides(o *OverrideStore) Option {
	return func(s *Server) { s.overrides = o }
}

// WithHolidays attaches a legal-holiday / 调休-workday store so the
// worktime compute can treat weekends and public holidays as
// overtime days. When nil, only the weekday-vs-weekend heuristic
// applies (Saturday/Sunday always = OT day).
func WithHolidays(h *HolidayStore) Option {
	return func(s *Server) { s.holidays = h }
}

// WithNotifications attaches a per-device notification store. When
// WithCredentials enables Web UI login. When set, every route except
// the login page itself + favicon requires a valid session cookie;
// requests without one are redirected to /login (HTML clients) or
// returned 401 (JSON / SSE clients). Pass nil to disable the login
// gate entirely (default).
func WithCredentials(store *CredentialsStore) Option {
	return func(s *Server) {
		s.creds = store
		if store != nil && s.sessions == nil {
			s.sessions = NewSessionStore()
		}
	}
}

// WithVersion stamps the build identity (Version / Commit / Date)
// onto the server so /api/version can return it. When repo is
// non-empty, it also enables /api/version/check (probe GitHub for
// the latest tag) and /api/upgrade (trigger self-upgrade via the
// published install.sh).
//
// repo format: "owner/name" — e.g. "xxl6097/argus-app".
func WithVersion(v VersionInfo, repo string) Option {
	return func(s *Server) {
		s.version = v
		if repo != "" {
			s.versionSvc = NewVersionService(repo)
		}
	}
}

// set, OnEvent fires webhooks and ntfy publishes, and /api/notifications
// / /api/notifications/messages endpoints are served.
func WithNotifications(store *NotifyStore, notifier *Notifier) Option {
	return func(s *Server) {
		s.notifyStore = store
		s.notifier = notifier
	}
}

// Server is an http.Handler that serves the argus dashboard + API.
// Embed it in your own http.ServeMux or pass it directly to
// http.ListenAndServe.
//
// Server is safe for concurrent use by multiple HTTP clients.
type Server struct {
	watcher *argus.Watcher
	mux     *http.ServeMux

	// SSE fan-out: set of subscriber channels. Each /api/events connection
	// registers a channel on connect and un-registers on disconnect.
	subsMu sync.RWMutex
	subs   map[chan argus.Event]struct{}

	// Offline cache: devices that have gone Offline but we still want to
	// surface in /api/devices. Entries are added by OnEvent on EventOffline
	// and removed on EventOnline for the same MAC; also evicted by TTL
	// (offlineTTL) and soft cap (offlineMax).
	offlineMu  sync.Mutex
	offline    map[string]offlineEntry
	offlineTTL time.Duration
	offlineMax int

	// aliases is an optional user-managed MAC -> friendly-name store.
	// When non-nil, /api/devices rows carry an `alias` field and the
	// dashboard prefers it for display. nil means "no alias feature".
	aliases *AliasStore

	// dhcp is an optional router-specific manager for static DHCP
	// reservations. Exposes /api/dhcp when non-nil; the dashboard
	// hides the "set static IP" UI when nil.
	dhcp DHCPManager

	// history persists per-MAC ONLINE/OFFLINE transitions for the
	// expandable-row timeline and the worktime report. nil disables
	// both /api/history and /api/worktime.
	history *HistoryStore

	// settings is the user-configurable "me MAC + workday window"
	// store. nil disables /api/settings and /api/worktime.
	settings *SettingsStore

	// overrides is the per-(mac, date) manual in/out store. Lets the
	// user fix worktime days where the Watcher missed transitions.
	// nil disables /api/worktime/override.
	overrides *OverrideStore

	// holidays tags dates as legal holidays / 调休 workdays so
	// the worktime compute can special-case them as OT days.
	// nil = weekday/weekend heuristic only.
	holidays *HolidayStore

	// notifyStore is the per-device webhook + ntfy config store.
	// notifier dispatches events to those destinations.
	notifyStore *NotifyStore
	notifier    *Notifier

	// writeAuth gates mutating APIs (POST/DELETE /api/aliases). nil
	// means the default LAN policy (loopback + RFC1918).
	writeAuth AuthCheck

	// creds + sessions implement the Web UI login gate. When creds is
	// nil, requireAuth() short-circuits to "always allowed" — that's
	// the legacy / dev path where the dashboard is exposed unauthenticated.
	// When creds is non-nil, every route except /login + /api/login +
	// /favicon.ico requires a valid session cookie.
	creds    *CredentialsStore
	sessions *SessionStore

	// version is the build-stamped identity of this binary, surfaced
	// at /api/version. versionSvc handles GitHub probing + cache and
	// drives the "check for upgrades" flow when non-nil.
	version    VersionInfo
	versionSvc *VersionService

	// recentSyslog is a per-MAC short-lived cache of the most recent
	// syslog event seen for each direction (connect / disconnect).
	// OnEvent consults this within syslogHintTTL to attribute the
	// fetcher-detected ONLINE/OFFLINE to a specific syslog cause
	// (WIFI_CONNECT, WPA_COMPLETE, DHCP_ACK, ...). When no recent
	// hint matches the direction, the entry is recorded with the
	// current FetcherKind (poll-based attribution) instead.
	syslogMu    sync.Mutex
	syslogHints map[string]syslogHint

	// dataDir is the on-disk root holding the JSON stores + history/.
	// Empty disables /api/backup/export and /api/backup/import.
	dataDir string
}

// syslogHint is one direction-tagged syslog observation (connect side
// vs disconnect side), stored per MAC. Only the most recent of each
// direction is kept; older entries are overwritten because OnEvent
// only looks back a few seconds anyway.
type syslogHint struct {
	connectKind    string // e.g. "WIFI_CONNECT", "WPA_COMPLETE", "DHCP_ACK"
	connectAt      time.Time
	disconnectKind string // e.g. "WIFI_DISCONNECT", "DEAUTH", "MACTABLE_DELETE"
	disconnectAt   time.Time
}

// offlineEntry stores the last-known Device shape at the moment it went
// offline, plus the time we observed the offline event. LastSeen on the
// Device itself is preserved from the library's point of view (wire
// format reports both as separate fields).
type offlineEntry struct {
	dev       argus.Device
	offlineAt time.Time
}

// NewServer constructs a dashboard server around the given Watcher.
// The Watcher must already be running (or about to run) via its Run
// method elsewhere in the process — Server does NOT call Run for you.
//
// To plumb events into the /api/events SSE stream, wrap your existing
// EventHandler with (*Server).OnEvent before passing it to Watcher.Run:
//
//	srv := web.NewServer(w)
//	w.Run(ctx, srv.OnEvent, nil)
//
// If you already have a business EventHandler, call srv.OnEvent
// alongside it:
//
//	w.Run(ctx, func(e argus.Event) {
//	    srv.OnEvent(e)
//	    myBusinessHandler(e)
//	}, nil)
func NewServer(w *argus.Watcher, opts ...Option) *Server {
	s := &Server{
		watcher:     w,
		mux:         http.NewServeMux(),
		subs:        make(map[chan argus.Event]struct{}),
		offline:     make(map[string]offlineEntry),
		offlineTTL:  defaultOfflineRetention,
		offlineMax:  defaultOfflineMax,
		syslogHints: make(map[string]syslogHint),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.writeAuth == nil {
		s.writeAuth = defaultLANAuth
	}
	// Login flow routes — bypass requireAuth so unauthenticated users
	// can actually reach the login form.
	s.mux.HandleFunc("/login", s.handleLogin)
	s.mux.HandleFunc("/api/login", s.handleAPILogin)
	s.mux.HandleFunc("/api/logout", s.handleAPILogout)
	// Favicon stays public — it's pulled by the browser before any
	// auth happens, and serving it 401 just produces console noise.
	s.mux.HandleFunc("/favicon.ico", s.handleFavicon)

	// Everything else goes through requireAuth. When creds == nil
	// requireAuth is a pass-through, so the legacy "no login gate"
	// behaviour is preserved.
	gate := s.requireAuth
	s.mux.HandleFunc("/", gate(s.handleIndex))
	s.mux.HandleFunc("/api/devices", gate(s.handleDevices))
	s.mux.HandleFunc("/api/events", gate(s.handleEvents))
	s.mux.HandleFunc("/api/aliases", gate(s.handleAliases))
	s.mux.HandleFunc("/api/dhcp", gate(s.handleDHCP))
	s.mux.HandleFunc("/api/system/reboot", gate(s.handleReboot))
	s.mux.HandleFunc("/api/system/restart-network", gate(s.handleRestartNetwork))
	s.mux.HandleFunc("/api/history", gate(s.handleHistory))
	s.mux.HandleFunc("/api/worktime", gate(s.handleWorktime))
	s.mux.HandleFunc("/api/worktime/month", gate(s.handleWorktimeMonth))
	s.mux.HandleFunc("/api/worktime/override", gate(s.handleWorktimeOverride))
	s.mux.HandleFunc("/api/settings", gate(s.handleSettings))
	s.mux.HandleFunc("/api/holidays", gate(s.handleHolidays))
	s.mux.HandleFunc("/api/notifications", gate(s.handleNotifications))
	s.mux.HandleFunc("/api/notifications/messages", gate(s.handleNotificationMessages))
	s.mux.HandleFunc("/api/notifications/test", gate(s.handleNotificationTest))
	s.mux.HandleFunc("/api/password", gate(s.handleAPIPassword))
	s.mux.HandleFunc("/api/version", gate(s.handleVersion))
	s.mux.HandleFunc("/api/version/check", gate(s.handleVersionCheck))
	s.mux.HandleFunc("/api/upgrade", gate(s.handleUpgrade))
	s.mux.HandleFunc("/api/backup/export", gate(s.handleBackupExport))
	s.mux.HandleFunc("/api/backup/import", gate(s.handleBackupImport))
	return s
}

// History returns the attached HistoryStore, or nil when history
// is disabled. Useful for embedders that want to seed baseline
// ONLINE entries on startup.
func (s *Server) History() *HistoryStore { return s.history }

// RefreshNotifySubs rebuilds ntfy subscriptions. Exposed so the
// process owner can trigger it once after the watcher has populated
// its known set (alias-keyed configs need that to resolve).
func (s *Server) RefreshNotifySubs() { s.refreshNotifySubs() }

// StopNotifier cancels all ntfy subscribers. Safe to call when
// notifications are disabled.
func (s *Server) StopNotifier() {
	if s.notifier != nil {
		s.notifier.StopSubscriptions()
	}
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// OnEvent fans an Event out to SSE subscribers AND maintains the offline
// cache used by /api/devices. Safe to call from any goroutine.
//
// Offline cache behavior:
//   - EventOffline adds the Device to the cache (keyed by MAC).
//   - EventOnline removes the MAC from the cache (the Watcher's Known()
//     will surface it as online).
//   - EventChange updates the cached entry IF the MAC is currently in
//     the cache (i.e. the device is in its offline retention window
//     and picked up a field change anyway).
//
// SSE fan-out: returns immediately; if a subscriber's channel is full
// the event is dropped for that subscriber only (others unaffected).
// The channel buffer is deliberately small (8) so a slow client does
// not pin memory for the whole server.
func (s *Server) OnEvent(e argus.Event) {
	s.updateOfflineCache(e)
	if s.history != nil {
		s.history.Record(e, s.sourceFor(e))
	}
	// Dispatch to webhook/ntfy with rich markdown context. We do this
	// here (not inside Notifier) because the context — alias, punch
	// membership, worktime stats — lives in Server-level stores.
	s.dispatchNotify(e)

	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	for ch := range s.subs {
		select {
		case ch <- e:
		default:
			// Slow subscriber — drop. Clients should reconnect if they
			// miss events; the dashboard /api/devices endpoint lets
			// them re-sync on (re)load.
		}
	}
}

// syslogHintTTL caps how stale a syslog hint can be before OnEvent
// stops crediting it as the trigger of a fetcher-detected transition.
// Real wifi flows fire connect-side syslog (WPA_COMPLETE / WIFI_CONNECT
// / DHCP_ACK) within ~1s of the device showing up in poll results, so
// 8s gives slack without smearing into the next session.
const syslogHintTTL = 8 * time.Second

// OnSyslog records a low-level syslog observation for a MAC, keyed by
// direction (connect vs disconnect). The next ONLINE/OFFLINE that
// matches that direction will be attributed to this syslog cause
// instead of the generic poll fetcher. Safe to call from any goroutine.
//
// Wire it in at startup like this:
//
//	owrt.WatchSyslog(ctx, srv.OnSyslog, onError)
func (s *Server) OnSyslog(e argus.SyslogEvent) {
	mac := normalizeMAC(e.MAC)
	if mac == "" {
		return
	}
	s.syslogMu.Lock()
	defer s.syslogMu.Unlock()
	h := s.syslogHints[mac]
	switch {
	case e.Kind.IsConnect():
		h.connectKind = e.Kind.String()
		h.connectAt = nonZeroTime(e.Time)
	case e.Kind.IsDisconnect():
		h.disconnectKind = e.Kind.String()
		h.disconnectAt = nonZeroTime(e.Time)
	default:
		return
	}
	s.syslogHints[mac] = h
}

// sourceFor returns the attribution string for a fetcher-emitted event.
// Returns "syslog:<KIND>" when a fresh same-direction hint exists,
// otherwise "fetcher:<kind>" using the watcher's selected fetcher.
func (s *Server) sourceFor(e argus.Event) string {
	mac := normalizeMAC(e.Device.MAC)
	now := nonZeroTime(e.Time)
	s.syslogMu.Lock()
	h, ok := s.syslogHints[mac]
	s.syslogMu.Unlock()
	if ok {
		switch e.Kind {
		case argus.EventOnline:
			if h.connectKind != "" && now.Sub(h.connectAt) <= syslogHintTTL && now.Sub(h.connectAt) >= -syslogHintTTL {
				return "syslog:" + h.connectKind
			}
		case argus.EventOffline:
			if h.disconnectKind != "" && now.Sub(h.disconnectAt) <= syslogHintTTL && now.Sub(h.disconnectAt) >= -syslogHintTTL {
				return "syslog:" + h.disconnectKind
			}
		}
	}
	if k := s.watcher.FetcherKind(); k != "" {
		return "fetcher:" + string(k)
	}
	return "fetcher"
}

func (s *Server) updateOfflineCache(e argus.Event) {
	switch e.Kind {
	case argus.EventOffline:
		s.offlineMu.Lock()
		defer s.offlineMu.Unlock()
		if s.offlineMax > 0 && len(s.offline) >= s.offlineMax {
			// Evict the oldest entry by offlineAt. This is O(n) but n is
			// bounded by offlineMax and only triggers when at capacity,
			// so overall cost is amortized and the map stays bounded.
			var oldestMAC string
			var oldestTime time.Time
			for m, entry := range s.offline {
				if oldestMAC == "" || entry.offlineAt.Before(oldestTime) {
					oldestMAC = m
					oldestTime = entry.offlineAt
				}
			}
			delete(s.offline, oldestMAC)
		}
		s.offline[normalizeMAC(e.Device.MAC)] = offlineEntry{
			dev:       e.Device,
			offlineAt: nonZeroTime(e.Time),
		}

	case argus.EventOnline:
		s.offlineMu.Lock()
		defer s.offlineMu.Unlock()
		delete(s.offline, normalizeMAC(e.Device.MAC))

	case argus.EventChange:
		// Only relevant if the MAC is currently in our offline cache —
		// an offline device that happened to get an enrichment update.
		s.offlineMu.Lock()
		defer s.offlineMu.Unlock()
		mac := normalizeMAC(e.Device.MAC)
		if existing, ok := s.offline[mac]; ok {
			existing.dev = e.Device
			s.offline[mac] = existing
		}
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	// no-cache + ETag: browser keeps the cached body but revalidates
	// on every navigation. After a self-upgrade the embedded HTML
	// changes → ETag changes → 200 with the new body. Old behaviour
	// (max-age=300) silently served stale UI for 5 min after upgrade.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", dashboardETag)
	if match := r.Header.Get("If-None-Match"); match != "" && match == dashboardETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(dashboardHTML)
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	// Embedded asset never changes between rebuilds; cache for a day.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(faviconICO)
}

// deviceRow is the wire format for /api/devices. Fields mirror the
// stable JSON field names in STABILITY.md so consumers can script
// against them.
//
// Status, OfflineAtMs, and Alias are web additions and are
// documented in STABILITY.md under the web subpackage.
type deviceRow struct {
	MAC         string `json:"mac"`
	IP          string `json:"ip,omitempty"`
	Hostname    string `json:"hostname,omitempty"`
	Vendor      string `json:"vendor,omitempty"`
	Type        string `json:"type,omitempty"`
	Radio       string `json:"radio,omitempty"`
	SSID        string `json:"ssid,omitempty"`
	Channel     int    `json:"channel,omitempty"`
	RSSI        int    `json:"rssi,omitempty"`
	Wired       bool   `json:"wired"`
	LastSeenMs  int64  `json:"last_seen_ms,omitempty"`
	Status      string `json:"status"`                  // "online" | "offline" (since v0.13.3)
	OfflineAtMs int64  `json:"offline_at_ms,omitempty"` // set when status=="offline"
	Alias       string `json:"alias,omitempty"`         // user-defined name (since v0.14.0)
	IsMe        bool   `json:"is_me,omitempty"`         // tagged as 打卡设备 (workflow statistics)
}

// applyAlias annotates the row with a user-defined friendly name when
// one is configured. Returns the row unchanged if no alias store is
// attached or the MAC has no entry. Also tags the row as IsMe when
// the MAC appears in the 打卡设备 set in settings.
func (s *Server) applyAlias(row deviceRow) deviceRow {
	if s.aliases != nil {
		if name := s.aliases.Lookup(row.MAC); name != "" {
			row.Alias = name
		}
	}
	if s.settings != nil && s.settings.IsPunch(row.MAC) {
		row.IsMe = true
	}
	return row
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	known := s.watcher.Known()

	// Prune offline cache: drop entries older than offlineTTL, and any
	// MAC that reappeared in known (defensive — OnEvent should have
	// already removed it, but a lost EventOnline shouldn't pin an
	// incorrect offline row forever).
	s.offlineMu.Lock()
	now := time.Now()
	for mac, entry := range s.offline {
		if s.offlineTTL > 0 && now.Sub(entry.offlineAt) > s.offlineTTL {
			delete(s.offline, mac)
			continue
		}
		if _, onlineNow := known[mac]; onlineNow {
			delete(s.offline, mac)
		}
	}
	// Copy offline entries while holding the lock so we can release it
	// before the JSON encoding.
	offlineSnapshot := make(map[string]offlineEntry, len(s.offline))
	for k, v := range s.offline {
		offlineSnapshot[k] = v
	}
	s.offlineMu.Unlock()

	rows := make([]deviceRow, 0, len(known)+len(offlineSnapshot))
	onlineCount := 0
	offlineCount := 0

	for _, d := range known {
		row := deviceRow{
			MAC:      strings.ToUpper(d.MAC),
			IP:       d.IP,
			Hostname: d.Hostname,
			Vendor:   d.Vendor,
			Type:     d.Type,
			Radio:    d.Radio,
			SSID:     d.SSID,
			Channel:  d.Channel,
			RSSI:     d.RSSI,
			Wired:    d.Wired(),
			Status:   "online",
		}
		if !d.LastSeen.IsZero() {
			row.LastSeenMs = d.LastSeen.UnixMilli()
		}
		rows = append(rows, s.applyAlias(row))
		onlineCount++
	}

	for mac, entry := range offlineSnapshot {
		if _, ok := known[mac]; ok {
			continue // defensive: already online, don't double-count
		}
		d := entry.dev
		row := deviceRow{
			MAC:         strings.ToUpper(d.MAC),
			IP:          d.IP,
			Hostname:    d.Hostname,
			Vendor:      d.Vendor,
			Type:        d.Type,
			Radio:       d.Radio,
			SSID:        d.SSID,
			Channel:     d.Channel,
			RSSI:        d.RSSI,
			Wired:       d.Wired(),
			Status:      "offline",
			OfflineAtMs: entry.offlineAt.UnixMilli(),
		}
		if !d.LastSeen.IsZero() {
			row.LastSeenMs = d.LastSeen.UnixMilli()
		}
		rows = append(rows, s.applyAlias(row))
		offlineCount++
	}

	// Sort: online first, then offline, each alphabetical by MAC. Keeps
	// the active fleet visually on top; offline history trails below.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Status != rows[j].Status {
			return rows[i].Status == "online"
		}
		return rows[i].MAC < rows[j].MAC
	})

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"devices":      rows,
		"count":        len(rows),
		"online":       onlineCount,
		"offline":      offlineCount,
		"capabilities": s.capabilities(),
	})
}

// capabilities tells the dashboard which features it may expose. The
// fields are part of the Stable wire shape (see STABILITY.md).
func (s *Server) capabilities() map[string]bool {
	return map[string]bool{
		"aliases":       s.aliases != nil,
		"dhcp":          s.dhcp != nil,
		"history":       s.history != nil,
		"worktime":      s.history != nil && s.settings != nil,
		"overrides":     s.overrides != nil,
		"settings":      s.settings != nil,
		"holidays":      s.holidays != nil,
		"notifications": s.notifyStore != nil,
	}
}

// dayKindFor returns the DayKind for t, consulting the HolidayStore
// when attached and falling back to weekday/weekend detection.
func (s *Server) dayKindFor(t time.Time) DayKind {
	if s.holidays != nil {
		return s.holidays.Kind(t)
	}
	wd := t.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return DayKindWeekend
	}
	return DayKindWorkday
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE requires a flushable ResponseWriter", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	// Prevent proxy buffering (e.g. nginx needs `proxy_buffering off`).
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan argus.Event, 8)
	s.subsMu.Lock()
	s.subs[ch] = struct{}{}
	s.subsMu.Unlock()
	defer func() {
		s.subsMu.Lock()
		delete(s.subs, ch)
		s.subsMu.Unlock()
	}()

	// Initial hello so clients see the connection is live.
	_, _ = w.Write([]byte("event: hello\ndata: {\"ok\":true}\n\n"))
	flusher.Flush()

	ctx := r.Context()
	enc := json.NewEncoder(w)
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			// SSE envelope: `event: <kind>\ndata: <json>\n\n`
			_, _ = w.Write([]byte("event: " + e.Kind.String() + "\ndata: "))
			if err := enc.Encode(e); err != nil {
				return
			}
			_, _ = w.Write([]byte("\n"))
			flusher.Flush()
		}
	}
}

// Shutdown disconnects all SSE subscribers. Callers typically wrap
// Server in an http.Server and call that server's Shutdown; this
// method is exposed so embedders without http.Server can drain
// subscribers explicitly.
func (s *Server) Shutdown(_ context.Context) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for ch := range s.subs {
		delete(s.subs, ch)
		close(ch)
	}
}

func normalizeMAC(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func nonZeroTime(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

// handleAliases multiplexes GET / POST / DELETE on /api/aliases.
//
//	GET    /api/aliases                -> {"aliases": {MAC: name, ...}}
//	POST   /api/aliases  {mac, name}   -> {"ok": true, "mac": MAC, "name": N}
//	                                      empty name deletes the alias
//	DELETE /api/aliases?mac=AA:BB:...  -> {"ok": true}
//
// All paths return JSON. Mutating methods are gated by s.writeAuth.
func (s *Server) handleAliases(w http.ResponseWriter, r *http.Request) {
	if s.aliases == nil {
		http.Error(w, `{"error":"alias store not configured"}`, http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.handleAliasesGet(w, r)
	case http.MethodPost:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		s.handleAliasesSet(w, r)
	case http.MethodDelete:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		s.handleAliasesDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleAliasesGet(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{"aliases": s.aliases.All()})
}

func (s *Server) handleAliasesSet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		MAC  string `json:"mac"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if err := s.aliases.Set(in.MAC, in.Name); err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":   true,
		"mac":  strings.ToUpper(normalizeMAC(in.MAC)),
		"name": strings.TrimSpace(in.Name),
	})
}

func (s *Server) handleAliasesDelete(w http.ResponseWriter, r *http.Request) {
	mac := r.URL.Query().Get("mac")
	if mac == "" {
		writeJSONErr(w, http.StatusBadRequest, "mac query parameter required")
		return
	}
	if err := s.aliases.Set(mac, ""); err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func writeJSONErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// defaultLANAuth is the default predicate used when the user doesn't
// override WithWriteAuth. It allows requests whose remote address is
// loopback or any of the well-known "private network" ranges in IPv4
// (RFC1918) or IPv6 (ULA fc00::/7, link-local fe80::/10) — appropriate
// for a dashboard bound to a home LAN. X-Forwarded-For is NOT
// consulted: if you front Argus with a reverse proxy, supply your own
// AuthCheck.
//
// IPv6 dual-stack note: when argus-app binds [::]:9099 modern clients
// (browsers using Happy Eyeballs, mDNS-resolved hostnames, etc.) often
// land on an IPv6 socket — so we MUST allow ULA and link-local, not
// just RFC1918, or every browser request would 403.
func defaultLANAuth1(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	// Strip IPv6 zone (e.g. "fe80::1%br-lan").
	if i := strings.IndexByte(host, '%'); i >= 0 {
		host = host[:i]
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	// Normalise IPv4-mapped IPv6 ("::ffff:192.168.x.y") so the RFC1918
	// check below works on the embedded v4 octets.
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.To4() != nil {
		return isRFC1918(ip)
	}
	return isPrivateV6(ip)
}

func defaultLANAuth(r *http.Request) bool {
	return true
}

// isPrivateV6 returns true for IPv6 addresses that should be treated
// as "trusted LAN" — ULA (fc00::/7, includes fd00::/8) and link-local
// (fe80::/10).
func isPrivateV6(ip net.IP) bool {
	if ip.IsLinkLocalUnicast() {
		return true
	}
	// fc00::/7 — Unique Local Addresses (RFC 4193). Go's stdlib has
	// IsPrivate() since 1.17 that covers this; use it instead of
	// hand-rolling CIDR matching.
	return ip.IsPrivate()
}

func isRFC1918(ip net.IP) bool {
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		_, subnet, _ := net.ParseCIDR(cidr)
		if subnet.Contains(ip) {
			return true
		}
	}
	return false
}

// -------- Web UI auth gate (cookie-based login) --------

// requireAuth wraps an http.HandlerFunc so it only runs after the
// caller presents a valid session cookie. When s.creds is nil the
// gate is disabled and requests pass through unchanged — that's the
// legacy "no login" mode preserved by `-credentials=""`.
//
// Failure modes:
//   - HTML clients (Accept includes text/html) → 302 to /login?next=<path>
//   - JSON / SSE / curl / fetch → 401 with a JSON body
//
// The HTML-vs-API split lets the dashboard's running fetch() calls
// surface a clean 401 (so the SSE client can reconnect after login)
// while making the address-bar experience obvious for humans.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.creds == nil {
			next(w, r)
			return
		}
		c, err := r.Cookie(cookieName)
		if err == nil {
			if user, ok := s.sessions.Validate(c.Value); ok {
				// Stash username for downstream handlers that care
				// (currently only /api/password).
				ctx := contextWithUser(r.Context(), user)
				next(w, r.WithContext(ctx))
				return
			}
		}
		// Unauthenticated. HTML browsers get redirected; everything
		// else gets a 401 JSON.
		if wantsHTML(r) {
			next := url.QueryEscape(r.URL.RequestURI())
			http.Redirect(w, r, "/login?next="+next, http.StatusFound)
			return
		}
		writeJSONErr(w, http.StatusUnauthorized, "unauthenticated")
	}
}

// wantsHTML returns true when the client appears to be a browser
// asking for a page (vs an XHR / fetch / curl / SSE consumer).
// Heuristic: Accept header contains text/html and is NOT an SSE
// stream (Accept: text/event-stream or X-Requested-With: fetch).
func wantsHTML(r *http.Request) bool {
	if r.Header.Get("X-Requested-With") != "" {
		return false
	}
	accept := r.Header.Get("Accept")
	if strings.Contains(accept, "text/event-stream") {
		return false
	}
	return strings.Contains(accept, "text/html")
}

// userCtxKey is the unexported context key used to stash the
// authenticated username on the request context.
type userCtxKey struct{}

func contextWithUser(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, userCtxKey{}, user)
}

func userFromContext(ctx context.Context) string {
	v, _ := ctx.Value(userCtxKey{}).(string)
	return v
}

// handleLogin serves the login page (GET) — a single embedded HTML
// document that posts to /api/login. Public route.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// If creds is disabled, /login is meaningless — bounce back to /.
	if s.creds == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	// Already logged in? Bounce to dashboard so users don't get stuck
	// staring at the login page after refreshing.
	if c, err := r.Cookie(cookieName); err == nil {
		if _, ok := s.sessions.Validate(c.Value); ok {
			http.Redirect(w, r, nextOrRoot(r), http.StatusFound)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(loginHTML)
}

// nextOrRoot extracts ?next=URL from the query string, sanity-checks
// it, and falls back to "/". Prevents open-redirect: the URL must
// start with a single slash and not be //... (which would resolve
// to a different host).
func nextOrRoot(r *http.Request) string {
	n := r.URL.Query().Get("next")
	if n == "" || !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") {
		return "/"
	}
	return n
}

// handleAPILogin processes a username/password POST. On success it
// writes the session cookie and returns JSON describing where to go
// next ("/" or "/login?must_change=1" depending on the seeded-default
// flag). On failure: 401.
func (s *Server) handleAPILogin(w http.ResponseWriter, r *http.Request) {
	if s.creds == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "credentials not enabled")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if !s.creds.Verify(in.Username, in.Password) {
		// Constant-ish response time — bcrypt already runs ~100ms
		// on success and failure, so we don't need an explicit sleep.
		writeJSONErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	tok, err := s.sessions.Issue(s.creds.Username())
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "issue session: "+err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// No MaxAge → session cookie. The server-side TTL still bounds
		// validity to sessionTTL; this just avoids persisting the
		// cookie across browser restarts. Add MaxAge if you want
		// "remember me" behaviour.
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":          true,
		"must_change": s.creds.MustChange(),
	})
}

// handleAPILogout drops the current session. Idempotent; always 200.
func (s *Server) handleAPILogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if c, err := r.Cookie(cookieName); err == nil && s.sessions != nil {
		s.sessions.Revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleAPIPassword changes the admin password. Requires an active
// session AND the caller to demonstrate knowledge of the current
// password. After a successful change all OTHER sessions are
// revoked so a forgotten browser can't continue with the old
// credentials.
func (s *Server) handleAPIPassword(w http.ResponseWriter, r *http.Request) {
	if s.creds == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "credentials not enabled")
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
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048)).Decode(&in); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if err := s.creds.ChangePassword(in.OldPassword, in.NewPassword); err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Drop every existing session and issue a fresh one for the
	// caller, so other browsers (and any leaked cookie) get logged out.
	s.sessions.RevokeAll()
	tok, err := s.sessions.Issue(s.creds.Username())
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "issue session: "+err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// handleVersion returns the running binary's identity. Cheap, no
// network. The dashboard polls this once on load to fill the
// version pill in the header.
//
//	GET /api/version  →  {"version":"v0.1.13","commit":"abc1234","date":"..."}
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"version":      s.version.Version,
		"commit":       s.version.Commit,
		"date":         s.version.Date,
		"upgrade_open": s.versionSvc != nil,
	})
}

// handleVersionCheck probes GitHub for the latest release. Cached
// for 30 minutes; ?force=1 bypasses the cache (the dashboard's
// "check now" button passes this).
//
//	GET /api/version/check          → cached probe
//	GET /api/version/check?force=1  → fresh probe
//
// Response shape:
//
//	{
//	  "current": "v0.1.13",
//	  "latest":  "v0.1.14",
//	  "has_update": true,
//	  "release_url": "https://github.com/.../releases/tag/v0.1.14",
//	  "notes": "...",
//	  "fetched_at": "2026-05-14T22:30:00Z"
//	}
func (s *Server) handleVersionCheck(w http.ResponseWriter, r *http.Request) {
	if s.versionSvc == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "version check disabled")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	force := r.URL.Query().Get("force") == "1"
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	rel, err := s.versionSvc.Latest(ctx, force)
	if err != nil {
		writeJSONErr(w, http.StatusBadGateway, "fetch latest: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"current":     s.version.Version,
		"latest":      rel.TagName,
		"name":        rel.Name,
		"has_update":  HasUpdate(s.version.Version, rel.TagName),
		"release_url": rel.HTMLURL,
		"notes":       rel.Body,
		"prerelease":  rel.Prerelease,
		"fetched_at":  rel.FetchedAt.Format(time.RFC3339),
	})
}

// handleUpgrade kicks off a self-upgrade by spawning a detached
// shell that re-runs install.sh. argus-app itself will be killed
// shortly afterwards (install.sh stops + restarts the procd
// service), so the response goes out BEFORE the kill happens.
//
// The body may carry {"version":"vX.Y.Z"} to pin a target version;
// empty/missing means "latest".
//
//	POST /api/upgrade  {"version":"v0.1.14"}
//	→ {"ok":true,"started":true,"target":"v0.1.14",
//	   "log":"/tmp/argus-upgrade.log"}
//
// Gated by writeAuth in addition to requireAuth, since it's a host
// mutation.
func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.versionSvc == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "upgrade disabled")
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
		Version string `json:"version"`
	}
	// Body is optional; ignore decode errors so an empty POST works.
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in)
	target := strings.TrimSpace(in.Version)
	if err := triggerUpgrade(target); err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "spawn upgrade: "+err.Error())
		return
	}
	resolved := target
	if resolved == "" {
		resolved = "latest"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"started": true,
		"target":  resolved,
		"log":     "/tmp/argus-upgrade.log",
		"hint":    "服务将在 30-60 秒内重启,期间页面短暂不可用,完成后请刷新",
	})
}

// handleHistory serves GET /api/history?mac=XX&from=YYYY-MM-DD&to=YYYY-MM-DD
// Returns the online/offline transition log for a MAC. from/to are
// optional; defaults to "last 30 days" when omitted.
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.history == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "history not enabled")
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
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")
	var from, to time.Time
	if fromStr != "" {
		var err error
		from, err = time.ParseInLocation("2006-01-02", fromStr, time.Local)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "from must be YYYY-MM-DD")
			return
		}
	} else {
		from = time.Now().Add(-HistoryRetention)
	}
	if toStr != "" {
		var err error
		to, err = time.ParseInLocation("2006-01-02", toStr, time.Local)
		if err != nil {
			writeJSONErr(w, http.StatusBadRequest, "to must be YYYY-MM-DD")
			return
		}
		to = to.Add(24 * time.Hour) // include the entire day
	}
	entries, err := s.history.Query(mac, from, to)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(map[string]any{
		"mac":     strings.ToUpper(normalizeMAC(mac)),
		"entries": entries,
		"count":   len(entries),
	})
}

// handleWorktime serves GET /api/worktime?mac=XX&date=YYYY-MM-DD&start=HH:MM&end=HH:MM
// Returns a computed worktime report for the given MAC on the given
// date. start/end override the stored settings when provided.
func (s *Server) handleWorktime(w http.ResponseWriter, r *http.Request) {
	if s.history == nil || s.settings == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "worktime not enabled")
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
	dateStr := r.URL.Query().Get("date")
	if dateStr == "" {
		dateStr = time.Now().Format("2006-01-02")
	}
	// Parse the date in the server's local time zone, not UTC: "today"
	// means local midnight 00:00 to 24:00, so standard-hour overtime
	// comparisons line up with the user's wall clock.
	date, err := time.ParseInLocation("2006-01-02", dateStr, time.Local)
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}
	cfg := s.settings.Get()
	startHHMM := r.URL.Query().Get("start")
	if startHHMM == "" {
		startHHMM = cfg.WorkStart
	}
	endHHMM := r.URL.Query().Get("end")
	if endHHMM == "" {
		endHHMM = cfg.WorkEnd
	}
	// Fetch entries covering [date-1d, date+1d] so we can see the
	// transition that opened a segment before 00:00.
	from := date.Add(-24 * time.Hour)
	to := date.Add(48 * time.Hour)
	entries, err := s.history.Query(mac, from, to)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var override Override
	if s.overrides != nil {
		if o, ok := s.overrides.Lookup(mac, date.Format("2006-01-02")); ok {
			override = o
		}
	}
	rep := ComputeWorktime(mac, date, startHHMM, endHHMM, entries, time.Now(), override, s.dayKindFor(date))
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
}

// handleWorktimeMonth serves GET /api/worktime/month?mac=XX&month=YYYY-MM&start=HH:MM&end=HH:MM
// Aggregates daily worktime reports across the month. Zero-present
// days are omitted. Overrides are applied per-day, same as the
// single-day endpoint.
func (s *Server) handleWorktimeMonth(w http.ResponseWriter, r *http.Request) {
	if s.history == nil || s.settings == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "worktime not enabled")
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
	monthStr := r.URL.Query().Get("month")
	if monthStr == "" {
		monthStr = time.Now().Format("2006-01")
	}
	monthStart, err := time.ParseInLocation("2006-01", monthStr, time.Local)
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, "month must be YYYY-MM")
		return
	}
	cfg := s.settings.Get()
	startHHMM := r.URL.Query().Get("start")
	if startHHMM == "" {
		startHHMM = cfg.WorkStart
	}
	endHHMM := r.URL.Query().Get("end")
	if endHHMM == "" {
		endHHMM = cfg.WorkEnd
	}

	// Single history read covering the whole month (+1 day padding on
	// each side for "was online at 00:00" detection).
	monthEnd := monthStart.AddDate(0, 1, 0)
	from := monthStart.Add(-24 * time.Hour)
	to := monthEnd.Add(24 * time.Hour)
	entries, err := s.history.Query(mac, from, to)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	rep := MonthlyReport{
		MAC:       strings.ToUpper(normalizeMAC(mac)),
		Month:     monthStart.Format("2006-01"),
		StartHHMM: startHHMM,
		EndHHMM:   endHHMM,
		Days:      []DayWorktime{},
	}
	now := time.Now()
	// Cap iteration at "today" so future days of the current month
	// don't create fake zero-present entries.
	lastDay := monthEnd
	if now.Before(lastDay) {
		lastDay = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).Add(24 * time.Hour)
	}
	for day := monthStart; day.Before(lastDay); day = day.AddDate(0, 0, 1) {
		var override Override
		if s.overrides != nil {
			if o, ok := s.overrides.Lookup(mac, day.Format("2006-01-02")); ok {
				override = o
			}
		}
		dr := ComputeWorktime(mac, day, startHHMM, endHHMM, entries, now, override, s.dayKindFor(day))
		if dr.PresentSecs <= 0 && !dr.Manual {
			continue // skip days the device never showed up (and wasn't manually filled)
		}
		rep.Days = append(rep.Days, DayWorktime{
			Date:            dr.Date,
			PresentSecs:     dr.PresentSecs,
			OvertimeSecs:    dr.OvertimeSecs,
			EarlyOTSecs:     dr.EarlyOTSecs,
			LateOTSecs:      dr.LateOTSecs,
			FirstSeenMs:     dr.FirstSeenMs,
			LastSeenMs:      dr.LastSeenMs,
			ArrivalStatus:   dr.ArrivalStatus,
			DepartureStatus: dr.DepartureStatus,
			DayKind:         dr.DayKind,
			OTDay:           dr.OTDay,
			Manual:          dr.Manual,
			MissingOut:      dr.MissingOut,
			OpenAtEnd:       dr.OpenAtEnd,
		})
		rep.PresentSecs += dr.PresentSecs
		rep.OvertimeSecs += dr.OvertimeSecs
		rep.EarlyOTSecs += dr.EarlyOTSecs
		rep.LateOTSecs += dr.LateOTSecs
		rep.WorkedDays++
		if dr.OTDay {
			rep.OTDays++
		}
		switch dr.ArrivalStatus {
		case "late":
			rep.LateDays++
		case "missed_in":
			rep.MissedInDays++
		}
		if dr.DepartureStatus == "early_leave" {
			rep.EarlyLeaveDays++
		}
	}

	if rep.WorkedDays > 0 {
		rep.AvgDailyOTSecs = rep.OvertimeSecs / int64(rep.WorkedDays)
		// Weekly avg prorates the daily figure to a 5-day work-week.
		// Using 5 (not 7) matches how people typically interpret
		// "周均" for office hours — 5 workdays, not every calendar day.
		rep.AvgWeeklyOTSecs = rep.AvgDailyOTSecs * 5
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
}

// handleWorktimeOverride manages per-(mac, date) manual in/out records.
//
//	GET    /api/worktime/override?mac=XX&date=YYYY-MM-DD
//	POST   /api/worktime/override  {mac, date, in, out}
//	DELETE /api/worktime/override?mac=XX&date=YYYY-MM-DD
//
// POST / DELETE are gated by writeAuth.
func (s *Server) handleWorktimeOverride(w http.ResponseWriter, r *http.Request) {
	if s.overrides == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "overrides not enabled")
		return
	}
	switch r.Method {
	case http.MethodGet:
		mac := r.URL.Query().Get("mac")
		date := r.URL.Query().Get("date")
		if mac == "" || date == "" {
			writeJSONErr(w, http.StatusBadRequest, "mac and date query parameters required")
			return
		}
		o, ok := s.overrides.Lookup(mac, date)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mac":      strings.ToUpper(normalizeMAC(mac)),
			"date":     date,
			"exists":   ok,
			"override": o,
		})
	case http.MethodPost:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		var in struct {
			MAC  string `json:"mac"`
			Date string `json:"date"`
			In   string `json:"in"`
			Out  string `json:"out"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := s.overrides.Set(in.MAC, in.Date, Override{In: in.In, Out: in.Out}); err != nil {
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
		mac := r.URL.Query().Get("mac")
		date := r.URL.Query().Get("date")
		if mac == "" || date == "" {
			writeJSONErr(w, http.StatusBadRequest, "mac and date query parameters required")
			return
		}
		if err := s.overrides.Delete(mac, date); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleSettings multiplexes GET / POST / DELETE on /api/settings.
//
// GET  /api/settings
//
//	-> {"punch_macs": ["AA:..", "BB:.."], "work_start": "09:00", "work_end": "18:30"}
//
// POST /api/settings
//
//	Body shapes (multiplexed on what fields are present):
//	  - {"punch_mac": "AA:..", "punch": true}   add a 打卡设备
//	  - {"punch_mac": "AA:..", "punch": false}  remove a 打卡设备
//	  - {"work_start": "09:00", "work_end": "18:30"}  update hours
//	Combined bodies are fine; each field is applied if set.
//
// DELETE /api/settings
//   - ?clear=punch        wipe the entire 打卡设备 set
//   - ?clear=me           alias of clear=punch (legacy)
//   - ?mac=AA:..          remove a single mac from the set
//
// POST / DELETE are gated by writeAuth. GET is public (read-only).
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
			// Legacy alias: older clients posted {"me_mac": "AA:.."} to
			// set the single punch device. Treat it as add-only.
			MeMAC string `json:"me_mac,omitempty"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid json body")
			return
		}
		// Work hours
		if in.WorkStart != "" || in.WorkEnd != "" {
			if err := s.settings.Update(Settings{
				WorkStart: in.WorkStart,
				WorkEnd:   in.WorkEnd,
			}); err != nil {
				writeJSONErr(w, http.StatusBadRequest, err.Error())
				return
			}
		}
		// Global webhook URL
		if in.GlobalWebhookURL != nil {
			if err := s.settings.SetGlobalWebhook(*in.GlobalWebhookURL); err != nil {
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
		clear := r.URL.Query().Get("clear")
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
//
// POST / DELETE are gated by writeAuth.
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

// chineseWeekdays maps Go's time.Weekday to Chinese day names.
var chineseWeekdays = []string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}

// historySyslogLabels maps the SyslogKind.String() values that the
// argusd library emits to the same Chinese pills the dashboard renders.
// Keep in sync with HISTORY_SYSLOG_LABELS in dashboard.html.
var historySyslogLabels = map[string]string{
	"WIFI_CONNECT":    "无线接入",
	"WPA_COMPLETE":    "认证完成",
	"DHCP_ACK":        "DHCP 分配",
	"MACTABLE_INSERT": "MAC 表新增",
	"WIFI_DISCONNECT": "无线断开",
	"DEAUTH":          "认证踢出",
	"MACTABLE_DELETE": "MAC 表移除",
}

// sourceLabel renders an attribution tag (as produced by Server.sourceFor)
// into a human-readable Chinese phrase suitable for webhook / ntfy bodies.
// Returns the empty string when src is empty so callers can soft-skip.
func sourceLabel(src string) string {
	if src == "" {
		return ""
	}
	if src == "seed" {
		return "启动快照"
	}
	if i := strings.Index(src, ":"); i > 0 {
		head, tail := src[:i], src[i+1:]
		switch head {
		case "syslog":
			if v, ok := historySyslogLabels[tail]; ok {
				return v
			}
			return tail
		case "fetcher":
			return tail + " 轮询"
		}
	}
	return src
}

// dispatchNotify formats a markdown payload and ships it to the
// global webhook (settings.GlobalWebhookURL) and the per-device
// webhook/ntfy destinations. Either / both may be enabled; the
// payload carries a `scope` field ("global" or "device") so the
// receiver can tell them apart.
func (s *Server) dispatchNotify(e argus.Event) {
	if s.notifier == nil {
		return
	}
	if e.Kind != argus.EventOnline && e.Kind != argus.EventOffline {
		return
	}
	mac := normalizeMAC(e.Device.MAC)
	macU := strings.ToUpper(mac)
	alias := ""
	if s.aliases != nil {
		alias = s.aliases.Lookup(mac)
	}
	displayName := alias
	if displayName == "" {
		displayName = e.Device.Hostname
	}
	if displayName == "" {
		displayName = macU
	}
	isPunch := s.settings != nil && s.settings.IsPunch(mac)

	when := nonZeroTime(e.Time)
	// Reuse the same attribution that the history store uses, so the
	// webhook body matches the timeline pill exactly. sourceFor() peeks
	// at the syslog hint cache; we have to call it BEFORE OnEvent's
	// history.Record runs (otherwise the hint TTL window closes), but
	// since dispatchNotify itself runs from inside OnEvent right after
	// Record, the timestamps line up identically.
	source := s.sourceFor(e)
	sourceText := sourceLabel(source)

	base := s.formatNotifyMarkdown(e, when, displayName, alias, mac, isPunch, sourceText)
	if source != "" {
		base["source"] = source
	}
	if sourceText != "" {
		base["source_label"] = sourceText
	}

	// 1) Global webhook (settings-level): fires for ANY device. Optional;
	//    skipped when not configured.
	if s.settings != nil {
		if gURL := s.settings.Get().GlobalWebhookURL; gURL != "" {
			gp := clonePayload(base)
			gp["scope"] = "global"
			s.notifier.Dispatch(mac, NotifyConfig{WebhookURL: gURL}, gp, e.Kind.String())
		}
	}

	// 2) Per-device webhook + ntfy: only when an entry exists for this MAC.
	if s.notifyStore != nil {
		if cfg, ok := s.notifyStore.Lookup(e.Device.MAC); ok {
			dp := clonePayload(base)
			dp["scope"] = "device"
			s.notifier.Dispatch(mac, cfg, dp, e.Kind.String())
		}
	}
}

// clonePayload makes a shallow copy of the notification payload so each
// dispatch can stamp its own `scope` without racing the other goroutine.
func clonePayload(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}

// formatNotifyMarkdown renders the per-device markdown body. Punch
// devices on ONLINE get worktime stats (today's overtime + month
// total); everything else gets the lightweight "上线啦/下线啦" form.
// sourceText is the Chinese attribution (sourceLabel(...)); empty
// string skips the line.
func (s *Server) formatNotifyMarkdown(e argus.Event, when time.Time, displayName, alias, mac string, isPunch bool, sourceText string) map[string]any {
	when = when.In(time.Local)
	dateStr := when.Format("2006-01-02")
	weekday := chineseWeekdays[int(when.Weekday())]
	clock := when.Format("15:04:05")
	clockMs := fmt.Sprintf("%s.%03d", clock, when.Nanosecond()/1e6)
	host, _ := os.Hostname()
	if host == "" {
		host = "—"
	}
	ip := e.Device.IP
	if ip == "" {
		ip = "—"
	}

	markdown := make(map[string]interface{})
	var verb string
	var b strings.Builder
	if isPunch {
		if e.Kind == argus.EventOnline {
			verb = "上班了"
		} else if e.Kind == argus.EventOffline {
			verb = "下班了"
		} else {
			verb = e.Kind.String()
		}
		fmt.Fprintf(&b, "【%s】%s\n", displayName, verb)
		fmt.Fprintf(&b, "- 今天是 %s %s\n", dateStr, weekday)
		fmt.Fprintf(&b, "- 设备：%s\n", host)
		fmt.Fprintf(&b, "- 信号：%d\n", e.Device.RSSI)
		fmt.Fprintf(&b, "- Wi-Fi：%s\n", e.Device.SSID)
		fmt.Fprintf(&b, "- 频道：%s\n", e.Device.Radio)
		fmt.Fprintf(&b, "- 类别：%s\n", e.Device.Type)
		fmt.Fprintf(&b, "- IP地址：%s\n", ip)
		fmt.Fprintf(&b, "- Mac地址：%s\n", strings.ToLower(mac))
		// Worktime context — only meaningful when history+settings are
		// enabled. Soft-skip individual lines that can't be computed.
		if s.history != nil && s.settings != nil {
			cfg := s.settings.Get()
			day, _ := time.ParseInLocation("2006-01-02", dateStr, time.Local)
			from := day.Add(-24 * time.Hour)
			to := day.Add(48 * time.Hour)
			entries, _ := s.history.Query(mac, from, to)
			var override Override
			if s.overrides != nil {
				if o, ok := s.overrides.Lookup(mac, dateStr); ok {
					override = o
				}
			}
			rep := ComputeWorktime(mac, day, cfg.WorkStart, cfg.WorkEnd, entries, when, override, s.dayKindFor(day))
			// On ONLINE we expect FirstSeen to equal `when` (or be very
			// close); on OFFLINE we want LastSeen.
			if e.Kind == argus.EventOnline && rep.FirstSeenMs > 0 {
				fmt.Fprintf(&b, "- 上班时间：%s\n", time.UnixMilli(rep.FirstSeenMs).In(time.Local).Format("15:04:05"))
			} else if e.Kind == argus.EventOffline && rep.LastSeenMs > 0 {
				fmt.Fprintf(&b, "- 下班时间：%s\n", time.UnixMilli(rep.LastSeenMs).In(time.Local).Format("15:04:05"))
			}
			fmt.Fprintf(&b, "- 今日加班时长：%s\n", humanDuration(rep.OvertimeSecs))
			// Month total — current calendar month up to today (or
			// through the event day, whichever the event date implies).
			if monthOT, ok := s.monthOvertimeSecs(mac, day, when); ok {
				fmt.Fprintf(&b, "- 本月加班时长：%s\n", humanDuration(monthOT))
			}
		}
		if sourceText != "" {
			fmt.Fprintf(&b, "- 触发原因：%s\n", sourceText)
		}
		fmt.Fprintf(&b, "- 消息时间：%s", clockMs)
	} else {
		if e.Kind == argus.EventOnline {
			verb = "上线啦"
		} else if e.Kind == argus.EventOffline {
			verb = "下线啦"
		} else {
			verb = e.Kind.String()
		}
		fmt.Fprintf(&b, "【%s】%s\n", displayName, verb)
		fmt.Fprintf(&b, "- 今天是 %s %s\n", dateStr, weekday)
		fmt.Fprintf(&b, "- 设备：%s\n", host)
		fmt.Fprintf(&b, "- 信号：%d\n", e.Device.RSSI)
		fmt.Fprintf(&b, "- Wi-Fi：%s\n", e.Device.SSID)
		fmt.Fprintf(&b, "- 频道：%s\n", e.Device.Radio)
		fmt.Fprintf(&b, "- 类别：%s\n", e.Device.Type)
		fmt.Fprintf(&b, "- IP地址：%s\n", ip)
		fmt.Fprintf(&b, "- Mac地址：%s\n", strings.ToLower(mac))
		if sourceText != "" {
			fmt.Fprintf(&b, "- 触发原因：%s\n", sourceText)
		}
		fmt.Fprintf(&b, "- 消息时间：%s", clockMs)
	}
	payload := map[string]any{}
	markdown["title"] = fmt.Sprintf("【%s】%s", alias, verb)
	markdown["text"] = b.String()
	payload["markdown"] = markdown
	payload["msgtype"] = "markdown"
	return payload
}

// monthOvertimeSecs sums overtime for the calendar month containing
// `day`, up to and including `now`. Returns false on missing stores.
func (s *Server) monthOvertimeSecs(mac string, day, now time.Time) (int64, bool) {
	if s.history == nil || s.settings == nil {
		return 0, false
	}
	cfg := s.settings.Get()
	monthStart := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, day.Location())
	monthEnd := monthStart.AddDate(0, 1, 0)
	from := monthStart.Add(-24 * time.Hour)
	to := monthEnd.Add(24 * time.Hour)
	entries, err := s.history.Query(mac, from, to)
	if err != nil {
		return 0, false
	}
	cap := monthEnd
	if now.Before(cap) {
		cap = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Add(24 * time.Hour)
	}
	var total int64
	for d := monthStart; d.Before(cap); d = d.AddDate(0, 0, 1) {
		var override Override
		if s.overrides != nil {
			if o, ok := s.overrides.Lookup(mac, d.Format("2006-01-02")); ok {
				override = o
			}
		}
		rep := ComputeWorktime(mac, d, cfg.WorkStart, cfg.WorkEnd, entries, now, override, s.dayKindFor(d))
		total += rep.OvertimeSecs
	}
	return total, true
}

// humanDuration renders seconds as "1h7m13s" / "45s" / "0s". Compact
// form to match the markdown spec (different from the dashboard's
// fully-spelled "1时7分13秒").
func humanDuration(secs int64) string {
	if secs <= 0 {
		return "0s"
	}
	h := secs / 3600
	m := (secs % 3600) / 60
	s := secs % 60
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s > 0 || b.Len() == 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	return b.String()
}
