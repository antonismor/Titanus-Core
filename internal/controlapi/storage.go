package controlapi

import (
	"fmt"
	"net/http"

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/identity"
)

func (s *Server) registerStorage(mux *http.ServeMux) {
	mux.HandleFunc("/v1/realm/disks", s.authorize(s.diskCatalogs))
	mux.HandleFunc("/v1/node/storage/adopt", s.authorize(s.adoptDisk))
	mux.HandleFunc("/v1/node/storage/release", s.authorize(s.releaseDisk))
}

func (s *Server) diskCatalogs(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil || s.Disks == nil {
		writeError(w, 503, fmt.Errorf("storage unavailable"))
		return
	}
	if r.Method == "GET" {
		writeJSON(w, 200, s.Store.Snapshot().Disks)
		return
	}
	p, _ := identity.RequestPrincipal(r)
	if r.Method != "POST" {
		methodNotAllowed(w)
		return
	}
	if p.Role != identity.RoleAdmin {
		writeError(w, 403, fmt.Errorf("catalog publication requires admin"))
		return
	}
	var c disk.Catalog
	if err := decodeJSON(r, &c); err != nil {
		writeError(w, 400, err)
		return
	}
	if c.Spec.Provider != disk.ProviderLocal {
		// Verify the backend using this controller's own private Ceph config.
		if err := s.Disks.VerifyCatalog(c); err != nil {
			writeError(w, 409, err)
			return
		}
	} else {
		if _, ok := s.Store.Snapshot().Nodes[c.LocalNode]; !ok {
			writeError(w, 409, fmt.Errorf("unknown local Disk node"))
			return
		}
	}
	if err := s.Store.PutDiskCatalog(c); err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 201, c)
}

func (s *Server) adoptDisk(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		methodNotAllowed(w)
		return
	}
	if s.Disks == nil {
		writeError(w, 503, fmt.Errorf("storage unavailable"))
		return
	}
	var c disk.Catalog
	if err := decodeJSON(r, &c); err != nil {
		writeError(w, 400, err)
		return
	}
	if err := s.Disks.Adopt(c); err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"adopted": true})
}

func (s *Server) releaseDisk(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		methodNotAllowed(w)
		return
	}
	if s.Disks == nil || s.Runtime == nil {
		writeError(w, 503, fmt.Errorf("runtime/storage unavailable"))
		return
	}
	var req struct {
		Disk      string `json:"disk"`
		Unit      string `json:"unit"`
		CatalogID string `json:"catalog_id"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, 400, err)
		return
	}
	spec, state, err := s.Runtime.Inspect(req.Unit)
	if err != nil || state.PID != 0 || state.DesiredRunning {
		writeError(w, 409, fmt.Errorf("acknowledged stopped Unit required"))
		return
	}
	d, err := s.Disks.Inspect(req.Disk)
	bound := false
	for _, m := range spec.Mounts {
		if m.Disk == req.Disk {
			bound = true
		}
	}
	if err != nil || !bound || d.ManagedID == "" || d.ManagedID != req.CatalogID {
		writeError(w, 409, fmt.Errorf("release is not bound to managed Unit Disk"))
		return
	}
	if err = s.Disks.Detach(req.Disk); err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"released": true})
}
