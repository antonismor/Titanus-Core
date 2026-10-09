package controlapi

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/version"
)

type CompatibilityInfo struct {
	NodeID       string               `json:"node_id"`
	Realm        string               `json:"realm"`
	Schema       int                  `json:"schema"`
	Capabilities version.Capabilities `json:"capabilities"`
}
type SchemaRequest struct {
	Target           int    `json:"target,omitempty"`
	ID               string `json:"id"`
	ExpectedRevision uint64 `json:"expected_revision"`
}
type SchemaPeer struct {
	ID  string
	API string
}
type TransitionClient interface {
	Compatibility(address string) (CompatibilityInfo, error)
	PrepareSchemaFloor(address string, floor offline.SchemaFloor) error
}

func (s *Server) compatibility(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	state := s.Store.Snapshot()
	writeJSON(w, http.StatusOK, CompatibilityInfo{s.NodeID, state.Name, state.SchemaVersion, version.Compatible()})
}
func (s *Server) schemaFloor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var floor offline.SchemaFloor
	if e := decodeJSON(r, &floor); e != nil {
		writeError(w, http.StatusBadRequest, e)
		return
	}
	if s.StateRoot == "" || floor.Realm != s.Store.Snapshot().Name {
		writeError(w, http.StatusConflict, fmt.Errorf("schema floor Realm/host mismatch"))
		return
	}
	if e := offline.PrepareSchemaFloor(s.StateRoot, floor); e != nil {
		writeError(w, http.StatusConflict, e)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"prepared": true})
}
func (s *Server) schemaTransition(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, s.Store.Snapshot().SchemaMigrations)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req SchemaRequest
	if e := decodeJSON(r, &req); e != nil {
		writeError(w, http.StatusBadRequest, e)
		return
	}
	state := s.Store.Snapshot()
	if req.Target == 0 {
		req.Target = 1
	}
	// An uncertain committed request is resolved by identity, never applied twice.
	for _, m := range state.SchemaMigrations {
		if m.ID == req.ID && m.To == req.Target {
			writeJSON(w, http.StatusOK, m)
			return
		}
	}
	if req.Target != state.SchemaVersion+1 || req.Target > version.MaxSchema || state.Revision != req.ExpectedRevision || s.StateRoot == "" || s.NodeID == "" {
		writeError(w, http.StatusConflict, fmt.Errorf("unsupported migration or stale revision/host identity"))
		return
	}
	peers := map[string]string{s.NodeID: ""}
	if s.Consensus != nil && len(s.SchemaPeers) != 3 && len(s.SchemaPeers) != 5 {
		writeError(w, http.StatusConflict, fmt.Errorf("original controller membership required"))
		return
	}
	for _, p := range s.SchemaPeers {
		if p.ID == s.NodeID {
			continue
		}
		peers[p.ID] = p.API
	}
	for id, n := range state.Nodes {
		if _, ok := peers[id]; !ok {
			peers[id] = n.Address
		}
	}
	required := []string{}
	observations := map[string]version.Capabilities{}
	for id, address := range peers {
		required = append(required, id)
		if id == s.NodeID {
			observations[id] = version.Compatible()
			continue
		}
		if s.TransitionClient == nil || address == "" {
			writeError(w, http.StatusConflict, fmt.Errorf("node %s is unreachable for schema preparation", id))
			return
		}
		info, e := s.TransitionClient.Compatibility(address)
		if e != nil || info.NodeID != id || info.Realm != state.Name || !info.Capabilities.Supports(req.Target) {
			writeError(w, http.StatusConflict, fmt.Errorf("node %s has not advertised verified target-schema support: %v", id, e))
			return
		}
		observations[id] = info.Capabilities
	}
	for id, n := range state.Nodes {
		if !version.Admits(n.Compatibility, req.Target) {
			writeError(w, http.StatusConflict, fmt.Errorf("registered node %s agent is not upgraded", id))
			return
		}
	}
	sort.Strings(required)
	floor := offline.NewSchemaFloor(state.Name, req.ID, req.Target)
	if e := floor.Validate(); e != nil {
		writeError(w, http.StatusBadRequest, e)
		return
	}
	// Every original voter plus every registered node must acknowledge durable
	// preparation. A failed partial prepare leaves floors in place and allows the
	// same ID to resume; it never authorizes an incompatible binary rollback.
	for _, id := range required {
		var e error
		if id == s.NodeID {
			e = offline.PrepareSchemaFloor(s.StateRoot, floor)
		} else {
			e = s.TransitionClient.PrepareSchemaFloor(peers[id], floor)
		}
		if e != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("prepare node %s: %w", id, e))
			return
		}
	}
	m, e := s.Store.TransitionSchemaTo(req.ID, req.ExpectedRevision, req.Target, observations, required)
	if e != nil {
		writeError(w, http.StatusConflict, e)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// Admission is kept at schema zero during controller-first binary upgrade.
// Missing capabilities identify an unadvertised legacy node only; schema-one
// placement and registration require explicit compatible advertisements.
