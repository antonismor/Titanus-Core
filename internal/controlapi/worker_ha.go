package controlapi

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"time"
)

type WorkerRecoveryClient interface {
	Compatibility(string) (CompatibilityInfo, error)
	ListUnits(string) ([]unitruntime.State, error)
}

func (s *Server) verifyWorkerRecovery(id string) error {
	state := s.Store.Snapshot()
	worker := false
	for _, f := range state.WorkerFailovers {
		if f.NodeID == id && f.RecoveredAt.IsZero() {
			worker = true
		}
	}
	if !worker {
		return nil
	}
	n, ok := state.Nodes[id]
	if !ok || n.State != realm.NodeReady || time.Since(n.LastPulse) > 15*time.Second || s.WorkerRecovery == nil {
		return fmt.Errorf("Worker recovery requires a fresh Pulse and authenticated idle runtime")
	}
	info, e := s.WorkerRecovery.Compatibility(n.Address)
	if e != nil || info.NodeID != id || info.Realm != state.Name || !info.Capabilities.Supports(3) {
		return fmt.Errorf("returning Worker identity/schema verification failed")
	}
	units, e := s.WorkerRecovery.ListUnits(n.Address)
	if e != nil || units == nil || len(units) > 0 {
		return fmt.Errorf("returning Worker must have an empty runtime inventory; independently exclude and clean old Units/mounts before recovery")
	}
	return s.Store.CheckLeader()
}
