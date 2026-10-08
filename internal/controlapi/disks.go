package controlapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/identity"
)

func (s *Server) disks(w http.ResponseWriter, r *http.Request) {
	if s.Disks == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("Disk manager unavailable"))
		return
	}
	if r.Method == http.MethodGet {
		items, err := s.Disks.List()
		if err != nil {
			writeError(w, 500, err)
			return
		}
		writeJSON(w, 200, items)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var spec disk.Spec
	if err := decodeJSON(r, &spec); err != nil {
		writeError(w, 400, err)
		return
	}
	result, err := s.Disks.Create(spec)
	if err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 201, result)
}
func (s *Server) diskAction(w http.ResponseWriter, r *http.Request) {
	if s.Disks == nil {
		writeError(w, 503, fmt.Errorf("Disk manager unavailable"))
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/node/disks/"), "/")
	if len(parts) > 2 || parts[0] == "" {
		writeError(w, 400, fmt.Errorf("invalid Disk action"))
		return
	}
	name := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	switch {
	case r.Method == http.MethodGet && action == "":
		spec, err := s.Disks.Inspect(name)
		if err != nil {
			writeError(w, 404, err)
			return
		}
		writeJSON(w, 200, spec)
	case r.Method == http.MethodGet && action == "snapshots":
		items, err := s.Disks.Snapshots(name)
		if err != nil {
			writeError(w, 409, err)
			return
		}
		writeJSON(w, 200, items)
	case r.Method == http.MethodPost && action == "snapshot":
		var req struct {
			Name string `json:"name"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, 400, err)
			return
		}
		result, err := s.Disks.Snapshot(name, req.Name)
		if err != nil {
			writeError(w, 409, err)
			return
		}
		writeJSON(w, 201, result)
	case r.Method == http.MethodPost && action == "restore":
		var req struct {
			Snapshot string `json:"snapshot"`
			Target   string `json:"target"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, 400, err)
			return
		}
		result, err := s.Disks.Restore(name, req.Snapshot, req.Target)
		if err != nil {
			writeError(w, 409, err)
			return
		}
		writeJSON(w, 201, result)
	case r.Method == http.MethodPost && action == "owner":
		var req struct {
			UID int `json:"uid"`
			GID int `json:"gid"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, 400, err)
			return
		}
		if err := s.Disks.ProvisionOwnership(name, req.UID, req.GID); err != nil {
			writeError(w, 409, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"provisioned": true})
	case r.Method == http.MethodPost && action == "detach":
		if err := s.Disks.Detach(name); err != nil {
			writeError(w, 409, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"detached": true})
	case r.Method == http.MethodPost && action == "fence":
		var req struct {
			Session uint64 `json:"session"`
			Address string `json:"address"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, 400, err)
			return
		}
		if err := s.Disks.FenceCephFS(name, req.Session, req.Address); err != nil {
			writeError(w, 409, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"fenced": true})
	case r.Method == http.MethodDelete && action == "":
		if err := s.Disks.Delete(name, r.URL.Query().Get("destroy_data") == "true"); err != nil {
			writeError(w, 409, err)
			return
		}
		writeJSON(w, 200, map[string]bool{"deleted": true})
	default:
		methodNotAllowed(w)
	}
}

// Storage mutation is admin-only; controllers can inspect but cannot destroy,
// restore or fence arbitrary data as a side effect of workload reconciliation.
func (s *Server) authorizeDisk(next http.HandlerFunc) http.HandlerFunc {
	return s.authorize(func(w http.ResponseWriter, r *http.Request) {
		p, ok := identity.RequestPrincipal(r)
		if !ok || r.Method != http.MethodGet && p.Role != identity.RoleAdmin {
			writeError(w, 403, fmt.Errorf("storage mutation requires admin role"))
			return
		}
		next(w, r)
	})
}
