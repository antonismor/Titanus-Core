package reconcile

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"github.com/antonismor/Titanus-Core/internal/version"
	"testing"
	"time"
)

func upgradeM6(t *testing.T, s *realm.Store) {
	t.Helper()
	caps := version.Compatible()
	observed := map[string]version.Capabilities{"controller": caps}
	required := []string{"controller"}
	for id, n := range s.Snapshot().Nodes {
		n.Compatibility = &caps
		if e := s.UpsertNode(n); e != nil {
			t.Fatal(e)
		}
		observed[id] = caps
		required = append(required, id)
	}
	for target := 1; target <= 2; target++ {
		if _, e := s.TransitionSchemaTo(fmt.Sprintf("reconcile-m6-schema-%d", target), s.Snapshot().Revision, target, observed, required); e != nil {
			t.Fatal(e)
		}
	}
}

type retryNode struct {
	taskNode
	revokeFail bool
}

func (n *retryNode) RevokeLease(string, string, string) error {
	if n.revokeFail {
		return fmt.Errorf("lost fencing acknowledgement")
	}
	return nil
}
func TestRetryRequiresOriginalExitStopAndLeaseAcknowledgement(t *testing.T) {
	s := taskStore(t)
	upgradeM6(t, s)
	now := time.Now().UTC()
	j := realm.TaskSchedule{Name: "retry", Template: realm.UnitTemplate{Source: "app", Command: []string{"app"}}, DueAt: now, MaxRuns: 1, MaxAttempts: 2, RetrySeconds: 1}
	if e := s.CreateTaskSchedule(j); e != nil {
		t.Fatal(e)
	}
	n := &retryNode{}
	c := &Controller{Store: s, Nodes: n, Now: func() time.Time { return now }}
	if e := c.Once(); e != nil {
		t.Fatal(e)
	}
	first := s.Snapshot().TaskSchedules[j.Name].ActiveTask
	n.inspectFail = true
	c.Once()
	now = now.Add(time.Hour)
	c = &Controller{Store: s, Nodes: n, Now: func() time.Time { return now }}
	c.Once()
	if n.starts != 1 || len(s.Snapshot().Tasks) != 1 || s.Snapshot().Tasks[first].Phase != realm.TaskUnknown {
		t.Fatal("unknown execution retried after controller reconstruction")
	}
	n.inspectFail = false
	code := 7
	n.state.Status = unitruntime.StatusStopped
	n.state.ExitCode = &code
	c.Once()
	n.revokeFail = true
	c.Once()
	if s.Snapshot().TaskSchedules[j.Name].Attempt != 1 {
		t.Fatal("unacknowledged fence advanced retry")
	}
	n.revokeFail = false
	c.Once()
	if s.Snapshot().TaskSchedules[j.Name].Attempt != 2 {
		t.Fatal("matching exit/fence did not advance retry")
	}
	now = now.Add(2 * time.Second)
	n.state = unitruntime.State{}
	c.Once()
	second := s.Snapshot().TaskSchedules[j.Name].ActiveTask
	if n.starts != 2 || first == second {
		t.Fatal("retry did not create new immutable Task")
	}
}
func TestAdditionalMetricsRequireCompleteFreshActualSamples(t *testing.T) {
	for _, metric := range []string{"memory", "pids"} {
		t.Run(metric, func(t *testing.T) {
			s := taskStore(t)
			upgradeM6(t, s)
			if e := s.PutFleet(realm.Fleet{Name: "web", Instances: 1, Template: realm.UnitTemplate{Source: "app", Command: []string{"app"}, CPUPercent: 50, MemoryBytes: 64 << 20, PidsMax: 64}}); e != nil {
				t.Fatal(e)
			}
			a := realm.Autoscaler{Fleet: "web", Min: 1, Max: 2, CooldownSeconds: 10, DownscaleSeconds: 30}
			if metric == "memory" {
				a.TargetMemory = 60
			} else {
				a.TargetPids = 60
			}
			if e := s.PutAutoscaler(a); e != nil {
				t.Fatal(e)
			}
			now := time.Now().Add(11 * time.Second)
			id := "web-001-g1"
			if e := s.SetAssignments("web", []realm.Assignment{{ID: id, Fleet: "web", NodeID: "n1", Generation: 1, State: realm.AssignmentActive, LeaseExpiresAt: now.Add(time.Minute)}}); e != nil {
				t.Fatal(e)
			}
			n := &taskNode{usage: unitruntime.Usage{RunID: "r", Time: now, MemoryBytes: 60 << 20, Pids: 60}}
			c := &Controller{Store: s, Nodes: n, Now: func() time.Time { return now }}
			c.autoscale()
			if s.Snapshot().Fleets["web"].Instances != 1 {
				t.Fatal("no-baseline metric scaled")
			}
			c.samples[id] = unitruntime.Usage{RunID: "r", Time: now.Add(-5 * time.Second)}
			n.usage.Time = now.Add(-10 * time.Second)
			c.autoscale()
			if s.Snapshot().Fleets["web"].Instances != 1 {
				t.Fatal("stale metric scaled")
			}
			c.samples[id] = unitruntime.Usage{RunID: "r", Time: now.Add(-5 * time.Second)}
			n.usage.Time = now
			c.autoscale()
			if s.Snapshot().Fleets["web"].Instances != 2 {
				t.Fatal("actual additional metric did not scale")
			}
		})
	}
}
