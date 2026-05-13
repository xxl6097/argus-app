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
	_ "embed"
	"encoding/json"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	argus "github.com/xxl6097/argusd"
)

//go:embed assets/dashboard.html
var dashboardHTML []byte

// Defaults for the offline device cache. Override via Option at
// construction time (see NewServer).
const (
	defaultOfflineRetention = 7 * 24 * time.Hour
	defaultOfflineMax       = 512
)

// Option configures a Server at construction time.
type Option func(*Server)

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

	// writeAuth gates mutating APIs (POST/DELETE /api/aliases). nil
	// means the default LAN policy (loopback + RFC1918).
	writeAuth AuthCheck
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
		watcher:    w,
		mux:        http.NewServeMux(),
		subs:       make(map[chan argus.Event]struct{}),
		offline:    make(map[string]offlineEntry),
		offlineTTL: defaultOfflineRetention,
		offlineMax: defaultOfflineMax,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.writeAuth == nil {
		s.writeAuth = defaultLANAuth
	}
	s.mux.HandleFunc("/", s.handleIndex)
	s.mux.HandleFunc("/api/devices", s.handleDevices)
	s.mux.HandleFunc("/api/events", s.handleEvents)
	s.mux.HandleFunc("/api/aliases", s.handleAliases)
	s.mux.HandleFunc("/api/dhcp", s.handleDHCP)
	s.mux.HandleFunc("/api/system/reboot", s.handleReboot)
	s.mux.HandleFunc("/api/system/restart-network", s.handleRestartNetwork)
	s.mux.HandleFunc("/api/history", s.handleHistory)
	s.mux.HandleFunc("/api/worktime", s.handleWorktime)
	s.mux.HandleFunc("/api/worktime/month", s.handleWorktimeMonth)
	s.mux.HandleFunc("/api/worktime/override", s.handleWorktimeOverride)
	s.mux.HandleFunc("/api/settings", s.handleSettings)
	s.mux.HandleFunc("/api/holidays", s.handleHolidays)
	return s
}

// History returns the attached HistoryStore, or nil when history
// is disabled. Useful for embedders that want to seed baseline
// ONLINE entries on startup.
func (s *Server) History() *HistoryStore { return s.history }

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
		s.history.Record(e)
	}

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
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Conservative cache: the dashboard HTML is embedded in the binary,
	// so a redeploy is required to change it. 5-min cache is fine.
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(dashboardHTML)
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
	IsMe        bool   `json:"is_me,omitempty"`         // tagged as "my phone" for worktime
}

// applyAlias annotates the row with a user-defined friendly name when
// one is configured. Returns the row unchanged if no alias store is
// attached or the MAC has no entry. Also tags the row as IsMe when
// the MAC matches the currently-selected "me" MAC in settings.
func (s *Server) applyAlias(row deviceRow) deviceRow {
	if s.aliases != nil {
		if name := s.aliases.Lookup(row.MAC); name != "" {
			row.Alias = name
		}
	}
	if s.settings != nil {
		if me := s.settings.Get().MeMAC; me != "" && me == normalizeMAC(row.MAC) {
			row.IsMe = true
		}
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
		"aliases":   s.aliases != nil,
		"dhcp":      s.dhcp != nil,
		"history":   s.history != nil,
		"worktime":  s.history != nil && s.settings != nil,
		"overrides": s.overrides != nil,
		"settings":  s.settings != nil,
		"holidays":  s.holidays != nil,
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
// loopback or an RFC1918 private network — appropriate for a dashboard
// bound to a home LAN. X-Forwarded-For is NOT consulted: if you front
// Argus with a reverse proxy, supply your own AuthCheck.
func defaultLANAuth(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	return isRFC1918(ip)
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
//	GET    /api/settings              -> {"me_mac": "...", "work_start": "09:00", "work_end": "18:30"}
//	POST   /api/settings  {me_mac, work_start, work_end}  -> {"ok": true}
//	DELETE /api/settings?clear=me     -> {"ok": true}  (clears me_mac only)
//
// POST is gated by writeAuth. GET is public (read-only).
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
			"me_mac":     strings.ToUpper(cfg.MeMAC),
			"work_start": cfg.WorkStart,
			"work_end":   cfg.WorkEnd,
		})
	case http.MethodPost:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		var in Settings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := s.settings.Update(in); err != nil {
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
		clear := r.URL.Query().Get("clear")
		if clear == "me" {
			if err := s.settings.ClearMe(); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		} else {
			writeJSONErr(w, http.StatusBadRequest, "clear query parameter must be 'me'")
		}
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

