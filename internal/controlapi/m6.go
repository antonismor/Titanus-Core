package controlapi

import (
	"context"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/observe"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"net/http"
	"sort"
	"strings"
	"time"
)

type KeyReadiness struct {
	NodeID       string            `json:"node_id"`
	Realm        string            `json:"realm"`
	Fingerprints map[string]string `json:"fingerprints"`
}
type M6Client interface {
	KeyReadiness(string) (KeyReadiness, error)
	NodeObservation(string) (observe.NodeObservation, error)
}

func (s *Server) registerM6(mux *http.ServeMux) {
	mux.HandleFunc("/v1/realm/task-schedules", s.authorize(s.taskSchedules))
	mux.HandleFunc("/v1/realm/task-schedules/", s.authorize(s.taskScheduleObject))
	mux.HandleFunc("/v1/realm/secret-rotation", s.authorize(s.secretRotation))
	mux.HandleFunc("/v1/node/secret-keyring", s.authorize(s.keyReadiness))
	mux.HandleFunc("/v1/node/observation", s.authorize(s.nodeObservation))
	mux.HandleFunc("/v1/realm/observations", s.authorize(s.centralObservations))
	mux.HandleFunc("/v1/realm/alerts", s.authorize(s.centralAlerts))
}
func (s *Server) taskSchedules(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		writeJSON(w, 200, s.Store.Snapshot().TaskSchedules)
	case "POST":
		var j realm.TaskSchedule
		if e := decodeJSON(r, &j); e != nil {
			writeError(w, 400, e)
			return
		}
		if e := s.Store.CreateTaskSchedule(j); e != nil {
			writeError(w, 409, e)
			return
		}
		writeJSON(w, 201, s.Store.Snapshot().TaskSchedules[j.Name])
	default:
		methodNotAllowed(w)
	}
}
func (s *Server) taskScheduleObject(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/v1/realm/task-schedules/")
	if r.Method == "POST" && strings.HasSuffix(name, "/pause") {
		if e := s.Store.PauseTaskSchedule(strings.TrimSuffix(name, "/pause")); e != nil {
			writeError(w, 409, e)
			return
		}
		writeJSON(w, 200, map[string]bool{"paused": true})
		return
	}
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	j, ok := s.Store.Snapshot().TaskSchedules[name]
	if !ok {
		writeError(w, 404, fmt.Errorf("unknown schedule"))
		return
	}
	writeJSON(w, 200, j)
}
func (s *Server) keyReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	keys, e := secrets.Load(s.SecretKeyring)
	if e != nil {
		writeError(w, 503, e)
		return
	}
	writeJSON(w, 200, KeyReadiness{s.NodeID, s.Store.Snapshot().Name, keys.Fingerprints()})
}
func (s *Server) originalPeers(state realm.State) (map[string]string, error) {
	peers := map[string]string{s.NodeID: ""}
	if s.NodeID == "" || (s.Consensus != nil && len(s.SchemaPeers) != 3 && len(s.SchemaPeers) != 5) {
		return nil, fmt.Errorf("original controller identities required")
	}
	for _, p := range s.SchemaPeers {
		if p.ID != s.NodeID {
			peers[p.ID] = p.API
		}
	}
	for id, n := range state.Nodes {
		if _, exists := peers[id]; !exists {
			peers[id] = n.Address
		}
	}
	return peers, nil
}
func (s *Server) secretRotation(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		writeJSON(w, 200, s.Store.Snapshot().SecretRotations)
		return
	}
	if r.Method != "POST" {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ID               string `json:"id"`
		Target           string `json:"target"`
		ExpectedRevision uint64 `json:"expected_revision"`
	}
	if e := decodeJSON(r, &req); e != nil {
		writeError(w, 400, e)
		return
	}
	state := s.Store.Snapshot()
	for _, rotation := range state.SecretRotations {
		if rotation.ID == req.ID && rotation.Target == req.Target {
			writeJSON(w, 200, rotation)
			return
		}
	}
	if state.SchemaVersion < 2 || state.Revision != req.ExpectedRevision {
		writeError(w, 409, fmt.Errorf("schema-two exact revision required"))
		return
	}
	keys, e := secrets.Load(s.SecretKeyring)
	if e != nil {
		writeError(w, 503, e)
		return
	}
	required := map[string]string{req.Target: keys.Fingerprints()[req.Target]}
	for _, versions := range state.Secrets {
		for _, v := range versions {
			if len(v.Ciphertext) > 0 {
				required[v.KeyID] = keys.Fingerprints()[v.KeyID]
			}
		}
	}
	for _, digest := range required {
		if digest == "" {
			writeError(w, 409, fmt.Errorf("complete source/target keyring required"))
			return
		}
	}
	peers, e := s.originalPeers(state)
	if e != nil {
		writeError(w, 409, e)
		return
	}
	for id, address := range peers {
		if id == s.NodeID {
			continue
		}
		if s.M6Client == nil || address == "" {
			writeError(w, 409, fmt.Errorf("all nodes must acknowledge encryption readiness"))
			return
		}
		ready, e := s.M6Client.KeyReadiness(address)
		if e != nil || ready.NodeID != id || ready.Realm != state.Name {
			writeError(w, 409, fmt.Errorf("verified original node key readiness required"))
			return
		}
		for key, digest := range required {
			if ready.Fingerprints[key] != digest {
				writeError(w, 409, fmt.Errorf("node key fingerprint mismatch"))
				return
			}
		}
	}
	result, e := s.Store.RotateSecrets(req.ID, req.Target, req.ExpectedRevision, keys)
	if e != nil {
		writeError(w, 409, e)
		return
	}
	writeJSON(w, 200, result)
}
func (s *Server) localObservation() observe.NodeObservation {
	d := s.collectDiagnostics()
	events := []observe.Event{}
	incomplete := len(d.Incomplete) > 0
	if s.Observations != nil {
		var e error
		events, e = s.Observations.Tail()
		incomplete = incomplete || e != nil
	} else {
		incomplete = true
	}
	return observe.NodeObservation{NodeID: s.NodeID, Realm: s.Store.Snapshot().Name, Time: time.Now().UTC(), Events: events, Incomplete: incomplete, LogFailures: d.LogSinkFailures, AuditFailures: d.ObservationFailures}
}
func (s *Server) nodeObservation(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	writeJSON(w, 200, s.localObservation())
}
func (s *Server) CollectClusterObservations() error {
	if s.Central == nil {
		return fmt.Errorf("central recorder unavailable")
	}
	if e := s.Store.CheckLeader(); e != nil {
		return e
	}
	state := s.Store.Snapshot()
	peers, e := s.originalPeers(state)
	if e != nil {
		return e
	}
	ids := []string{}
	for id := range peers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	observations := map[string]observe.NodeObservation{}
	deadline := time.Now().Add(15 * time.Second)
	for i, id := range ids {
		if i >= 64 || time.Now().After(deadline) {
			break
		}
		if e := s.Store.CheckLeader(); e != nil {
			return e
		}
		if id == s.NodeID {
			observations[id] = s.localObservation()
			continue
		}
		if s.M6Client == nil {
			continue
		}
		o, e := s.M6Client.NodeObservation(peers[id])
		if e == nil && o.NodeID == id && o.Realm == state.Name {
			observations[id] = o
		}
	}
	if e := s.Store.CheckLeader(); e != nil {
		return e
	}
	return s.Central.Save(ids, observations, time.Now().UTC())
}
func (s *Server) MaintainObservations(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		_ = s.CollectClusterObservations()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (s *Server) centralObservations(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	if s.Central == nil {
		writeError(w, 503, fmt.Errorf("central recorder unavailable"))
		return
	}
	events, e := s.Central.Tail()
	if e != nil {
		writeError(w, 503, e)
		return
	}
	writeJSON(w, 200, events)
}
func (s *Server) centralAlerts(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	if s.Central == nil {
		writeError(w, 503, fmt.Errorf("central recorder unavailable"))
		return
	}
	status, e := s.Central.Status()
	if e != nil {
		writeError(w, 503, e)
		return
	}
	if status.Time.IsZero() || time.Since(status.Time) > 45*time.Second {
		status.Alerts = append(status.Alerts, observe.Alert{Code: "collection.stale"})
	}
	for name, t := range s.Store.Snapshot().Tasks {
		if t.Phase == realm.TaskUnknown {
			if len(status.Alerts) >= 256 {
				break
			}
			status.Alerts = append(status.Alerts, observe.Alert{NodeHash: observe.TargetHash(name), Code: "task.unknown"})
		}
	}
	code := 200
	if status.Collected != status.Expected || status.Time.IsZero() || time.Since(status.Time) > 45*time.Second {
		code = 503
	}
	writeJSON(w, code, status)
}
