package controlapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
)

type Server struct {
	Store   *realm.Store
	Runtime *unitruntime.Manager
	Sources *source.Manager
}

type PulseRequest struct {
	NodeID    string          `json:"node_id"`
	Resources realm.Resources `json:"resources"`
}

func New(store *realm.Store, runtime *unitruntime.Manager, sources *source.Manager) *Server {
	return &Server{Store: store, Runtime: runtime, Sources: sources}
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/realm/state", s.state)
	mux.HandleFunc("/v1/realm/nodes", s.nodes)
	mux.HandleFunc("/v1/realm/pulse", s.pulse)
	mux.HandleFunc("/v1/realm/fleets", s.fleets)
	mux.HandleFunc("/v1/node/units", s.units)
	mux.HandleFunc("/v1/node/units/", s.unitAction)
	mux.HandleFunc("/v1/node/sources", s.sources)
	mux.HandleFunc("/v1/node/sources/", s.sourceObject)
}

func (s *Server) sources(w http.ResponseWriter, r *http.Request) {
	if s.Sources == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("Source manager unavailable"))
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	items, err := s.Sources.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func (s *Server) sourceObject(w http.ResponseWriter, r *http.Request) {
	if s.Sources == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("Source manager unavailable"))
		return
	}
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/node/sources/"), "/")
	if name == "" || strings.Contains(name, "/") {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid Source name"))
		return
	}
	switch r.Method {
	case http.MethodHead:
		exists, err := s.Sources.Exists(name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if !exists {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case http.MethodGet:
		exists, err := s.Sources.Exists(name)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if !exists {
			writeError(w, http.StatusNotFound, fmt.Errorf("Source %s not found", name))
			return
		}
		w.Header().Set("Content-Type", "application/vnd.titanus.source+gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".titanus"))
		if err := s.Sources.Export(name, w); err != nil {
			// Headers may already be committed; connection termination still
			// causes the receiving integrity verification to fail safely.
			return
		}
	case http.MethodPut:
		manifest, err := s.Sources.ImportBundle(io.LimitReader(r.Body, 64<<30))
		if err != nil {
			if strings.Contains(err.Error(), "already exists") {
				exists, existsErr := s.Sources.Exists(name)
				if existsErr == nil && exists {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if manifest.Name != name {
			writeError(w, http.StatusBadRequest, fmt.Errorf("bundle Source %q does not match URL %q", manifest.Name, name))
			return
		}
		writeJSON(w, http.StatusCreated, manifest)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) units(w http.ResponseWriter, r *http.Request) {
	if s.Runtime == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("Unit runtime unavailable"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		items, err := s.Runtime.List()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var spec unitruntime.Spec
		if err := decodeJSON(r, &spec); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		state, err := s.Runtime.Ensure(spec)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, state)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) unitAction(w http.ResponseWriter, r *http.Request) {
	if s.Runtime == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("Unit runtime unavailable"))
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/node/units/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("Unit ID is required"))
		return
	}
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	if r.Method == http.MethodGet && action == "" {
		spec, state, err := s.Runtime.Inspect(id)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"spec": spec, "state": state})
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		methodNotAllowed(w)
		return
	}
	switch action {
	case "start":
		state, err := s.Runtime.Start(id)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, state)
	case "stop":
		state, err := s.Runtime.Stop(id, 10*time.Second)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, state)
	case "":
		if r.Method != http.MethodDelete {
			methodNotAllowed(w)
			return
		}
		if err := s.Runtime.Delete(id); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	default:
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown Unit action %q", action))
	}
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, http.StatusOK, s.Store.Snapshot())
}

func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, realm.SortedNodes(s.Store.Snapshot()))
	case http.MethodPost:
		var node realm.Node
		if err := decodeJSON(r, &node); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := authorizeNode(r, node.ID); err != nil {
			writeError(w, http.StatusForbidden, err)
			return
		}
		if err := s.Store.UpsertNode(node); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, node)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) pulse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req PulseRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := authorizeNode(r, req.NodeID); err != nil {
		writeError(w, http.StatusForbidden, err)
		return
	}
	if err := s.Store.Pulse(req.NodeID, req.Resources); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": true, "revision": s.Store.Snapshot().Revision})
}

func (s *Server) fleets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		state := s.Store.Snapshot()
		items := make([]realm.Fleet, 0, len(state.Fleets))
		for _, fleet := range state.Fleets {
			items = append(items, fleet)
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var fleet realm.Fleet
		if err := decodeJSON(r, &fleet); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := s.Store.PutFleet(fleet); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		state := s.Store.Snapshot()
		fleet = state.Fleets[fleet.Name]
		assignments, err := realm.NewPlacementEngine().Plan(state, fleet)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		if err := s.Store.SetAssignments(fleet.Name, assignments); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"fleet": fleet, "assignments": assignments, "revision": s.Store.Snapshot().Revision,
		})
	default:
		methodNotAllowed(w)
	}
}

func authorizeNode(r *http.Request, nodeID string) error {
	if strings.TrimSpace(nodeID) == "" {
		return fmt.Errorf("node ID is required")
	}
	// Unix-socket requests are local privileged management requests.
	if r.TLS == nil {
		return nil
	}
	if len(r.TLS.PeerCertificates) == 0 {
		return fmt.Errorf("missing Titanus node certificate")
	}
	identity := r.TLS.PeerCertificates[0].Subject.CommonName
	if identity != nodeID {
		return fmt.Errorf("certificate identity %q cannot act as Node %q", identity, nodeID)
	}
	return nil
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
}
