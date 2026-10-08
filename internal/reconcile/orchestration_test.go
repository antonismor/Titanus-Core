package reconcile

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"testing"
	"time"
)

type taskNode struct {
	healthNode
	state       unitruntime.State
	starts      int
	lost        bool
	inspectFail bool
	usage       unitruntime.Usage
	usageFail   bool
}

func (n *taskNode) EnsureUnit(_ string, s unitruntime.Spec) (unitruntime.State, error) {
	n.last = s
	if n.state.Status == "" {
		n.state = unitruntime.State{ID: s.ID, Status: unitruntime.StatusCreated}
	}
	return n.state, nil
}
func (n *taskNode) StartUnit(_, id string) (unitruntime.State, error) {
	n.starts++
	n.state = unitruntime.State{ID: id, Status: unitruntime.StatusActive, Ready: true, RunID: "run1"}
	if n.lost {
		return unitruntime.State{}, fmt.Errorf("response lost")
	}
	return n.state, nil
}
func (n *taskNode) InspectUnit(string, string) (unitruntime.Spec, unitruntime.State, error) {
	if n.inspectFail {
		return unitruntime.Spec{}, unitruntime.State{}, fmt.Errorf("unreachable")
	}
	return n.last, n.state, nil
}
func (n *taskNode) StopUnit(string, string) (unitruntime.State, error) {
	if n.inspectFail {
		return unitruntime.State{}, fmt.Errorf("unreachable")
	}
	n.state.Status = unitruntime.StatusStopped
	return n.state, nil
}
func (n *taskNode) UnitUsage(string, string) (unitruntime.Usage, error) {
	if n.usageFail {
		return unitruntime.Usage{}, fmt.Errorf("missing counters")
	}
	return n.usage, nil
}
func taskStore(t *testing.T) *realm.Store {
	s, e := realm.Open(t.TempDir(), "LAB")
	if e != nil {
		t.Fatal(e)
	}
	e = s.UpsertNode(realm.Node{ID: "n1", Address: "local", State: realm.NodeReady, Capabilities: []model.Capability{model.CapabilityExecution}, Resources: realm.Resources{MemoryBytes: 4 << 30, CPUMilliCapacity: 4000}})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestTaskLostStartResponseExitAndNoReplay(t *testing.T) {
	s := taskStore(t)
	if e := s.CreateTask(realm.Task{Name: "one", Template: realm.UnitTemplate{Source: "app", Command: []string{"app"}}}); e != nil {
		t.Fatal(e)
	}
	n := &taskNode{lost: true, inspectFail: true}
	c := &Controller{Store: s, Nodes: n}
	if e := c.Once(); e != nil {
		t.Fatal(e)
	}
	if s.Snapshot().Tasks["one"].Phase != realm.TaskUnknown || n.starts != 1 {
		t.Fatal("ambiguous launch not represented")
	}
	c = &Controller{Store: s, Nodes: n}
	n.inspectFail = false
	if e := c.Once(); e != nil {
		t.Fatal(e)
	}
	if s.Snapshot().Tasks["one"].Phase != realm.TaskRunning || n.starts != 1 {
		t.Fatal("replayed Task after recovery")
	}
	code := 7
	n.state.Status = unitruntime.StatusStopped
	n.state.ExitCode = &code
	c.Once()
	task := s.Snapshot().Tasks["one"]
	if task.Phase != realm.TaskFailed || task.ExitCode == nil || *task.ExitCode != 7 {
		t.Fatal("lost real failure code")
	}
	c.Once()
	if n.starts != 1 {
		t.Fatal("terminal execution replayed")
	}
}
func TestTaskDispatchedBeforeCrashNeverReplayedAndCancelNeedsAcknowledgement(t *testing.T) {
	s := taskStore(t)
	s.CreateTask(realm.Task{Name: "one", Template: realm.UnitTemplate{Source: "app", Command: []string{"app"}}})
	task := s.Snapshot().Tasks["one"]
	task.NodeID = "n1"
	task.Phase = realm.TaskDispatched
	s.UpdateTask(task)
	n := &taskNode{state: unitruntime.State{Status: unitruntime.StatusCreated}}
	c := &Controller{Store: s, Nodes: n}
	c.Once()
	if n.starts != 0 || s.Snapshot().Tasks["one"].Phase != realm.TaskUnknown {
		t.Fatal("uncertain dispatch replayed")
	}
	s.CancelTask("one")
	n.inspectFail = true
	c.Once()
	if s.Snapshot().Tasks["one"].Phase == realm.TaskCancelled {
		t.Fatal("claimed cancellation without ack")
	}
	n.inspectFail = false
	c.Once()
	if s.Snapshot().Tasks["one"].Phase != realm.TaskCancelled {
		t.Fatal("acknowledged cancellation lost")
	}
}
func TestAutoscaleUsesActualDeltasAndRejectsMissingOrRestartedCounters(t *testing.T) {
	s := taskStore(t)
	s.PutFleet(realm.Fleet{Name: "web", Instances: 1, Template: realm.UnitTemplate{Source: "app", Command: []string{"app"}, CPUPercent: 50}})
	s.PutAutoscaler(realm.Autoscaler{Fleet: "web", Min: 1, Max: 5, TargetCPU: 60, CooldownSeconds: 10, DownscaleSeconds: 30})
	id := "web-001-g1"
	s.SetAssignments("web", []realm.Assignment{{ID: id, Fleet: "web", NodeID: "n1", Generation: 1, State: realm.AssignmentActive, LeaseExpiresAt: time.Now().Add(time.Minute)}})
	n := &taskNode{usage: unitruntime.Usage{RunID: "r", CPUUsec: 100, Time: time.Now()}}
	c := &Controller{Store: s, Nodes: n}
	c.autoscale()
	if s.Snapshot().Fleets["web"].Instances != 1 {
		t.Fatal("scaled without baseline")
	}
	c.samples[id] = unitruntime.Usage{RunID: "r", CPUUsec: 100, Time: n.usage.Time.Add(-5 * time.Second)}
	n.usage.CPUUsec = 2500100
	n.usageFail = true
	c.autoscale()
	if s.Snapshot().Fleets["web"].Instances != 1 {
		t.Fatal("missing metrics caused scaling")
	}
	n.usageFail = false
	c.samples[id] = unitruntime.Usage{RunID: "old", Time: n.usage.Time.Add(-5 * time.Second)}
	c.autoscale()
	if s.Snapshot().Fleets["web"].Instances != 1 {
		t.Fatal("restart produced CPU delta")
	}
}

func TestAutoscalePositiveCPUDeltaAndTolerance(t *testing.T) {
	s := taskStore(t)
	s.PutFleet(realm.Fleet{Name: "web", Instances: 1, Template: realm.UnitTemplate{Source: "app", Command: []string{"app"}, CPUPercent: 50}})
	s.PutAutoscaler(realm.Autoscaler{Fleet: "web", Min: 1, Max: 5, TargetCPU: 60, CooldownSeconds: 10, DownscaleSeconds: 30})
	now := time.Now().Add(11 * time.Second)
	id := "web-001-g1"
	s.SetAssignments("web", []realm.Assignment{{ID: id, Fleet: "web", NodeID: "n1", Generation: 1, State: realm.AssignmentActive, LeaseExpiresAt: now.Add(time.Minute)}})
	n := &taskNode{usage: unitruntime.Usage{RunID: "r", CPUUsec: 2500100, Time: now}}
	c := &Controller{Store: s, Nodes: n, Now: func() time.Time { return now }, samples: map[string]unitruntime.Usage{id: {RunID: "r", CPUUsec: 100, Time: now.Add(-5 * time.Second)}}}
	c.autoscale()
	if s.Snapshot().Fleets["web"].Instances != 2 {
		t.Fatal("real 100% CPU quota utilization did not scale to two instances")
	}
	// A non-complete rollout cannot immediately recommend a second scale.
	c.autoscale()
	if s.Snapshot().Fleets["web"].Instances != 2 {
		t.Fatal("scaled incomplete assignments")
	}
}
