package controlapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/lease"
	"github.com/antonismor/Titanus-Core/internal/observe"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
)

type LeaderGate interface {
	CheckLeader() error
	LeaderAPI() string
	Status() map[string]string
}

type Server struct {
	SecretKeyring string
	Observations  *observe.Recorder
	Disks         *disk.Manager
	Consensus     LeaderGate
	Store         *realm.Store
	Runtime       *unitruntime.Manager
	Sources       *source.Manager
	Leases        *lease.Manager
	CAPath        string
	Authority     *identity.Authority
}

type PulseRequest struct {
	NodeID    string          `json:"node_id"`
	Resources realm.Resources `json:"resources"`
}

func New(store *realm.Store, runtime *unitruntime.Manager, sources *source.Manager, leases *lease.Manager) *Server {
	return &Server{Store: store, Runtime: runtime, Sources: sources, Leases: leases}
}

func (s *Server) Register(mux *http.ServeMux) {
	s.registerStorage(mux)
	s.registerOrchestration(mux)
	mux.HandleFunc("/v1/metrics", s.authorize(s.metrics))
	mux.HandleFunc("/v1/diagnostics", s.authorize(s.diagnostics))
	mux.HandleFunc("/v1/events", s.authorize(s.events))
	mux.HandleFunc("/v1/node/disks", s.authorizeDisk(s.disks))
	mux.HandleFunc("/v1/node/disks/", s.authorizeDisk(s.diskAction))
	mux.HandleFunc("/v1/realm/consensus", s.authorize(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return
		}
		if s.Consensus == nil {
			writeJSON(w, http.StatusOK, map[string]string{"state": "standalone"})
			return
		}
		writeJSON(w, http.StatusOK, s.Consensus.Status())
	}))
	mux.HandleFunc("/v1/identity/crl", s.authorize(s.certificateRevocations))
	mux.HandleFunc("/v1/identity/renew", s.authorize(s.certificateRenewal))
	mux.HandleFunc("/v1/realm/state", s.authorize(s.state))
	mux.HandleFunc("/v1/realm/nodes", s.authorize(s.nodes))
	mux.HandleFunc("/v1/realm/pulse", s.authorize(s.pulse))
	mux.HandleFunc("/v1/realm/fleets", s.authorize(s.fleets))
	mux.HandleFunc("/v1/realm/fleets/", s.authorize(s.fleetObject))
	mux.HandleFunc("/v1/realm/routes", s.authorize(s.routes))
	mux.HandleFunc("/v1/realm/routes/", s.authorize(s.routeObject))
	mux.HandleFunc("/v1/realm/policies", s.authorize(s.policies))
	mux.HandleFunc("/v1/realm/policies/", s.authorize(s.policyObject))
	mux.HandleFunc("/v1/node/units", s.authorize(s.units))
	mux.HandleFunc("/v1/node/units/", s.authorize(s.unitAction))
	mux.HandleFunc("/v1/node/sources", s.authorize(s.sources))
	mux.HandleFunc("/v1/node/sources/", s.authorize(s.sourceObject))
	mux.HandleFunc("/v1/node/leases/", s.authorize(s.leaseObject))
}

type LeaseRequest struct {
	Token      string `json:"token"`
	TTLSeconds int    `json:"ttl_seconds"`
}

func (s *Server) leaseObject(w http.ResponseWriter, r *http.Request) {
	if s.Leases == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("lease watchdog unavailable"))
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/node/leases/"), "/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid Unit lease ID"))
		return
	}
	switch r.Method {
	case http.MethodPost:
		var req LeaseRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		record, err := s.Leases.Renew(id, req.Token, time.Duration(req.TTLSeconds)*time.Second)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, record)
	case http.MethodGet:
		record, ok := s.Leases.Inspect(id)
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("no live lease for Unit %s", id))
			return
		}
		writeJSON(w, http.StatusOK, record)
	case http.MethodDelete:
		token := r.URL.Query().Get("token")
		if err := s.Leases.Revoke(id, token); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"revoked": true})
	default:
		methodNotAllowed(w)
	}
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
	if r.Method == http.MethodGet && action == "usage" && len(parts) == 2 {
		u, err := s.Runtime.Usage(id)
		if err != nil {
			writeError(w, 409, fmt.Errorf("live cgroup measurements unavailable"))
			return
		}
		writeJSON(w, 200, u)
		return
	}
	if r.Method == http.MethodGet && action == "logs" && len(parts) == 2 {
		p, ok := identity.RequestPrincipal(r)
		if !ok || p.Role != identity.RoleAdmin {
			writeError(w, 403, fmt.Errorf("workload logs require admin role"))
			return
		}
		data, err := s.Runtime.ReadLogs(id)
		if err != nil {
			writeError(w, 503, fmt.Errorf("workload logs unavailable"))
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(data)
		return
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
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		if principal, ok := identity.RequestPrincipal(r); ok && principal.Role == identity.RoleController {
			if s.Leases == nil {
				writeError(w, http.StatusServiceUnavailable, fmt.Errorf("lease watchdog unavailable"))
				return
			}
			record, exists := s.Leases.Inspect(id)
			if !exists || !time.Now().Before(record.ExpiresAt) {
				writeError(w, http.StatusConflict, fmt.Errorf("live lease required before controller start"))
				return
			}
		}
		var state unitruntime.State
		var err error
		if principal, ok := identity.RequestPrincipal(r); ok && principal.Role == identity.RoleController {
			state, err = s.Runtime.EnsureRunning(id)
		} else {
			state, err = s.Runtime.Start(id)
		}
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, state)
	case "stop":
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
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
	state := s.Store.Snapshot()
	if principal, ok := identity.RequestPrincipal(r); ok && principal.Role == identity.RoleNode {
		state.StorageFailovers = nil
		state.UnitMappings = nil
		state.Disks = nil
		state.Secrets = nil
		for name, t := range state.Tasks {
			t.Template.Environment = nil
			t.LeaseToken = ""
			state.Tasks[name] = t
		}
		for name, fleet := range state.Fleets {
			fleet.Template.Environment = nil
			for i := range fleet.History {
				fleet.History[i].Template.Environment = nil
			}
			state.Fleets[name] = fleet
		}
		for id, assignment := range state.Assignments {
			assignment.StorageWriters = nil
			assignment.LeaseToken = ""
			state.Assignments[id] = assignment
		}
	}
	writeJSON(w, http.StatusOK, state)
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
		if principal, ok := identity.RequestPrincipal(r); ok && principal.Role == identity.RoleNode {
			for _, capability := range node.Capabilities {
				if string(capability) == "CONTROL" {
					writeError(w, http.StatusForbidden, fmt.Errorf("Node role cannot claim CONTROL capability"))
					return
				}
			}
		}
		if err := s.Store.UpsertNode(node); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		stored := s.Store.Snapshot().Nodes[node.ID]
		writeJSON(w, http.StatusCreated, stored)
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

func (s *Server) policies(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		state := s.Store.Snapshot()
		items := make([]realm.NetworkPolicy, 0, len(state.Policies))
		for _, policy := range state.Policies {
			items = append(items, policy)
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var policy realm.NetworkPolicy
		if err := decodeJSON(r, &policy); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		stored, err := s.Store.PutPolicy(policy)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, stored)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) policyObject(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/realm/policies/"), "/")
	if name == "" || strings.Contains(name, "/") {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid Network Policy name"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		policy, ok := s.Store.GetPolicy(name)
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("Network Policy %s not found", name))
			return
		}
		writeJSON(w, http.StatusOK, policy)
	case http.MethodDelete:
		if err := s.Store.DeletePolicy(name); err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) routes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		state := s.Store.Snapshot()
		items := make([]realm.Route, 0, len(state.Routes))
		for _, route := range state.Routes {
			items = append(items, route)
		}
		writeJSON(w, http.StatusOK, items)
	case http.MethodPost:
		var route realm.Route
		if err := decodeJSON(r, &route); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		stored, err := s.Store.PutRoute(route)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusCreated, stored)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) routeObject(w http.ResponseWriter, r *http.Request) {
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/realm/routes/"), "/")
	if name == "" || strings.Contains(name, "/") {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid Route name"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		route, ok := s.Store.GetRoute(name)
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("Route %s not found", name))
			return
		}
		writeJSON(w, http.StatusOK, route)
	case http.MethodDelete:
		if err := s.Store.DeleteRoute(name); err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	default:
		methodNotAllowed(w)
	}
}

type ScaleFleetRequest struct {
	Instances int `json:"instances"`
}

func (s *Server) fleetObject(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/realm/fleets/"), "/")
	parts := strings.Split(path, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("Fleet name is required"))
		return
	}
	name := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}
	switch {
	case r.Method == http.MethodGet && action == "":
		fleet, ok := s.Store.GetFleet(name)
		if !ok {
			writeError(w, http.StatusNotFound, fmt.Errorf("Fleet %s not found", name))
			return
		}
		state := s.Store.Snapshot()
		assignments := make([]realm.Assignment, 0)
		for _, assignment := range state.Assignments {
			if assignment.Fleet == name {
				assignments = append(assignments, assignment)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"fleet": fleet, "assignments": assignments, "rollout": realm.NewPlacementEngine().Rolling(state, fleet, time.Now().UTC())})
	case r.Method == http.MethodPost && action == "scale":
		var req ScaleFleetRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		fleet, err := s.Store.ScaleFleet(name, req.Instances)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, fleet)
	case r.Method == http.MethodPost && action == "rollback":
		var req struct {
			Generation uint64 `json:"generation"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		fleet, err := s.Store.RollbackFleet(name, req.Generation)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, http.StatusOK, fleet)
	case r.Method == http.MethodDelete && action == "":
		if err := s.Store.DeleteFleet(name); err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	default:
		methodNotAllowed(w)
	}
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
		assignments := make([]realm.Assignment, 0)
		for _, a := range state.Assignments {
			if a.Fleet == fleet.Name {
				assignments = append(assignments, a)
			}
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
	principal, ok := identity.RequestPrincipal(r)
	if !ok {
		return fmt.Errorf("authenticated identity required")
	}
	if principal.Role == identity.RoleAdmin {
		return nil
	}
	if principal.ID != nodeID {
		return fmt.Errorf("certificate identity cannot act as another Node")
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

func (s *Server) authorize(next http.HandlerFunc) http.HandlerFunc {
	return s.trace(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := identity.RequestPrincipal(r)
		if !ok || !identity.Allowed(principal, r.Method, r.URL.Path) {
			writeError(w, http.StatusForbidden, fmt.Errorf("role does not permit this operation"))
			return
		}
		if s.Consensus != nil && strings.HasPrefix(r.URL.Path, "/v1/realm/") && r.URL.Path != "/v1/realm/consensus" {
			if err := s.Consensus.CheckLeader(); err != nil {
				w.Header().Set("X-Titanus-Rejected", "true")
				w.Header().Set("X-Titanus-Leader", s.Consensus.LeaderAPI())
				writeError(w, http.StatusServiceUnavailable, err)
				return
			}
		}
		next(w, r)
	})
}
