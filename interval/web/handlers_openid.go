// handlers_openid.go — /api/openids CRUD + /api/login/openid passwordless
// login.
//
// /api/openids manages a server-side whitelist; only logged-in admins
// can read/edit it. /api/login/openid is the public counterpart —
// any caller who knows a whitelisted openid trades it for an admin
// session cookie. The openid IS the credential, so the whitelist
// file is 0600 and admins should rotate entries the same way they'd
// rotate a password.
//
// Both POST and GET are accepted on /api/login/openid:
//   - POST {"openid": "..."} for fetch-driven flows (preferred —
//     openid stays out of URLs and proxy logs).
//   - GET ?openid=... for one-shot redirect flows (WeChat menu /
//     QR code / iframe-launched). On match we 302 to ?next= or "/"
//     and the cookie ride-along; on miss we 302 to /login?error=...
//     so the browser still lands somewhere usable.

package web

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/xxl6097/argus-app/interval/store/credentials"
)

// handleOpenIDs multiplexes GET / POST / DELETE on /api/openids.
//
//	GET    /api/openids                 -> {"openids": ["wx_aaa", "wx_bbb"]}
//	POST   /api/openids   {openid}      -> {"ok": true, "openids": [...]}
//	DELETE /api/openids?openid=wx_aaa   -> {"ok": true, "openids": [...]}
//	DELETE /api/openids?clear=1         -> wipe the entire list
//
// POST / DELETE are gated by writeAuth.
func (s *Server) handleOpenIDs(w http.ResponseWriter, r *http.Request) {
	if s.openids == nil {
		writeJSONErr(w, http.StatusServiceUnavailable, "openids not enabled")
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"openids": s.openids.All()})
	case http.MethodPost:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		var in struct {
			OpenID string `json:"openid"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid json body")
			return
		}
		if err := s.openids.Add(in.OpenID); err != nil {
			writeJSONErr(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"openids": s.openids.All(),
		})
	case http.MethodDelete:
		if !s.writeAuth(r) {
			writeJSONErr(w, http.StatusForbidden, "write denied by auth policy")
			return
		}
		if r.URL.Query().Get("clear") == "1" {
			if err := s.openids.Clear(); err != nil {
				writeJSONErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "openids": []string{}})
			return
		}
		oid := r.URL.Query().Get("openid")
		if oid == "" {
			writeJSONErr(w, http.StatusBadRequest, "openid query parameter required (or ?clear=1)")
			return
		}
		if err := s.openids.Remove(oid); err != nil {
			writeJSONErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"openids": s.openids.All(),
		})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// handleAPILoginOpenID exchanges a whitelisted openid for an admin
// session cookie.
//
//	POST /api/login/openid  {openid}   -> {"ok":true} + cookie
//	GET  /api/login/openid?openid=...  -> 302 to ?next= or "/" with cookie
//
// On miss: POST returns 401 JSON; GET 302s to /login?error=invalid_openid
// (so the browser ends up somewhere useful even from a webview launch).
//
// Public route — no auth required. The openid IS the credential.
func (s *Server) handleAPILoginOpenID(w http.ResponseWriter, r *http.Request) {
	if s.openids == nil || s.creds == nil || s.sessions == nil {
		// Match POST/GET-shape failure consistently: GET callers get a
		// redirect so they don't see a raw JSON body, POST callers get
		// the 503 directly.
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/login?error=openid_disabled", http.StatusFound)
			return
		}
		writeJSONErr(w, http.StatusServiceUnavailable, "openid login not enabled")
		return
	}

	var oid string
	switch r.Method {
	case http.MethodGet:
		oid = strings.TrimSpace(r.URL.Query().Get("openid"))
	case http.MethodPost:
		var in struct {
			OpenID string `json:"openid"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, "invalid json body")
			return
		}
		oid = strings.TrimSpace(in.OpenID)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if oid == "" {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/login?error=missing_openid", http.StatusFound)
			return
		}
		writeJSONErr(w, http.StatusBadRequest, "openid required")
		return
	}
	if !s.openids.Has(oid) {
		// Generic message — don't echo the openid back, don't differentiate
		// "wrong" from "expired". Same shape as /api/login on bad password.
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/login?error=invalid_openid", http.StatusFound)
			return
		}
		writeJSONErr(w, http.StatusUnauthorized, "openid not whitelisted")
		return
	}

	tok, err := s.sessions.Issue(s.creds.Username())
	if err != nil {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, "/login?error=session", http.StatusFound)
			return
		}
		writeJSONErr(w, http.StatusInternalServerError, "issue session: "+err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     credentials.CookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	if r.Method == http.MethodGet {
		http.Redirect(w, r, nextOrRoot(r), http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
