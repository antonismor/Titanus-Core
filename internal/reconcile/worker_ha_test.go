package reconcile

import (
	"context"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/version"
	"testing"
	"time"
)

type workerFence struct {
	calls int
	fail  bool
	after func()
}

func (f *workerFence) Fence(_ string, check func() error) error {
	if e := check(); e != nil {
		return e
	}
	f.calls++
	if f.fail {
		return fmt.Errorf("power-off unconfirmed")
	}
	if f.after != nil {
		f.after()
	}
	return check()
}
func nativeRecoveryFixture(t *testing.T) (*realm.Store, *Controller, *workerFence, string) {
	t.Helper()
	root := t.TempDir()
	s, e := realm.Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SeedNativeHA(); e != nil {
		t.Fatal(e)
	}
	caps := version.Compatible()
	for _, id := range []string{"a", "b"} {
		if e = s.UpsertNode(realm.Node{ID: id, Address: id, Compatibility: &caps, Capabilities: []model.Capability{model.CapabilityExecution}, State: realm.NodeReady, Resources: realm.Resources{MemoryBytes: 8 << 30, CPUMilliCapacity: 8000}}); e != nil {
			t.Fatal(e)
		}
	}
	if e = s.PutFleet(realm.Fleet{Name: "web", Instances: 1, MinimumAvailable: 1, Template: realm.UnitTemplate{Source: "app", Command: []string{"/bin/app"}}}); e != nil {
		t.Fatal(e)
	}
	power := &workerFence{}
	c := &Controller{Store: s, Nodes: &healthNode{ready: true}, Power: power}
	if e = c.maintainWorkerFailovers(context.Background()); e != nil {
		t.Fatal(e)
	}
	if e = c.Once(); e != nil {
		t.Fatal(e)
	}
	return s, c, power, root
}
func loseWorker(t *testing.T, s *realm.Store) realm.Assignment {
	t.Helper()
	var a realm.Assignment
	for _, current := range s.Snapshot().Assignments {
		a = current
	}
	a.LeaseExpiresAt = time.Now().Add(-time.Minute)
	if e := s.SetAssignments("web", []realm.Assignment{a}); e != nil {
		t.Fatal(e)
	}
	n := s.Snapshot().Nodes[a.NodeID]
	n.State = realm.NodeUnreachable
	if e := s.UpsertNode(n); e != nil {
		t.Fatal(e)
	}
	return a
}
func TestNativeWorkerFailoverWaitsForFenceAndSurvivesRestart(t *testing.T) {
	s, c, power, root := nativeRecoveryFixture(t)
	old := loseWorker(t, s)
	power.fail = true
	for i := 0; i < 2; i++ {
		if e := c.maintainWorkerFailovers(context.Background()); e != nil {
			t.Fatal(e)
		}
		if e := c.Once(); e != nil {
			t.Fatal(e)
		}
	}
	state := s.Snapshot()
	if state.Assignments[old.ID].NodeID != old.NodeID || !state.Nodes[old.NodeID].PowerQuarantined {
		t.Fatal("unconfirmed power fence allowed replacement")
	}
	var e error
	s, e = realm.Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	c.Store = s
	power.fail = false
	for i := 0; i < 3; i++ {
		if e = c.maintainWorkerFailovers(context.Background()); e != nil {
			t.Fatal(e)
		}
		if e = c.Once(); e != nil {
			t.Fatal(e)
		}
	}
	state = s.Snapshot()
	current := state.Assignments[old.ID]
	if current.NodeID == old.NodeID || current.State != realm.AssignmentActive || len(state.Assignments) != 1 {
		t.Fatalf("automatic replacement not active: %+v", state.Assignments)
	}
	if e = s.Pulse(old.NodeID, realm.Resources{}); e != nil {
		t.Fatal(e)
	}
	calls := power.calls
	for i := 0; i < 3; i++ {
		if e = c.maintainWorkerFailovers(context.Background()); e != nil {
			t.Fatal(e)
		}
		if e = c.Once(); e != nil {
			t.Fatal(e)
		}
	}
	if s.Snapshot().Assignments[old.ID].NodeID != current.NodeID || power.calls != calls {
		t.Fatal("returning Worker stole current assignment or was repeatedly fenced")
	}
}
func TestNativeWorkerFailureDoesNotExpireHealthyFleetLeases(t *testing.T) {
	s, c, power, _ := nativeRecoveryFixture(t)
	old := loseWorker(t, s)
	power.fail = true
	if e := s.PutFleet(realm.Fleet{Name: "healthy", Instances: 1, MinimumAvailable: 1, Template: realm.UnitTemplate{Source: "app", Command: []string{"/bin/app"}}}); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		if e := c.maintainWorkerFailovers(context.Background()); e != nil {
			t.Fatal(e)
		}
		if e := c.Once(); e != nil {
			t.Fatal(e)
		}
	}
	healthy := false
	for _, a := range s.Snapshot().Assignments {
		if a.Fleet == "healthy" && a.NodeID != old.NodeID && a.State == realm.AssignmentActive && a.LeaseExpiresAt.After(time.Now()) {
			healthy = true
		}
	}
	if !healthy {
		t.Fatal("failed fencer starved unrelated healthy Fleet")
	}
}
func TestNativeWorkerLocalDiskDoesNotMoveAfterPowerFence(t *testing.T) {
	s, c, _, _ := nativeRecoveryFixture(t)
	// Unknown/local writable mount has no verified remote catalog. It must stall.
	f, _ := s.GetFleet("web")
	f.Template.Mounts = append(f.Template.Mounts, disk.Mount{Disk: "local-data", Target: "/data"})
	if e := s.PutFleet(f); e != nil {
		t.Fatal(e)
	}
	// Existing generation still has no storage. Create an assignment for current
	// generation to model an already realized local writer without claiming a copy.
	f, _ = s.GetFleet("web")
	a := loseWorker(t, s)
	a.Generation = f.Generation
	a.ID = "web-001-g2"
	if e := s.SetAssignments("web", []realm.Assignment{a}); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		if e := c.maintainWorkerFailovers(context.Background()); e != nil {
			t.Fatal(e)
		}
		if e := c.Once(); e != nil {
			t.Fatal(e)
		}
	}
	if s.Snapshot().Assignments[a.ID].NodeID != a.NodeID {
		t.Fatal("local writable data silently moved")
	}
}

type blockingWorkerFence struct{ entered, release chan struct{} }

func (f *blockingWorkerFence) Fence(_ string, check func() error) error {
	if e := check(); e != nil {
		return e
	}
	close(f.entered)
	<-f.release
	return fmt.Errorf("slow management endpoint failed")
}
func TestSlowWorkerFenceDoesNotBlockHealthyLeaseRenewal(t *testing.T) {
	s, c, _, _ := nativeRecoveryFixture(t)
	old := loseWorker(t, s)
	if e := s.PutFleet(realm.Fleet{Name: "healthy", Instances: 1, MinimumAvailable: 1, Template: realm.UnitTemplate{Source: "app", Command: []string{"/bin/app"}}}); e != nil {
		t.Fatal(e)
	}
	fence := &blockingWorkerFence{make(chan struct{}), make(chan struct{})}
	c.Power = fence
	done := make(chan error, 1)
	go func() { done <- c.maintainWorkerFailovers(context.Background()) }()
	select {
	case <-fence.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("fence did not begin")
	}
	renewed := make(chan error, 1)
	go func() { renewed <- c.Once() }()
	select {
	case e := <-renewed:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		close(fence.release)
		t.Fatal("slow fencer held Fleet orchestration/leases")
	}
	healthy := false
	for _, a := range s.Snapshot().Assignments {
		if a.Fleet == "healthy" && a.NodeID != old.NodeID && a.State == realm.AssignmentActive {
			healthy = true
		}
	}
	close(fence.release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if !healthy {
		t.Fatal("healthy Fleet was not served during blocked fence")
	}
}

func TestGatewayFenceBeforeWorkerIntentRecoversAssignmentCohort(t *testing.T) {
	for _, returned := range []bool{false, true} {
		t.Run(fmt.Sprintf("returned-%t", returned), func(t *testing.T) {
			s, c, power, root := nativeRecoveryFixture(t)
			var old realm.Assignment
			for _, a := range s.Snapshot().Assignments {
				old = a
			}
			old.LeaseExpiresAt = time.Now().Add(-time.Minute)
			if e := s.SetAssignments("web", []realm.Assignment{old}); e != nil {
				t.Fatal(e)
			}
			destination := ""
			for id, n := range s.Snapshot().Nodes {
				n.Capabilities = append(n.Capabilities, model.CapabilityGateway)
				if e := s.UpsertNode(n); e != nil {
					t.Fatal(e)
				}
				if id != old.NodeID {
					destination = id
				}
			}
			if e := s.PutGateway(realm.Gateway{Name: "vip", VIP: "192.0.2.100/32", Device: "eth0", Owner: old.NodeID, Standby: destination}); e != nil {
				t.Fatal(e)
			}
			transfer, e := s.BeginGatewayTransfer("vip", destination)
			if e != nil {
				t.Fatal(e)
			}
			if e = s.CompleteGatewayTransfer(transfer, true); e != nil {
				t.Fatal(e)
			}
			if !returned {
				n := s.Snapshot().Nodes[old.NodeID]
				n.State = realm.NodeUnreachable
				if e = s.UpsertNode(n); e != nil {
					t.Fatal(e)
				}
			}
			// Resume persisted Gateway quarantine without an existing Worker intent.
			s, e = realm.Open(root, "LAB")
			if e != nil {
				t.Fatal(e)
			}
			c.Store = s
			power.fail = true
			if e = c.maintainWorkerFailovers(context.Background()); e != nil {
				t.Fatal(e)
			}
			if power.calls != 1 || realm.WorkerFenced(s.Snapshot(), old) {
				t.Fatal("Gateway history substituted for a fresh confirmed Worker fence")
			}
			if e = c.Once(); e != nil {
				t.Fatal(e)
			}
			if a, ok := s.Snapshot().Assignments[old.ID]; !ok || a.NodeID != old.NodeID {
				t.Fatal("assignment retired before Worker power confirmation")
			}
			power.fail = false
			if e = c.maintainWorkerFailovers(context.Background()); e != nil {
				t.Fatal(e)
			}
			if power.calls != 2 || !realm.WorkerFenced(s.Snapshot(), old) {
				t.Fatal("quarantined Gateway owner never obtained exact Worker exclusion")
			}
			for i := 0; i < 3; i++ {
				if e = c.Once(); e != nil {
					t.Fatal(e)
				}
			}
			replacement := s.Snapshot().Assignments[old.ID]
			if replacement.NodeID != destination || replacement.State != realm.AssignmentActive {
				t.Fatalf("assignment cohort stuck after confirmed fence: %+v", replacement)
			}
			if e = c.maintainWorkerFailovers(context.Background()); e != nil {
				t.Fatal(e)
			}
			if power.calls != 2 {
				t.Fatal("completed assignment cohort was fenced repeatedly")
			}
		})
	}
}
