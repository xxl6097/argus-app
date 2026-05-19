// Package web — login / session / credentials wiring.
//
// This file is the single owner of the Web UI authentication state:
//   - Store   the on-disk admin user + bcrypt hash
//   - SessionStore       in-memory cookie → username map (TTL 24h)
//   - CookieName         the only Set-Cookie this app emits
//
// The middleware that consumes these (Server.requireAuth) lives in
// server.go because it touches the http.ServeMux and other Server
// state; everything else stays here.
package credentials

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// CookieName is the name of the session cookie. Same name across
// login / logout / requireAuth so we don't drift.
const CookieName = "argus_session"

// SessionTTL caps how long a successful login is valid. Long enough
// that a user doesn't re-auth daily; short enough that a stolen
// cookie has a finite blast radius.
const SessionTTL = 24 * time.Hour

// bcryptCost is the password hashing work factor. cost=10 takes ~100ms
// on a mid-range ARM64 router (MT7981); higher would make logins
// noticeably sluggish. The whole point of bcrypt is to be slow on
// purpose, but we still need users not to give up on the login page.
const bcryptCost = 10

// Credentials is the on-disk shape of credentials.json. We do NOT
// surface PasswordHash on any public method; consumers ask
// Store for the metadata (Username / MustChange) and feed
// candidate passwords back in to Verify.
type Credentials struct {
	Username     string `json:"username"`
	PasswordHash string `json:"password_hash"`
	MustChange   bool   `json:"must_change"`
	UpdatedAt    int64  `json:"updated_at"`
}

// Store is a single-account credential vault, mirroring
// SettingsStore's load-on-init + atomic-write pattern.
//
// First-boot semantics: if the file doesn't exist, we seed
// admin/admin and set MustChange=true so the dashboard can force a
// password change on first login. The hash is persisted so subsequent
// boots don't re-seed.
type Store struct {
	path string
	mu   sync.RWMutex
	data Credentials
}

// New loads (or creates) credentials.json at path.
// Passing path="" disables disk persistence entirely — the vault
// still works in-memory and seeds the default admin/admin, but no
// changes survive a restart. Used by tests; production should always
// pass a real path.
func New(path string) *Store {
	s := &Store{path: path}
	if !s.load() {
		s.seedDefault()
	}
	return s
}

// Reload re-reads credentials.json from disk, replacing the in-memory
// admin record. Used after backup import overwrites the file. If the
// new file is missing or corrupt, the in-memory state is left as-is
// (the caller should not have imported a missing credentials.json).
func (s *Store) Reload() bool {
	return s.load()
}

// load returns true if a usable credential record was read off disk.
func (s *Store) load() bool {
	if s.path == "" {
		return false
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return false
	}
	// Tighten perms on every load — older deployments may have written
	// 0644 by accident. Best-effort.
	_ = os.Chmod(s.path, 0o600)
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return false
	}
	if strings.TrimSpace(c.Username) == "" || c.PasswordHash == "" {
		return false
	}
	s.mu.Lock()
	s.data = c
	s.mu.Unlock()
	return true
}

// seedDefault writes admin/admin with MustChange=true. Logged once at
// startup by the caller (server.go) so the operator notices.
func (s *Store) seedDefault() {
	hash, err := bcrypt.GenerateFromPassword([]byte("admin"), bcryptCost)
	if err != nil {
		// Should be impossible (bcrypt only fails on invalid cost), but
		// rather than crash the whole router on a paranoid edge case,
		// fall back to a fixed but valid hash for "admin". Replaced on
		// first user-driven password change.
		hash = []byte("$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy") // bcrypt("admin")
	}
	c := Credentials{
		Username:     "admin",
		PasswordHash: string(hash),
		MustChange:   true,
		UpdatedAt:    time.Now().Unix(),
	}
	s.mu.Lock()
	s.data = c
	s.mu.Unlock()
	_ = s.persist()
}

// persist writes the current data to disk atomically (tmp + rename),
// 0600 so non-root users on the router can't read the bcrypt hash.
func (s *Store) persist() error {
	if s.path == "" {
		return nil
	}
	s.mu.RLock()
	b, err := json.MarshalIndent(s.data, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Username returns the configured admin user name.
func (s *Store) Username() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.Username
}

// MustChange reports whether the current password is the seeded
// default and the user has yet to rotate it. The login flow uses
// this to force a password change before issuing a session cookie.
func (s *Store) MustChange() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.data.MustChange
}

// Verify constant-time compares the candidate password against the
// stored bcrypt hash. Returns true only when both username and
// password match.
func (s *Store) Verify(username, password string) bool {
	s.mu.RLock()
	user := s.data.Username
	hash := s.data.PasswordHash
	s.mu.RUnlock()
	if !strings.EqualFold(strings.TrimSpace(username), user) {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// ChangePassword rotates the password after verifying the old one.
// Clears MustChange. Empty new password rejected; minimum length
// 6 chars (intentionally low for a single-user home setup — the
// real backstop is the LAN-only deployment).
func (s *Store) ChangePassword(oldPass, newPass string) error {
	if len(newPass) < 6 {
		return errors.New("credentials: new password must be at least 6 characters")
	}
	s.mu.RLock()
	hash := s.data.PasswordHash
	s.mu.RUnlock()
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(oldPass)) != nil {
		return errors.New("credentials: old password incorrect")
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcryptCost)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.data.PasswordHash = string(newHash)
	s.data.MustChange = false
	s.data.UpdatedAt = time.Now().Unix()
	s.mu.Unlock()
	return s.persist()
}

// -------- Session store --------

// SessionStore is an in-memory map from cookie token → username +
// expiry. Sessions die with the process; on restart users re-auth.
// We accept that tradeoff to avoid pulling in a DB dependency.
type SessionStore struct {
	mu    sync.Mutex
	byTok map[string]session
}

type session struct {
	username string
	expires  time.Time
}

// NewSessionStore returns an empty in-memory session store.
func NewSessionStore() *SessionStore {
	return &SessionStore{byTok: make(map[string]session)}
}

// Issue mints a fresh 32-byte token bound to username, valid for
// SessionTTL. Returns the opaque cookie value.
func (s *SessionStore) Issue(username string) (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byTok[tok] = session{
		username: username,
		expires:  time.Now().Add(SessionTTL),
	}
	s.gcLocked()
	return tok, nil
}

// Validate returns the username for a token if it exists and isn't
// expired. Empty string + false otherwise.
func (s *SessionStore) Validate(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byTok[tok]
	if !ok {
		return "", false
	}
	if time.Now().After(sess.expires) {
		delete(s.byTok, tok)
		return "", false
	}
	return sess.username, true
}

// Revoke explicitly drops a token (logout). Idempotent.
func (s *SessionStore) Revoke(tok string) {
	if tok == "" {
		return
	}
	s.mu.Lock()
	delete(s.byTok, tok)
	s.mu.Unlock()
}

// RevokeAll drops every active session. Used after a password change
// so all other browsers (e.g. on a forgotten device) get kicked out.
func (s *SessionStore) RevokeAll() {
	s.mu.Lock()
	s.byTok = make(map[string]session)
	s.mu.Unlock()
}

// gcLocked sweeps expired entries. Called opportunistically on
// every Issue so the map can't grow unboundedly.
func (s *SessionStore) gcLocked() {
	now := time.Now()
	for tok, sess := range s.byTok {
		if now.After(sess.expires) {
			delete(s.byTok, tok)
		}
	}
}
