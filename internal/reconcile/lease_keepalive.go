package reconcile

import (
	"github.com/antonismor/Titanus-Core/internal/realm"
	"time"
)

const assignmentLeaseTTL = 20 * time.Second

type assignmentLeaseKeeper struct {
	stop chan struct{}
	done chan struct{}
}

// Keep only an already quorum-retained, exact assignment alive while a slow
// storage/runtime action holds the orchestration lock. This does not create
// tokens, authorize placement or substitute for Worker/storage fencing.
func (c *Controller) keepAssignmentLease(id string) {
	if c.leaseKeepers == nil {
		return
	}
	if _, ok := c.leaseKeepers[id]; ok {
		return
	}
	state := c.Store.Snapshot()
	a, ok := state.Assignments[id]
	if !ok || a.LeaseToken == "" || a.State == realm.AssignmentStopped {
		return
	}
	if _, ok := state.Fleets[a.Fleet]; !ok {
		return
	}
	k := &assignmentLeaseKeeper{stop: make(chan struct{}), done: make(chan struct{})}
	c.leaseKeepers[id] = k
	interval := 3 * time.Second
	if c.leaseRefreshInterval > 0 {
		interval = c.leaseRefreshInterval
	}
	go func() {
		defer close(k.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-k.stop:
				return
			case <-ticker.C:
			}
			// An already selected tick must not dispatch after shutdown/retirement.
			select {
			case <-k.stop:
				return
			default:
			}
			s := c.Store.Snapshot()
			current, exists := s.Assignments[id]
			node := s.Nodes[a.NodeID]
			if !exists || current.NodeID != a.NodeID || current.Generation != a.Generation || current.LeaseToken != a.LeaseToken || current.State == realm.AssignmentStopped {
				return
			}
			if _, exists = s.Fleets[a.Fleet]; !exists {
				return
			}
			if node.State != realm.NodeReady || node.PowerQuarantined || node.StorageQuarantined {
				continue
			}
			if c.Store.CheckLeader() != nil {
				return
			}
			now := time.Now().UTC()
			record, err := c.Nodes.RenewLease(node.Address, id, a.LeaseToken, assignmentLeaseTTL)
			if err != nil {
				continue
			}
			deadline := record.ExpiresAt
			if deadline.IsZero() || deadline.After(now.Add(assignmentLeaseTTL)) {
				deadline = now.Add(assignmentLeaseTTL)
			}
			// Orchestration remains locked until every keeper has joined. No cleanup,
			// incarnation replacement or operator mutation can race this commit.
			if err = c.Store.RenewAssignmentLease(id, a.LeaseToken, deadline); err != nil {
				return
			}
		}
	}()
}

// Join all in-flight renewals before Stop/Revoke/Delete for this incarnation.
func (c *Controller) stopAssignmentLease(id string) {
	if k, ok := c.leaseKeepers[id]; ok {
		close(k.stop)
		<-k.done
		delete(c.leaseKeepers, id)
	}
}
func (c *Controller) stopAssignmentLeases() {
	for _, k := range c.leaseKeepers {
		close(k.stop)
	}
	for _, k := range c.leaseKeepers {
		<-k.done
	}
	c.leaseKeepers = nil
}
