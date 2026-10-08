package reconcile

import (
	"context"
	"fmt"
	"time"

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/fabric"
	"github.com/antonismor/Titanus-Core/internal/lease"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
)

type NodeRuntime interface {
	EnsureUnit(address string, spec unitruntime.Spec) (unitruntime.State, error)
	StartUnit(address, id string) (unitruntime.State, error)
	StopUnit(address, id string) (unitruntime.State, error)
	DeleteUnit(address, id string) error
	EnsureSource(address, name string, local *source.Manager) error
	RenewLease(address, id, token string, ttl time.Duration) (lease.Record, error)
	RevokeLease(address, id, token string) error
}

type Controller struct {
	Storage  StorageFencer
	Store    *realm.Store
	Nodes    NodeRuntime
	Sources  *source.Manager
	Interval time.Duration
	samples  map[string]unitruntime.Usage
	Now      func() time.Time
}

func (c *Controller) Run(ctx context.Context) {
	if c.Interval <= 0 {
		c.Interval = 5 * time.Second
	}
	ticker := time.NewTicker(c.Interval)
	defer ticker.Stop()
	for {
		_ = c.Once()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Controller) Once() error {
	if c.Store == nil || c.Nodes == nil {
		return fmt.Errorf("Realm reconciler is not configured")
	}
	c.Store.Orchestration.Lock()
	defer c.Store.Orchestration.Unlock()
	if err := c.Store.CheckLeader(); err != nil {
		return err
	}
	if err := c.reconcileTasks(); err != nil {
		return err
	}
	c.autoscale()
	state := c.Store.Snapshot()
	if err := c.cleanupDeletedFleets(state); err != nil {
		return err
	}
	engine := realm.NewPlacementEngine()
	for name := range state.Fleets {
		state = c.Store.Snapshot()
		fleet := state.Fleets[name]
		// Refresh readiness before deciding which old Units can be retired.
		for _, a := range assignmentsForFleet(state, name) {
			if a.State != realm.AssignmentStopped {
				c.syncAssignment(state, fleet, a)
			}
		}
		state = c.Store.Snapshot()
		plan := engine.Rolling(state, fleet, time.Now().UTC())
		retained := append([]realm.Assignment(nil), plan.Keep...)
		for _, a := range plan.Retire {
			if err := c.cleanupAssignment(state, a); err != nil {
				// Stop may have succeeded before delete failed. Preserve the latest
				// state so the partially retired application is never restarted.
				retained = append(retained, c.Store.Snapshot().Assignments[a.ID])
			}
		}
		retained = append(retained, plan.Create...)
		if err := c.Store.SetAssignments(name, retained); err != nil {
			return err
		}
		state = c.Store.Snapshot()
		for _, a := range plan.Create {
			c.syncAssignment(state, fleet, a)
		}
	}
	return nil
}
func (c *Controller) syncAssignment(state realm.State, fleet realm.Fleet, assignment realm.Assignment) {
	node, ok := state.Nodes[assignment.NodeID]
	if !ok || node.State != realm.NodeReady || node.StorageQuarantined {
		return
	}
	if err := c.Store.CheckLeader(); err != nil {
		return
	}
	now := time.Now().UTC()
	if !assignment.StartAfter.IsZero() && now.Before(assignment.StartAfter) {
		return
	}
	revision, err := fleet.ForGeneration(assignment.Generation)
	if err != nil {
		_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
		return
	}
	token := assignment.LeaseToken
	if token == "" {
		token, err = lease.NewToken()
		if err != nil {
			return
		}
	}
	const leaseTTL = 20 * time.Second
	record, err := c.Nodes.RenewLease(node.Address, assignment.ID, token, leaseTTL)
	if err != nil {
		_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
		return
	}
	// Response latency must not extend the node's authoritative lease deadline.
	deadline := record.ExpiresAt
	if deadline.IsZero() || deadline.After(now.Add(leaseTTL)) {
		deadline = now.Add(leaseTTL)
	}
	if err := c.Store.RenewAssignmentLease(assignment.ID, token, deadline); err != nil {
		_ = c.Nodes.RevokeLease(node.Address, assignment.ID, token)
		_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
		return
	}
	spec := unitSpec(revision, assignment)
	if err := c.prepareStorage(state, assignment, &spec); err != nil {
		_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
		return
	}
	if err := realm.BindSecrets(state, revision.Template, &spec); err != nil {
		_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
		return
	}
	if err := c.Nodes.EnsureSource(node.Address, spec.Source, c.Sources); err != nil {
		_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
		return
	}
	if _, err := c.Nodes.EnsureUnit(node.Address, spec); err != nil {
		_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
		return
	}
	runtimeState, err := c.Nodes.StartUnit(node.Address, assignment.ID)
	if err != nil {
		_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
		return
	}
	if err := c.recordStorage(state, assignment); err != nil {
		_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
		return
	}
	status := realm.AssignmentStarting
	if runtimeState.Status == unitruntime.StatusActive && runtimeState.Ready {
		status = realm.AssignmentActive
	}
	_ = c.Store.UpdateAssignmentRuntime(assignment.ID, runtimeState.NetworkAddress, status)
}

func (c *Controller) cleanupDeletedFleets(state realm.State) error {
	groups := map[string][]realm.Assignment{}
	for _, assignment := range state.Assignments {
		if _, exists := state.Fleets[assignment.Fleet]; !exists {
			groups[assignment.Fleet] = append(groups[assignment.Fleet], assignment)
		}
	}
	for name, assignments := range groups {
		retained := []realm.Assignment{}
		for _, a := range assignments {
			if err := c.cleanupAssignment(state, a); err != nil {
				retained = append(retained, c.Store.Snapshot().Assignments[a.ID])
			}
		}
		if err := c.Store.SetAssignments(name, retained); err != nil {
			return err
		}
	}

	return nil
}

func (c *Controller) cleanupAssignment(state realm.State, assignment realm.Assignment) error {
	if err := c.Store.CheckLeader(); err != nil {
		return err
	}
	node, ok := state.Nodes[assignment.NodeID]
	if !ok || node.State == realm.NodeUnreachable || node.StorageQuarantined || node.Address == "" {
		fleet, exists := state.Fleets[assignment.Fleet]
		if !exists {
			return fmt.Errorf("deleted Fleet requires acknowledged node cleanup")
		}
		previous, err := fleet.ForGeneration(assignment.Generation)
		if err != nil {
			return err
		}
		if len(assignment.StorageWriters) > 0 {
			return c.fenceStorage(assignment)
		}
		// Writable storage cannot be released by a time-based scheduling lease.
		for _, m := range previous.Template.Mounts {
			if !m.ReadOnly {
				return c.fenceStorage(assignment)
			}
		}
		if assignment.LeaseExpiresAt.IsZero() || time.Now().Before(assignment.LeaseExpiresAt.Add(2*time.Second)) {
			return fmt.Errorf("node lease has not expired")
		}
		return nil
	}
	if _, err := c.Nodes.StopUnit(node.Address, assignment.ID); err != nil {
		return err
	}
	if err := c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentStopped); err != nil {
		return err
	}
	if assignment.LeaseToken != "" {
		if err := c.Nodes.RevokeLease(node.Address, assignment.ID, assignment.LeaseToken); err != nil {
			return err
		}
	}
	if err := c.releaseStorage(state, assignment); err != nil {
		return err
	}
	return c.Nodes.DeleteUnit(node.Address, assignment.ID)
}

func staleAssignments(current, desired []realm.Assignment) []realm.Assignment {
	wanted := map[string]string{}
	for _, assignment := range desired {
		wanted[assignment.ID] = assignment.NodeID
	}
	stale := make([]realm.Assignment, 0)
	for _, assignment := range current {
		if nodeID, ok := wanted[assignment.ID]; !ok || nodeID != assignment.NodeID {
			stale = append(stale, assignment)
		}
	}
	return stale
}

func unitSpec(fleet realm.Fleet, assignment realm.Assignment) unitruntime.Spec {
	fleet.Template.Health.Normalize("always")
	return unitruntime.Spec{
		ID:          assignment.ID,
		Source:      fleet.Template.Source,
		Hostname:    assignment.ID,
		Command:     append([]string(nil), fleet.Template.Command...),
		Environment: append([]string(nil), fleet.Template.Environment...),
		MemoryBytes: fleet.Template.MemoryBytes,
		CPUPercent:  fleet.Template.CPUPercent,
		PidsMax:     fleet.Template.PidsMax,
		Security:    fleet.Template.Security,
		Health:      fleet.Template.Health,
		Network: unitruntime.NetworkSpec{
			Fabric: fleet.Template.Fabric,
			Ports:  append([]fabric.Port(nil), fleet.Template.Ports...),
		},
		Mounts: append([]disk.Mount(nil), fleet.Template.Mounts...),
	}
}

func assignmentsForFleet(state realm.State, name string) []realm.Assignment {
	out := make([]realm.Assignment, 0)
	for _, assignment := range state.Assignments {
		if assignment.Fleet == name {
			out = append(out, assignment)
		}
	}
	return out
}

func normalizeAssignments(items []realm.Assignment) map[string]string {
	out := map[string]string{}
	for _, item := range items {
		out[item.ID] = item.NodeID + ":" + string(item.State) + ":" + item.StartAfter.UTC().Format(time.RFC3339Nano)
	}
	return out
}
