// Package web exposes a built-in HTTP + Server-Sent Events (SSE)
// dashboard on top of an argus.Watcher.
//
// It is intentionally zero-dependency: the rendering is a single
// embedded HTML file with vanilla JS + EventSource; no build step, no
// framework, no external CDN. The handler set is split across a few
// files for readability:
//
//   - server.go (this file) — Options, the Server struct, NewServer,
//     ServeHTTP, the small shared helpers (writeJSONErr, util.NormalizeMAC,
//     util.NonZeroTime, dayKindFor)
//   - auth.go              — login session middleware + login/logout/
//     password endpoints + the LAN-auth predicate
//   - events.go            — OnEvent / OnSyslog / SSE stream / offline
//     cache
//   - devices.go           — / (dashboard HTML) + /api/devices
//   - handlers_aliases.go  — /api/aliases
//   - handlers_settings.go — /api/settings + /api/holidays
//   - handlers_worktime.go — /api/history + /api/worktime{,/month,
//     /override}
//   - handlers_notify.go   — /api/notifications{,/test,/messages}
//   - handlers_version.go  — /api/version{,/check} + /api/upgrade
//   - notify_dispatch.go   — internal: dispatchNotify +
//     formatNotifyMarkdown + classifyPunchEvent + recordPunchCheckout
//   - backup_handlers.go   — /api/backup/{export,import}
//   - kick.go              — /api/devices/kick
//
// The package is opt-in (no code in the core library changes); typical
// wiring in argusd:
//
//	srv := web.NewServer(w)
//	http.ListenAndServe("127.0.0.1:9099", srv)
//
// # Chinese · 中文
//
// web 子包提供基于 HTTP + SSE 的只读仪表板, 零依赖, 单文件 HTML,
// 直接嵌入二进制。默认监听 127.0.0.1:9099, 不带鉴权 (如需外网访问请
// 在反向代理层加认证)。
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xxl6097/argus-app/interval/owrt"
	"github.com/xxl6097/argus-app/interval/release"
	"github.com/xxl6097/argus-app/interval/store/alias"
	"github.com/xxl6097/argus-app/interval/store/credentials"
	"github.com/xxl6097/argus-app/interval/store/history"
	"github.com/xxl6097/argus-app/interval/store/holidays"
	"github.com/xxl6097/argus-app/interval/store/notify"
	"github.com/xxl6097/argus-app/interval/store/override"
	"github.com/xxl6097/argus-app/interval/store/settings"
	argus "github.com/xxl6097/argusd"
)

//go:embed assets/dashboard.html
var dashboardHTML []byte

//go:embed assets/app.css
var appCSS []byte

//go:embed assets/app
var appModulesFS embed.FS

//go:embed assets/favicon.ico
var faviconICO []byte

//go:embed assets/login.html
var loginHTML []byte

// Per-asset ETags, computed once at process start. Each one is stable
// within a binary's lifetime and changes only when that specific file
// is rebuilt, so browsers re-download the file that actually changed
// instead of all three on every release. JS modules under assets/app/
// share a single ETag (app.js bundle ID): any release that rebuilds
// the binary changes its sha256, but switching one module without
// touching others is rare enough that finer-grained ETags aren't
// worth the bookkeeping.
var (
	dashboardETag  = computeETag(dashboardHTML)
	appCSSETag     = computeETag(appCSS)
	appModulesETag = computeAppModulesETag()
)

// computeAppModulesETag walks the embedded app/ tree and produces a
// single SHA256 across all JS bytes — same shape as computeETag.
func computeAppModulesETag() string {
	h := sha256.New()
	entries, err := appModulesFS.ReadDir("assets/app")
	if err != nil {
		return `"app-unknown"`
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		b, err := appModulesFS.ReadFile("assets/app/" + e.Name())
		if err != nil {
			continue
		}
		_, _ = h.Write([]byte(e.Name()))
		_, _ = h.Write(b)
	}
	sum := h.Sum(nil)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

func computeETag(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// Defaults for the offline device cache. override.Override via Option at
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
func WithAliases(store *alias.Store) Option {
	return func(s *Server) { s.aliases = store }
}

// AuthCheck decides whether an incoming HTTP request may mutate state
// (currently: the alias write APIs). Return true to allow, false to
// reject with 403. See [WithWriteAuth] for the default policy.
type AuthCheck func(r *http.Request) bool

// WithWriteAuth replaces the default write-API auth predicate. The
// default (defaultLANAuth) is a noop — every authenticated request
// is allowed. Pre-v0.1.20 the default was an RFC1918/ULA filter,
// which is preserved as defaultLANAuth1 if you want to layer it back
// in.
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
func WithDHCPManager(m owrt.DHCPManager) Option {
	return func(s *Server) { s.dhcp = m }
}

// WithHistory attaches a per-MAC online/offline history store. When
// set, /api/history and /api/worktime are enabled and OnEvent records
// Online/Offline transitions. nil is the default (feature disabled).
func WithHistory(h *history.Store) Option {
	return func(s *Server) { s.history = h }
}

// WithSettings attaches a persistent settings store (worktime window,
// "me" MAC). Enables /api/settings when non-nil.
func WithSettings(st *settings.Store) Option {
	return func(s *Server) { s.settings = st }
}

// WithOverrides attaches a per-(mac, date) manual in/out override
// store. Used for worktime days where the Watcher missed transitions.
// Enables /api/worktime/override when non-nil.
func WithOverrides(o *override.Store) Option {
	return func(s *Server) { s.overrides = o }
}

// WithHolidays attaches a legal-holiday / 调休-workday store so the
// worktime compute can treat weekends and public holidays as
// overtime days. When nil, only the weekday-vs-weekend heuristic
// applies (Saturday/Sunday always = OT day).
func WithHolidays(h *holidays.Store) Option {
	return func(s *Server) { s.holidays = h }
}

// WithCredentials enables Web UI login. When set, every route except
// the login page itself + favicon requires a valid session cookie;
// requests without one are redirected to /login (HTML clients) or
// returned 401 (JSON / SSE clients). Pass nil to disable the login
// gate entirely (default).
func WithCredentials(store *credentials.Store) Option {
	return func(s *Server) {
		s.creds = store
		if store != nil && s.sessions == nil {
			s.sessions = credentials.NewSessionStore()
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
func WithVersion(v release.VersionInfo, repo string) Option {
	return func(s *Server) {
		s.version = v
		if repo != "" {
			s.versionSvc = release.NewVersionService(repo)
		}
	}
}

// WithNotifications attaches a per-device notification store + the
// dispatcher that fans events out to webhook + ntfy.  When set,
// OnEvent fires webhooks and ntfy publishes, and /api/notifications
// / /api/notifications/messages endpoints are served.
func WithNotifications(store *notify.Store, notifier *notify.Notifier) Option {
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
	aliases *alias.Store

	// dhcp is an optional router-specific manager for static DHCP
	// reservations. Exposes /api/dhcp when non-nil; the dashboard
	// hides the "set static IP" UI when nil.
	dhcp owrt.DHCPManager

	// history persists per-MAC ONLINE/OFFLINE transitions for the
	// expandable-row timeline and the worktime report. nil disables
	// both /api/history and /api/worktime.
	history *history.Store

	// settings is the user-configurable "me MAC + workday window"
	// store. nil disables /api/settings and /api/worktime.
	settings *settings.Store

	// overrides is the per-(mac, date) manual in/out store. Lets the
	// user fix worktime days where the Watcher missed transitions.
	// nil disables /api/worktime/override.
	overrides *override.Store

	// holidays tags dates as legal holidays / 调休 workdays so
	// the worktime compute can special-case them as OT days.
	// nil = weekday/weekend heuristic only.
	holidays *holidays.Store

	// notifyStore is the per-device webhook + ntfy config store.
	// notifier dispatches events to those destinations.
	notifyStore *notify.Store
	notifier    *notify.Notifier

	// writeAuth gates mutating APIs (POST/DELETE /api/aliases). nil
	// means the default (currently a noop pass-through).
	writeAuth AuthCheck

	// creds + sessions implement the Web UI login gate. When creds is
	// nil, requireAuth() short-circuits to "always allowed" — that's
	// the legacy / dev path where the dashboard is exposed unauthenticated.
	// When creds is non-nil, every route except /login + /api/login +
	// /favicon.ico requires a valid session cookie.
	creds    *credentials.Store
	sessions *credentials.SessionStore

	// version is the build-stamped identity of this binary, surfaced
	// at /api/version. versionSvc handles GitHub probing + cache and
	// drives the "check for upgrades" flow when non-nil.
	version    release.VersionInfo
	versionSvc *release.VersionService

	// syslogHints is a per-MAC short-lived cache of the most recent
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
	// CSS / JS for the dashboard. Same logic as favicon — these are
	// loaded by the dashboard.html itself before any cookie is checked
	// in some browsers, and serving them 401 would also break the
	// /login page if it grew styled assets.
	s.mux.HandleFunc("/assets/app.css", s.handleAppCSS)
	s.mux.HandleFunc("/assets/app/", s.handleAppModule)

	// Everything else goes through requireAuth. When creds == nil
	// requireAuth is a pass-through, so the legacy "no login gate"
	// behaviour is preserved.
	gate := s.requireAuth
	s.mux.HandleFunc("/", gate(s.handleIndex))
	s.mux.HandleFunc("/api/devices", gate(s.handleDevices))
	s.mux.HandleFunc("/api/devices/kick", gate(s.handleDeviceKick))
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
func (s *Server) History() *history.Store { return s.history }

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

// dayKindFor returns the holidays.DayKind for t, consulting the HolidayStore
// when attached and falling back to weekday/weekend detection.
func (s *Server) dayKindFor(t time.Time) holidays.DayKind {
	if s.holidays != nil {
		return s.holidays.Kind(t)
	}
	wd := t.Weekday()
	if wd == time.Saturday || wd == time.Sunday {
		return holidays.DayKindWeekend
	}
	return holidays.DayKindWorkday
}

// --- Small shared helpers used across the handler files ---
//
// MAC / time normalisation lives in interval/util now; this file
// keeps only the HTTP-shaped helpers.

func writeJSONErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
