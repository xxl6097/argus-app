// events.go — argus event lifecycle: OnEvent (the watcher entry
// point), OnSyslog (low-level syslog hint cache), the SSE fan-out
// stream at /api/events, and the offline-device cache used by
// handleDevices.
//
// OnEvent is the authoritative spot where everything synthesised
// from a single device transition gets sequenced:
//
//   1. updateOfflineCache  — keep "recently went offline" rows alive
//      so /api/devices can still surface them
//   2. history.Record       — persist the transition (when history is
//      enabled) so worktime + UI timeline see it
//   3. recordPunchCheckout  — write today's "out" to overrides.json
//      for punch devices that drop after WorkEnd
//   4. dispatchNotify       — webhook + ntfy
//   5. SSE fan-out          — every /api/events subscriber
//
// Order matters because dispatchNotify reads back the history we just
// recorded; see classifyPunchEvent for the slicing.

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	argus "github.com/xxl6097/argusd"
)

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

// syslogHintTTL caps how stale a syslog hint can be before OnEvent
// stops crediting it as the trigger of a fetcher-detected transition.
// Real wifi flows fire connect-side syslog (WPA_COMPLETE / WIFI_CONNECT
// / DHCP_ACK) within ~1s of the device showing up in poll results, so
// 8s gives slack without smearing into the next session.
const syslogHintTTL = 8 * time.Second

// offlineEntry stores the last-known Device shape at the moment it went
// offline, plus the time we observed the offline event. LastSeen on the
// Device itself is preserved from the library's point of view (wire
// format reports both as separate fields).
type offlineEntry struct {
	dev       argus.Device
	offlineAt time.Time
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
	// For punch devices that go offline at/after WorkEnd, persist the
	// last-seen time into overrides.json as the day's "out" — so the
	// worktime report reflects the latest checkout even if the user
	// hits the road and never comes back, and survives a reboot.
	s.recordPunchCheckout(e)
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

// handleEvents serves the /api/events SSE stream. Each connection
// registers a buffered channel with the server; OnEvent pushes events
// into every registered channel. Disconnects unregister automatically.
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
