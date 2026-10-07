package reconcile

import (
	"context"
	"fmt"
	"reflect"
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
	Store    *realm.Store
	Nodes    NodeRuntime
	Sources  *source.Manager
	Interval time.Duration
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
	state := c.Store.Snapshot()
	engine := realm.NewPlacementEngine()

	if err := c.cleanupDeletedFleets(state); err != nil {
		return err
	}
	state = c.Store.Snapshot()

	for _, fleet := range state.Fleets {
		assignments, err := engine.Reconcile(state, fleet)
		if err != nil {
			return err
		}
		current := assignmentsForFleet(state, fleet.Name)
		if !reflect.DeepEqual(normalizeAssignments(current), normalizeAssignments(assignments)) {
			for _, stale := range staleAssignments(current, assignments) {
				c.cleanupAssignment(state, stale)
			}
			if err := c.Store.SetAssignments(fleet.Name, assignments); err != nil {
				return err
			}
			state = c.Store.Snapshot()
			assignments = assignmentsForFleet(state, fleet.Name)
		}

		for _, assignment := range assignments {
			node, ok := state.Nodes[assignment.NodeID]
			if !ok || node.State != realm.NodeReady {
				continue
			}
			now := time.Now().UTC()
			if !assignment.StartAfter.IsZero() && now.Before(assignment.StartAfter) {
				continue
			}

			token := assignment.LeaseToken
			if token == "" {
				var err error
				token, err = lease.NewToken()
				if err != nil {
					_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
					continue
				}
			}
			const leaseTTL = 20 * time.Second
			record, err := c.Nodes.RenewLease(node.Address, assignment.ID, token, leaseTTL)
			if err != nil {
				_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
				continue
			}
			if err := c.Store.RenewAssignmentLease(assignment.ID, token, now.Add(leaseTTL)); err != nil {
				_ = c.Nodes.RevokeLease(node.Address, assignment.ID, token)
				_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
				continue
			}
			_ = record

			spec := unitSpec(fleet, assignment)
			if err := c.Nodes.EnsureSource(node.Address, spec.Source, c.Sources); err != nil {
				_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
				continue
			}
			if _, err := c.Nodes.EnsureUnit(node.Address, spec); err != nil {
				_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
				continue
			}
			runtimeState, err := c.Nodes.StartUnit(node.Address, assignment.ID)
			if err != nil {
				_ = c.Store.UpdateAssignmentState(assignment.ID, realm.AssignmentImpaired)
				continue
			}
			if runtimeState.Status == unitruntime.StatusActive {
				_ = c.Store.UpdateAssignmentRuntime(assignment.ID, runtimeState.NetworkAddress, realm.AssignmentActive)
			} else {
				_ = c.Store.UpdateAssignmentRuntime(assignment.ID, runtimeState.NetworkAddress, realm.AssignmentStarting)
			}
		}
	}
	return nil
}

func (c *Controller) cleanupDeletedFleets(state realm.State) error {
	groups := map[string][]realm.Assignment{}
	for _, assignment := range state.Assignments {
		if _, exists := state.Fleets[assignment.Fleet]; !exists {
			groups[assignment.Fleet] = append(groups[assignment.Fleet], assignment)
		}
	}
	for fleetName, assignments := range groups {
		for _, assignment := range assignments {
			c.cleanupAssignment(state, assignment)
		}
		if err := c.Store.SetAssignments(fleetName, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *Controller) cleanupAssignment(state realm.State, assignment realm.Assignment) {
	node, ok := state.Nodes[assignment.NodeID]
	if !ok || node.State == realm.NodeUnreachable || node.Address == "" {
		return
	}
	if _, err := c.Nodes.StopUnit(node.Address, assignment.ID); err != nil {
		return
	}
	if assignment.LeaseToken != "" {
		_ = c.Nodes.RevokeLease(node.Address, assignment.ID, assignment.LeaseToken)
	}
	_ = c.Nodes.DeleteUnit(node.Address, assignment.ID)
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
	return unitruntime.Spec{
		ID:          assignment.ID,
		Source:      fleet.Template.Source,
		Hostname:    assignment.ID,
		Command:     append([]string(nil), fleet.Template.Command...),
		Environment: append([]string(nil), fleet.Template.Environment...),
		MemoryBytes: fleet.Template.MemoryBytes,
		CPUPercent:  fleet.Template.CPUPercent,
		PidsMax:     fleet.Template.PidsMax,
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
