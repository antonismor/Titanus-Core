package controlapi

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/observe"
)

type responseStatus struct {
	http.ResponseWriter
	status int
}

func (w *responseStatus) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseStatus) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(p)
}
func (w *responseStatus) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) trace(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Observations == nil {
			next(w, r)
			return
		}
		start := time.Now()
		op := observe.Operation(r.URL.Path)
		principal, _ := identity.RequestPrincipal(r)
		request, err := observe.NewRequestID()
		if err != nil {
			writeError(w, 503, fmt.Errorf("audit request ID unavailable"))
			return
		}
		e := observe.Event{Kind: "api.result", Request: request, Actor: principal.ID, Role: string(principal.Role), Method: observe.Method(r.Method), Operation: op, TargetHash: observe.TargetHash(r.URL.Path)}
		mutation := r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions
		if mutation {
			e.Kind = "api.intent"
			if err = s.Observations.Record(e); err != nil {
				writeError(w, 503, fmt.Errorf("durable audit unavailable; operation rejected"))
				return
			}
		}
		w.Header().Set("X-Titanus-Request-ID", request)
		status := &responseStatus{ResponseWriter: w}
		defer func() {
			aborted := recover()
			code := status.status
			if code == 0 || aborted != nil {
				code = 500
			}
			e.Kind = "api.result"
			if aborted != nil {
				e.Kind = "api.aborted"
			}
			e.Status = code
			_ = s.Observations.Record(e)
			s.Observations.Request(r.Method, op, code, time.Since(start))
			if aborted != nil {
				panic(aborted)
			}
		}()
		next(status, r)
	}
}

type diagnosticState struct {
	Time                time.Time         `json:"time"`
	RealmRevision       uint64            `json:"realm_revision"`
	Nodes               map[string]int    `json:"nodes"`
	Assignments         map[string]int    `json:"assignments"`
	Units               map[string]int    `json:"units"`
	Disks               map[string]int    `json:"disks"`
	ReadyUnits          int               `json:"ready_units"`
	LiveUnits           int               `json:"live_units"`
	Restarts            int               `json:"restarts"`
	LogSinkFailures     int               `json:"log_sink_failures"`
	Consensus           map[string]string `json:"consensus"`
	Incomplete          []string          `json:"incomplete"`
	ObservationFailures uint64            `json:"observation_failures"`
}

func (s *Server) collectDiagnostics() diagnosticState {
	d := diagnosticState{Time: time.Now().UTC(), Nodes: map[string]int{}, Assignments: map[string]int{}, Units: map[string]int{}, Disks: map[string]int{}, Incomplete: []string{}, Consensus: map[string]string{"state": "standalone"}}
	if s.Store == nil {
		d.Incomplete = append(d.Incomplete, "realm")
	} else {
		snapshot := s.Store.Snapshot()
		d.RealmRevision = snapshot.Revision
		for _, n := range snapshot.Nodes {
			d.Nodes[string(n.State)]++
		}
		for _, a := range snapshot.Assignments {
			d.Assignments[string(a.State)]++
		}
	}
	if s.Runtime == nil {
		d.Incomplete = append(d.Incomplete, "runtime")
	} else {
		states, err := s.Runtime.List()
		if err != nil {
			d.Incomplete = append(d.Incomplete, "runtime")
		} else {
			for _, u := range states {
				if s.Runtime.LogSinkFailed(u.ID) {
					d.LogSinkFailures++
				}
				d.Units[string(u.Status)]++
				if u.Ready {
					d.ReadyUnits++
				}
				if u.Live {
					d.LiveUnits++
				}
				d.Restarts += u.RestartCount
			}
		}
	}
	if s.Disks == nil {
		d.Incomplete = append(d.Incomplete, "disks")
	} else {
		disks, err := s.Disks.List()
		if err != nil {
			d.Incomplete = append(d.Incomplete, "disks")
		} else {
			for _, disk := range disks {
				d.Disks[string(disk.Provider)]++
			}
		}
	}
	if s.Consensus != nil { // Do not copy endpoints, identities or error strings.
		status := s.Consensus.Status()
		d.Consensus = map[string]string{}
		for _, key := range []string{"state", "role"} {
			if value, ok := status[key]; ok {
				d.Consensus[key] = value
			}
		}
	}
	if s.Observations != nil {
		d.ObservationFailures = s.Observations.Failures()
	}
	return d
}

func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	d := s.collectDiagnostics()
	code := 200
	if len(d.Incomplete) > 0 {
		code = 503
	}
	writeJSON(w, code, d)
}
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.Observations == nil {
		writeError(w, 503, fmt.Errorf("observation journal unavailable"))
		return
	}
	events, err := s.Observations.Tail()
	if err != nil {
		writeError(w, 503, fmt.Errorf("observation journal unreadable"))
		return
	}
	writeJSON(w, 200, events)
}
func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if s.Observations == nil {
		writeError(w, 503, fmt.Errorf("metrics unavailable"))
		return
	}
	d := s.collectDiagnostics()
	var b strings.Builder
	b.WriteString(s.Observations.Metrics())
	fmt.Fprintf(&b, "# TYPE titanus_realm_revision gauge\ntitanus_realm_revision %d\n# TYPE titanus_diagnostics_complete gauge\ntitanus_diagnostics_complete %d\n", d.RealmRevision, boolNumber(len(d.Incomplete) == 0))
	for kind, states := range map[string]map[string]int{"nodes": d.Nodes, "assignments": d.Assignments, "units": d.Units, "disks": d.Disks} {
		fmt.Fprintf(&b, "# TYPE titanus_%s gauge\n", kind)
		for state, count := range states {
			fmt.Fprintf(&b, "titanus_%s{state=%q} %d\n", kind, state, count)
		}
	}
	fmt.Fprintf(&b, "# TYPE titanus_units_ready gauge\ntitanus_units_ready %d\n# TYPE titanus_units_live gauge\ntitanus_units_live %d\n# TYPE titanus_unit_restarts gauge\ntitanus_unit_restarts %d\n", d.ReadyUnits, d.LiveUnits, d.Restarts)
	fmt.Fprintf(&b, "# TYPE titanus_log_sink_failures gauge\ntitanus_log_sink_failures %d\n", d.LogSinkFailures)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	fmt.Fprint(w, b.String())
}
func boolNumber(value bool) int {
	if value {
		return 1
	}
	return 0
}
