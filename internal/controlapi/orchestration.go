package controlapi

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"net/http"
	"strings"
)

func (s *Server) registerOrchestration(mux *http.ServeMux) {
	mux.HandleFunc("/v1/realm/tasks", s.authorize(s.tasks))
	mux.HandleFunc("/v1/realm/tasks/", s.authorize(s.taskObject))
	mux.HandleFunc("/v1/realm/autoscalers", s.authorize(s.autoscalers))
	mux.HandleFunc("/v1/realm/autoscalers/", s.authorize(s.autoscalerObject))
	mux.HandleFunc("/v1/realm/secrets", s.authorize(s.secretObjects))
	mux.HandleFunc("/v1/realm/secrets/", s.authorize(s.secretObject))
	s.registerCommandCenter(mux)
}
func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeError(w, 503, fmt.Errorf("Realm unavailable"))
		return
	}
	switch r.Method {
	case "GET":
		items := []realm.Task{}
		for _, t := range s.Store.Snapshot().Tasks {
			t.LeaseToken = ""
			items = append(items, t)
		}
		writeJSON(w, 200, items)
	case "POST":
		var t realm.Task
		if e := decodeJSON(r, &t); e != nil {
			writeError(w, 400, e)
			return
		}
		if e := s.Store.CreateTask(t); e != nil {
			writeError(w, 409, e)
			return
		}
		stored := s.Store.Snapshot().Tasks[t.Name]
		writeJSON(w, 201, stored)
	default:
		methodNotAllowed(w)
	}
}
func (s *Server) taskObject(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/v1/realm/tasks/")
	if strings.HasSuffix(name, "/cancel") {
		name = strings.TrimSuffix(name, "/cancel")
		if r.Method != "POST" {
			methodNotAllowed(w)
			return
		}
		if e := s.Store.CancelTask(name); e != nil {
			writeError(w, 409, e)
			return
		}
		writeJSON(w, 202, map[string]bool{"cancel_requested": true})
		return
	}
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	t, ok := s.Store.Snapshot().Tasks[name]
	if !ok {
		writeError(w, 404, fmt.Errorf("unknown Task"))
		return
	}
	t.LeaseToken = ""
	writeJSON(w, 200, t)
}
func (s *Server) autoscalers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		items := []realm.Autoscaler{}
		for _, a := range s.Store.Snapshot().Autoscalers {
			items = append(items, a)
		}
		writeJSON(w, 200, items)
	case "POST":
		var a realm.Autoscaler
		if e := decodeJSON(r, &a); e != nil {
			writeError(w, 400, e)
			return
		}
		if e := s.Store.PutAutoscaler(a); e != nil {
			writeError(w, 409, e)
			return
		}
		writeJSON(w, 201, s.Store.Snapshot().Autoscalers[a.Fleet])
	default:
		methodNotAllowed(w)
	}
}
func (s *Server) autoscalerObject(w http.ResponseWriter, r *http.Request) {
	if r.Method != "DELETE" {
		methodNotAllowed(w)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/v1/realm/autoscalers/")
	if e := s.Store.DeleteAutoscaler(name); e != nil {
		writeError(w, 409, e)
		return
	}
	writeJSON(w, 200, map[string]bool{"deleted": true})
}

type secretMetadata struct {
	Name    string `json:"name"`
	Version uint64 `json:"version"`
	KeyID   string `json:"key_id"`
}

func (s *Server) secretObjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		methodNotAllowed(w)
		return
	}
	items := []secretMetadata{}
	for _, versions := range s.Store.Snapshot().Secrets {
		for _, v := range versions {
			if len(v.Ciphertext) > 0 {
				items = append(items, secretMetadata{v.Name, v.Version, v.KeyID})
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, items)
}
func (s *Server) secretObject(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/v1/realm/secrets/")
	if !secrets.Name.MatchString(name) {
		writeError(w, 400, fmt.Errorf("invalid secret name"))
		return
	}
	switch r.Method {
	case "PUT":
		var req struct {
			Value []byte `json:"value"`
		}
		if e := decodeJSON(r, &req); e != nil {
			writeError(w, 400, fmt.Errorf("invalid secret payload"))
			return
		}
		defer clear(req.Value)
		keys, e := secrets.Load(s.SecretKeyring)
		if e != nil {
			writeError(w, 503, e)
			return
		}
		state := s.Store.Snapshot()
		v := uint64(len(state.Secrets[name]) + 1)
		record, e := keys.Encrypt(state.Name, name, v, req.Value)
		if e != nil {
			writeError(w, 400, e)
			return
		}
		if e = s.Store.PutSecret(record); e != nil {
			writeError(w, 409, e)
			return
		}
		writeJSON(w, 201, secretMetadata{name, v, record.KeyID})
	case "DELETE":
		if e := s.Store.DeleteSecret(name); e != nil {
			writeError(w, 409, e)
			return
		}
		writeJSON(w, 200, map[string]bool{"deleted": true})
	default:
		methodNotAllowed(w)
	}
}
