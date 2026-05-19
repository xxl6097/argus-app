// auth.go — login gate (cookie session) + write-API LAN auth predicate
// + login/logout/password endpoints.
//
// Two layers, applied independently:
//
//   1. requireAuth — wraps every non-public route. Validates a session
//      cookie when WithCredentials is configured; redirects browsers to
//      /login and returns 401 to API/SSE clients otherwise. Pass-through
//      when credentials aren't configured (legacy "no login" mode).
//
//   2. defaultLANAuth — the fallback write-auth predicate gating
//      mutating endpoints (POST/DELETE on settings, aliases, etc.).
//      Currently a noop (returns true); identity is the session cookie
//      from layer 1, so a second LAN-IP filter would just reject VPN
//      and IPv6-link-local clients without adding any real protection.
//      The original RFC1918/ULA-aware predicate is preserved here as
//      defaultLANAuth1 for callers that want to opt back in via
//      WithWriteAuth.

package web

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"
	"github.com/xxl6097/argus-app/interval/store/credentials"
)

// defaultLANAuth1 keeps the previous "loopback or private network"
// behaviour around for sites that want to layer it back in via
// WithWriteAuth. Allows IPv4 RFC1918, IPv6 ULA (fc00::/7), IPv6
// link-local (fe80::/10), and IPv4-mapped IPv6 with the embedded v4
// in RFC1918. Strips IPv6 zone IDs (fe80::1%br-lan) before parsing.
func defaultLANAuth1(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
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
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.To4() != nil {
		return isRFC1918(ip)
	}
	return isPrivateV6(ip)
}

// defaultLANAuth is the predicate used when WithWriteAuth isn't
// supplied. Currently lets every authenticated request through —
// see the package doc comment for rationale.
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
		c, err := r.Cookie(credentials.CookieName)
		if err == nil {
			if user, ok := s.sessions.Validate(c.Value); ok {
				ctx := contextWithUser(r.Context(), user)
				next(w, r.WithContext(ctx))
				return
			}
		}
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

// handleLogin serves the login page (GET) — a single embedded HTML
// document that posts to /api/login. Public route.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.creds == nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if c, err := r.Cookie(credentials.CookieName); err == nil {
		if _, ok := s.sessions.Validate(c.Value); ok {
			http.Redirect(w, r, nextOrRoot(r), http.StatusFound)
			return
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(loginHTML)
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
		writeJSONErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}
	tok, err := s.sessions.Issue(s.creds.Username())
	if err != nil {
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
	if c, err := r.Cookie(credentials.CookieName); err == nil && s.sessions != nil {
		s.sessions.Revoke(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     credentials.CookieName,
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
	s.sessions.RevokeAll()
	tok, err := s.sessions.Issue(s.creds.Username())
	if err != nil {
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
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
