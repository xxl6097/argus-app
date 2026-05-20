// Package openid stores a whitelist of OpenID strings that grant
// passwordless login.
//
// Use case: WeChat / 二维码 / iframe 引导式登录 — caller sends the
// pre-issued openid in the URL or a POST body, the server checks
// membership in this set, and (on hit) issues a regular admin
// session cookie. The list is admin-managed via /api/openids and
// stored in a JSON array on disk.
//
// File format (JSON array, one entry per openid):
//
//	[
//	  "wx_oxQ7BvE1abcd",
//	  "wx_oxQ7Bvxyz1234"
//	]
//
// File permissions: 0600 (the openid IS the credential — anyone who
// can read it can log in).
//
// Concurrency: many readers, serialised writes. Persistence is atomic
// (write-tmp + rename) so a mid-write crash leaves the prior file
// intact.
package openid

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/xxl6097/argus-app/interval/util"
)

// MaxLen caps a single openid; refuses absurd input that would bloat
// the JSON file. WeChat openids are 28 chars, this leaves headroom
// for 自有/oauth issuers.
const MaxLen = 128

// Store holds the in-memory whitelist with disk persistence.
type Store struct {
	path string

	mu   sync.RWMutex
	data map[string]struct{} // openid -> present
}

// New constructs a store backed by the given file path. Empty path =
// in-memory only. Missing / corrupt files are treated as empty.
func New(path string) *Store {
	s := &Store{path: path, data: make(map[string]struct{})}
	s.load()
	return s
}

// Reload re-reads the file from disk, replacing the in-memory set.
// Used after backup import overwrites the JSON file.
func (s *Store) Reload() {
	s.mu.Lock()
	s.data = make(map[string]struct{})
	s.mu.Unlock()
	s.load()
}

func (s *Store) load() {
	if s.path == "" {
		return
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		return
	}
	out := make(map[string]struct{}, len(list))
	for _, v := range list {
		v = strings.TrimSpace(v)
		if v == "" || len(v) > MaxLen {
			continue
		}
		out[v] = struct{}{}
	}
	s.mu.Lock()
	s.data = out
	s.mu.Unlock()
}

// Has reports whether the given openid is in the whitelist. Trims
// surrounding whitespace; case-sensitive (openids from real OAuth
// issuers are case-sensitive).
func (s *Store) Has(openid string) bool {
	openid = strings.TrimSpace(openid)
	if openid == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.data[openid]
	return ok
}

// All returns a sorted snapshot of every openid. The returned slice
// is independent of the store's internal state.
func (s *Store) All() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.data))
	for v := range s.data {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Add inserts an openid into the whitelist. No-op if already present.
// Returns an error on empty input, oversize input, or persistence
// failure.
func (s *Store) Add(openid string) error {
	openid = strings.TrimSpace(openid)
	if openid == "" {
		return errors.New("openid: openid required")
	}
	if len(openid) > MaxLen {
		return fmt.Errorf("openid: too long (%d > %d)", len(openid), MaxLen)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[openid]; ok {
		return nil
	}
	s.data[openid] = struct{}{}
	return s.persistLocked()
}

// Remove deletes an openid from the whitelist. No-op if absent.
func (s *Store) Remove(openid string) error {
	openid = strings.TrimSpace(openid)
	if openid == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[openid]; !ok {
		return nil
	}
	delete(s.data, openid)
	return s.persistLocked()
}

// Clear empties the whitelist.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = make(map[string]struct{})
	return s.persistLocked()
}

func (s *Store) persistLocked() error {
	if s.path == "" {
		return nil
	}
	out := make([]string, 0, len(s.data))
	for v := range s.data {
		out = append(out, v)
	}
	sort.Strings(out)
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return util.WriteJSONAtomic(s.path, b, 0o600)
}
