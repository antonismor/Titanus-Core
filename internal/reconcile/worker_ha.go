package reconcile

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"log"
	"sort"
)

type WorkerPowerFencer interface {
	Fence(string, func() error) error
}

func (c *Controller) maintainWorkerFailovers() error {
	state := c.Store.Snapshot()
	if !state.NativeHA {
		return nil
	}
	if c.Power == nil {
		return fmt.Errorf("Native HA requires out-of-band Worker power fencing")
	}
	candidates := map[string]bool{}
	for _, a := range state.Assignments {
		n := state.Nodes[a.NodeID]
		if n.State == realm.NodeUnreachable {
			for _, cap := range n.Capabilities {
				if cap == model.CapabilityExecution {
					candidates[n.ID] = true
				}
			}
		}
	}
	for _, f := range state.WorkerFailovers {
		if f.CompletedAt.IsZero() {
			candidates[f.NodeID] = true
		}
	}
	ids := []string{}
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		intent, err := c.Store.BeginWorkerFailover(id)
		if err == nil && intent.CompletedAt.IsZero() {
			check := func() error { return c.Store.CheckWorkerFailover(intent) }
			if err = c.Power.Fence(id, check); err == nil {
				err = c.Store.CompleteWorkerFailover(intent)
			}
		}
		// A failed target stays quarantined and blocks only its own retirements.
		// Other healthy Workers must continue receiving lease renewals.
		if err != nil {
			log.Printf("Worker %s failover pending: %v", id, err)
		}
		if err = c.Store.CheckLeader(); err != nil {
			return err
		}
	}
	return nil
}
