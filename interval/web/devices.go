// devices.go — / (dashboard HTML), /favicon.ico, /api/devices.
//
// /api/devices snapshots Watcher.Known() merged with the offline cache
// from events.go (so devices that just dropped still appear, sorted to
// the bottom). Per-row enrichment (alias, is_me) is layered on here.

package web

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// handleIndex serves the embedded dashboard HTML at "/".
//
// We use no-cache + ETag instead of a long max-age: after self-upgrade
// the embedded HTML changes, ETag changes, browsers reload. The old
// max-age=300 silently served stale UI for 5 minutes after every
// upgrade.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", dashboardETag)
	if match := r.Header.Get("If-None-Match"); match != "" && match == dashboardETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(dashboardHTML)
}

// handleFavicon serves the embedded favicon. Stable across rebuilds,
// so a one-day cache is safe and stops the constant 304 chatter.
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(faviconICO)
}

// handleAppCSS serves the embedded dashboard stylesheet. Same caching
// posture as handleIndex: no-cache + ETag, so the browser keeps the
// file but revalidates on every load — after a release the ETag
// changes and the new CSS lands without a hard reload.
func (s *Server) handleAppCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", appCSSETag)
	if match := r.Header.Get("If-None-Match"); match != "" && match == appCSSETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(appCSS)
}

// handleAppModule serves any /assets/app/<name>.js from the embedded
// app/ directory. Used by ES Module imports in /assets/app/main.js
// (which is itself loaded with `<script type="module">` from
// dashboard.html). All modules share a single ETag (the bundle's
// rolled-up sha256) — re-deploying the binary invalidates them
// together, which matches the rebuild-everything reality.
func (s *Server) handleAppModule(w http.ResponseWriter, r *http.Request) {
	// Strip "/assets/" prefix → "app/<name>.js"; reject anything else.
	const prefix = "/assets/"
	if !strings.HasPrefix(r.URL.Path, prefix+"app/") {
		http.NotFound(w, r)
		return
	}
	name := r.URL.Path[len(prefix):] // "app/foo.js"
	// Reject path traversal — `embed.FS.ReadFile` does its own
	// validation but err on the side of caution.
	if strings.Contains(name, "..") || strings.Contains(name, "//") {
		http.NotFound(w, r)
		return
	}
	if !strings.HasSuffix(name, ".js") {
		http.NotFound(w, r)
		return
	}
	body, err := appModulesFS.ReadFile("assets/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", appModulesETag)
	if match := r.Header.Get("If-None-Match"); match != "" && match == appModulesETag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
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
