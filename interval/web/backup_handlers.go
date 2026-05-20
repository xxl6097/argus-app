package web

import (
	"encoding/json"
	"fmt"
	"github.com/xxl6097/argus-app/interval/release"
	"net/http"
	"time"
)

// handleBackupExport streams a gzipped tar of the configured data
// directory. Requires auth + writeAuth (the bundle includes secrets,
// so even a "read-only" download is treated like a privileged op).
//
//	GET /api/backup/export
//	→ Content-Type: application/gzip
//	  Content-Disposition: attachment; filename="argus-app-backup-YYYYMMDD-HHMMSS.tar.gz"
func (s *Server) handleBackupExport(w http.ResponseWriter, r *http.Request) {
	if s.dataDir == "" {
		writeJSONErr(w, http.StatusServiceUnavailable, "backup disabled (data-dir not configured)")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.writeAuth(r) {
		writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
		return
	}
	host := s.version.Version
	if host == "" {
		host = "dev"
	}
	fname := fmt.Sprintf("argus-app-backup-%s.tar.gz", time.Now().UTC().Format("20060102-150405"))
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)
	w.Header().Set("Cache-Control", "no-store")
	// Streaming: we don't know the final size up front, so no
	// Content-Length. Browsers handle this fine for downloads.
	if _, err := release.PackDataDir(s.dataDir, host, w); err != nil {
		// Headers are already flushed by the time pack starts writing,
		// so we can't switch to a JSON error. Best we can do is abort
		// the connection so the client sees a truncated download.
		// Log via fmt would be noise; the panic-style halt isn't right
		// either. Just return — the partial tar will fail to extract.
		return
	}
}

// handleBackupImport accepts a multipart upload of a previously
// exported tar.gz, validates it, swaps the live data dir for the
// extracted contents, and reloads the in-memory stores. Requires
// auth + writeAuth.
//
//	POST /api/backup/import
//	  multipart/form-data:
//	    file (required)               — the .tar.gz body
//	    restore_credentials (optional) — "true"/"false" (default true)
//
//	→ 200 {"ok":true, "restored":[...], "skipped":[...],
//	       "backup_dir":"/etc/argus-app.bak.20260519-..."
//	       "session_revoked": true|false}
func (s *Server) handleBackupImport(w http.ResponseWriter, r *http.Request) {
	if s.dataDir == "" {
		writeJSONErr(w, http.StatusServiceUnavailable, "backup disabled (data-dir not configured)")
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

	// 32 MiB upload cap — well above any realistic argus-app data dir.
	r.Body = http.MaxBytesReader(w, r.Body, release.MaxImportArchiveBytes+1024)
	if err := r.ParseMultipartForm(release.MaxImportArchiveBytes); err != nil {
		writeJSONErr(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, "missing 'file' field")
		return
	}
	defer file.Close()
	_ = header // not currently used; we trust the manifest, not the filename

	restoreCreds := true
	if v := r.FormValue("restore_credentials"); v != "" {
		switch v {
		case "0", "false", "no", "off":
			restoreCreds = false
		}
	}

	res, err := release.ImportBackup(s.dataDir, file, restoreCreds)
	if err != nil {
		writeJSONErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// Reload in-memory stores so the new files take effect without
	// a process restart. Order-independent.
	if s.aliases != nil {
		s.aliases.Reload()
	}
	if s.settings != nil {
		s.settings.Reload()
	}
	if s.notifyStore != nil {
		s.notifyStore.Reload()
		// Ntfy subscribers re-keyed too — without this, old subs keep
		// running against the previous topics until next config write.
		s.refreshNotifySubs()
	}
	if s.overrides != nil {
		s.overrides.Reload()
	}
	if s.holidays != nil {
		s.holidays.Reload()
	}

	// Credentials reload + session revocation: only when the user
	// actually opted to restore creds (the file's been replaced) AND
	// the file made it into the archive (skipped is empty for that
	// name).
	sessionRevoked := false
	if restoreCreds && s.creds != nil {
		s.creds.Reload()
		if s.sessions != nil {
			s.sessions.RevokeAll()
			sessionRevoked = true
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":              true,
		"restored":        res.Restored,
		"skipped":         res.Skipped,
		"manifest":        res.Manifest,
		"session_revoked": sessionRevoked,
	})
}
