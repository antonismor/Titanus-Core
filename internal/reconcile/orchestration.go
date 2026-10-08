package reconcile

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/lease"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"math"
	"time"
)

type InspectingNodes interface {
	InspectUnit(string, string) (unitruntime.Spec, unitruntime.State, error)
}
type MeasuringNodes interface {
	UnitUsage(string, string) (unitruntime.Usage, error)
}

func (c *Controller) reconcileTasks() error {
	inspector, ok := c.Nodes.(InspectingNodes)
	if !ok {
		return nil
	}
	for _, t := range c.Store.Snapshot().Tasks {
		if t.Terminal() {
			continue
		}
		if e := c.Store.CheckLeader(); e != nil {
			return e
		}
		state := c.Store.Snapshot()
		if t.CancelRequested && t.NodeID == "" {
			t.Phase = realm.TaskCancelled
			if e := c.Store.UpdateTask(t); e != nil {
				return e
			}
			continue
		}
		if t.Phase == realm.TaskPending {
			fleet := realm.Fleet{Name: "task-" + t.Name, Instances: 1, Template: t.Template, RequiredLabels: t.RequiredLabels}
			ranked := realm.NewPlacementEngine().Rank(state, fleet, nil)
			if len(ranked) == 0 {
				continue
			}
			t.NodeID = ranked[0].NodeID
			t.Phase = realm.TaskPrepared
			token, e := lease.NewToken()
			if e != nil {
				return e
			}
			t.LeaseToken = token
			if e = c.Store.UpdateTask(t); e != nil {
				return e
			}
		}
		node, exists := state.Nodes[t.NodeID]
		if !exists || node.State != realm.NodeReady {
			if t.Phase == realm.TaskPrepared {
				continue
			}
			if t.Phase != realm.TaskUnknown {
				t.Phase = realm.TaskUnknown
				if e := c.Store.UpdateTask(t); e != nil {
					return e
				}
			}
			continue
		}
		if t.CancelRequested {
			if t.Phase == realm.TaskPrepared {
				_, _, inspectErr := inspector.InspectUnit(node.Address, t.UnitID)
				if inspectErr != nil {
					t.Phase = realm.TaskCancelled
					if e := c.Store.UpdateTask(t); e != nil {
						return e
					}
					continue
				}
			}
			if _, e := c.Nodes.StopUnit(node.Address, t.UnitID); e != nil {
				continue
			}
			if e := c.Nodes.RevokeLease(node.Address, t.UnitID, t.LeaseToken); e != nil {
				continue
			}
			t.Phase = realm.TaskCancelled
			if e := c.Store.UpdateTask(t); e != nil {
				return e
			}
			continue
		}
		if t.Phase == realm.TaskPrepared {
			spec := unitSpec(realm.Fleet{Template: t.Template}, realm.Assignment{ID: t.UnitID})
			if e := realm.BindSecrets(state, t.Template, &spec); e != nil {
				return e
			}
			if e := c.Nodes.EnsureSource(node.Address, spec.Source, c.Sources); e != nil {
				continue
			}
			if _, e := c.Nodes.EnsureUnit(node.Address, spec); e != nil {
				continue
			}
			if _, e := c.Nodes.RenewLease(node.Address, t.UnitID, t.LeaseToken, 20*time.Second); e != nil {
				continue
			}
			// Persist before invoking Start: a lost response or crash cannot replay a
			// command with side effects. Ambiguous dispatch remains visible, not retried.
			t.Phase = realm.TaskDispatched
			if e := c.Store.UpdateTask(t); e != nil {
				return e
			}
			_, _ = c.Nodes.StartUnit(node.Address, t.UnitID)
		}
		_, u, e := inspector.InspectUnit(node.Address, t.UnitID)
		if e != nil {
			if t.Phase != realm.TaskUnknown {
				t.Phase = realm.TaskUnknown
				if e = c.Store.UpdateTask(t); e != nil {
					return e
				}
			}
			continue
		}
		if t.RunID != "" && u.RunID != t.RunID {
			t.Phase = realm.TaskUnknown
			t.ExitCode = nil
		} else if u.Status == unitruntime.StatusActive || u.Status == unitruntime.StatusStarting {
			if u.RunID == "" {
				t.Phase = realm.TaskUnknown
			} else {
				t.RunID = u.RunID
				t.Phase = realm.TaskRunning
				if _, e = c.Nodes.RenewLease(node.Address, t.UnitID, t.LeaseToken, 20*time.Second); e != nil {
					t.Phase = realm.TaskUnknown
				}
			}
		} else if u.ExitCode != nil && u.RunID != "" {
			t.RunID = u.RunID
			t.ExitCode = u.ExitCode
			t.Phase = realm.TaskFailed
			if *u.ExitCode == 0 {
				t.Phase = realm.TaskSucceeded
			}
		} else {
			t.Phase = realm.TaskUnknown
		}
		if e = c.Store.UpdateTask(t); e != nil {
			return e
		}
	}
	return nil
}
func (c *Controller) autoscale() {
	meter, ok := c.Nodes.(MeasuringNodes)
	if !ok {
		return
	}
	if c.samples == nil {
		c.samples = map[string]unitruntime.Usage{}
	}
	state := c.Store.Snapshot()
	next := map[string]unitruntime.Usage{}
	for name, a := range state.Autoscalers {
		f, exists := state.Fleets[name]
		if !exists {
			continue
		}
		now := time.Now().UTC()
		if c.Now != nil {
			now = c.Now().UTC()
		}
		valid := true
		count := 0
		total := float64(0)
		if f.Template.CPUPercent < 1 || len(f.Template.Mounts) > 0 {
			_ = c.Store.ResetAutoscaleWindow(name)
			continue
		}
		for _, assignment := range assignmentsForFleet(state, name) {
			node := state.Nodes[assignment.NodeID]
			if assignment.Generation != f.Generation || assignment.State != realm.AssignmentActive || node.State != realm.NodeReady || !now.Before(assignment.LeaseExpiresAt) {
				valid = false
				continue
			}
			if e := c.Store.CheckLeader(); e != nil {
				return
			}
			u, e := meter.UnitUsage(node.Address, assignment.ID)
			if e != nil {
				valid = false
				continue
			}
			next[assignment.ID] = u
			prev, found := c.samples[assignment.ID]
			dt := u.Time.Sub(prev.Time)
			if !found || prev.RunID != u.RunID || u.RunID == "" || u.CPUUsec < prev.CPUUsec || dt < time.Second || dt > 30*time.Second || now.Sub(u.Time) > 5*time.Second || u.Time.After(now.Add(time.Second)) {
				valid = false
				continue
			}
			total += float64(u.CPUUsec-prev.CPUUsec) / float64(dt.Microseconds()) * 10000 / float64(f.Template.CPUPercent)
			count++
		}
		if !valid || count != f.Instances || count == 0 {
			_ = c.Store.ResetAutoscaleWindow(name)
			continue
		}
		average := total / float64(count)
		desired := f.Instances
		if math.Abs(average-float64(a.TargetCPU)) > float64(a.TargetCPU)*0.10 {
			raw := math.Ceil(float64(count) * average / float64(a.TargetCPU))
			raw = math.Max(float64(a.Min), math.Min(float64(a.Max), raw))
			desired = int(raw)
		}
		_ = c.Store.ApplyAutoscale(name, desired, now)
	}
	c.samples = next
}
func (g *GuardedNodes) InspectUnit(a, id string) (unitruntime.Spec, unitruntime.State, error) {
	if e := g.Check(); e != nil {
		return unitruntime.Spec{}, unitruntime.State{}, e
	}
	n, ok := g.NodeRuntime.(InspectingNodes)
	if !ok {
		return unitruntime.Spec{}, unitruntime.State{}, fmt.Errorf("runtime inspection unavailable")
	}
	return n.InspectUnit(a, id)
}
func (g *GuardedNodes) UnitUsage(a, id string) (unitruntime.Usage, error) {
	if e := g.Check(); e != nil {
		return unitruntime.Usage{}, e
	}
	n, ok := g.NodeRuntime.(MeasuringNodes)
	if !ok {
		return unitruntime.Usage{}, fmt.Errorf("runtime measurements unavailable")
	}
	return n.UnitUsage(a, id)
}
