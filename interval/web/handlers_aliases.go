// handlers_aliases.go — /api/aliases CRUD on the MAC → name map.
//
// All paths return JSON. Mutating methods (POST/DELETE) are gated by
// s.writeAuth in addition to requireAuth.

package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"github.com/xxl6097/argus-app/interval/util"
)

// handleAliases multiplexes GET / POST / DELETE on /api/aliases.
//
//	GET    /api/aliases                -> {"aliases": {MAC: name, ...}}
//	POST   /api/aliases  {mac, name}   -> {"ok": true, "mac": MAC, "name": N}
//	                                      empty name deletes the alias
//	DELETE /api/aliases?mac=AA:BB:...  -> {"ok": true}
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
		"mac":  strings.ToUpper(util.NormalizeMAC(in.MAC)),
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
