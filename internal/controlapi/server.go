package controlapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/antonismor/Titanus-Core/internal/realm"
)

type Server struct {
	Store *realm.Store
}

type PulseRequest struct {
	NodeID    string          `json:"node_id"`
	Resources realm.Resources `json:"resources"`
}

func New(store *realm.Store) *Server {
	return &Server{Store: store}
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/v1/realm/state", s.state)
	mux.HandleFunc("/v1/realm/nodes", s.nodes)
	mux.HandleFunc("/v1/realm/pulse", s.pulse)
	mux.HandleFunc("/v1/realm/fleets", s.fleets)
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
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
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
