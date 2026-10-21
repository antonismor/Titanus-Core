package webinstaller

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/fencing"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/sshtrust"
)

func (s *Server) handleProxmoxSetup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	s.mu.RLock()
	busy := s.running
	s.mu.RUnlock()
	if busy || s.quiescing {
		writeError(w, http.StatusConflict, "installer is busy")
		return
	}
	var req struct {
		Plan     model.RealmPlan      `json:"plan"`
		Password string               `json:"password"`
		Proxmox  fencing.ProxmoxSetup `json:"proxmox"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	defer func() { req.Password = ""; req.Proxmox.Secret = "" }()
	if req.Plan.Installation == nil || req.Plan.Installation.Profile != model.ProfileNativeHA || req.Plan.Installation.Operation != "install" {
		writeError(w, http.StatusBadRequest, "Native HA initial installation required")
		return
	}
	if err := validateAutoQueryInventory(req.Plan); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Password == "" || len(req.Password) > 4096 || strings.ContainsAny(req.Password, "\x00\r\n") {
		writeError(w, http.StatusBadRequest, "enter the initial SSH password in Step 2")
		return
	}
	if err := sshtrust.CheckTools(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	workers := []model.NodeSpec{}
	for _, node := range req.Plan.Nodes {
		if hasCapability(node, model.CapabilityExecution) {
			workers = append(workers, node)
		}
	}
	if len(workers) < 2 {
		writeError(w, http.StatusBadRequest, "Native HA needs at least two EXECUTION Workers")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	bootstrap := s.bootstrapFor(req.Plan)
	hosts, err := bootstrap.Discover(ctx, workers)
	if err != nil {
		writeError(w, http.StatusBadGateway, "Worker SSH identity discovery failed")
		return
	}
	password := []byte(req.Password)
	defer func() {
		for i := range password {
			password[i] = 0
		}
	}()
	probes, err := s.probeNodes(ctx, hosts, password)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	uuids := map[string]string{}
	for _, n := range workers {
		if probes[n.Name].Existing {
			writeError(w, http.StatusBadRequest, "initial fencing setup refuses existing Titanus/Ceph configuration")
			return
		}
		uuids[n.Name] = probes[n.Name].UUID
	}
	targets, err := fencing.ConfigureProxmox(filepath.Join(s.cfg.StateDir, "fencing"), req.Proxmox, uuids)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	data, err := json.MarshalIndent(targets, "", "  ")
	if err == nil {
		err = atomicWrite(filepath.Join(s.cfg.StateDir, "fencing-profile.json"), data, 0600)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot save private fencing profile")
		return
	}
	req.Plan.Installation.PowerFencing = targets
	s.logf("OK", "Proxmox fencing setup verified %d Worker UUIDs and VM power permissions with read-only requests", len(targets))
	writeJSON(w, http.StatusOK, map[string]any{"plan": req.Plan, "findings": []string{"Proxmox fencing configured; CA trust, guest UUIDs and effective VM.Audit/VM.PowerMgmt permissions verified. No power command was sent."}})
}
