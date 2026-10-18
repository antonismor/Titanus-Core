package reconcile

import (
	"context"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"log"
	"sort"
	"time"
)

type WorkerPowerFencer interface {
	Fence(string, func() error) error
}

// Power management can take longer than a dispatch lease. Keep it outside
// the Fleet renewal loop and do not hold Orchestration while waiting on a BMC.
func (c *Controller) runWorkerFailovers(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := c.maintainWorkerFailovers(ctx); err != nil && ctx.Err() == nil {
			log.Printf("Worker fencing pending: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (c *Controller) maintainWorkerFailovers(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.Store.CheckLeader(); err != nil {
		return err
	}
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
			check := func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return c.Store.CheckWorkerFailover(intent)
			}
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
