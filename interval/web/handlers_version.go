// handlers_version.go — version endpoints + self-upgrade trigger.
//
// /api/version returns the build identity stamped onto the binary at
// link time; /api/version/check probes GitHub Releases (cached for
// 30 min) so the dashboard's version pill can flag updates;
// /api/upgrade spawns a detached install.sh that swaps the binary
// and restarts the procd service.

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"github.com/xxl6097/argus-app/interval/release"
)

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
		"has_update":  release.HasUpdate(s.version.Version, rel.TagName),
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
	if err := release.TriggerUpgrade(target); err != nil {
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
