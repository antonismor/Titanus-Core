package reconcile

import (
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/lease"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
)

type healthNode struct {
	ready bool
	last  unitruntime.Spec
}

func (n *healthNode) EnsureUnit(_ string, spec unitruntime.Spec) (unitruntime.State, error) {
	n.last = spec
	return unitruntime.State{}, nil
}
func (n *healthNode) StartUnit(string, string) (unitruntime.State, error) {
	return unitruntime.State{Status: unitruntime.StatusActive, Ready: n.ready, NetworkAddress: "10.240.1.10"}, nil
}
func (*healthNode) StopUnit(string, string) (unitruntime.State, error) {
	return unitruntime.State{}, nil
}
func (*healthNode) DeleteUnit(string, string) error                    { return nil }
func (*healthNode) EnsureSource(string, string, *source.Manager) error { return nil }
func (*healthNode) RenewLease(_, _, token string, ttl time.Duration) (lease.Record, error) {
	return lease.Record{Token: token, ExpiresAt: time.Now().Add(ttl)}, nil
}
func (*healthNode) RevokeLease(string, string, string) error { return nil }

func TestReadinessControlsAssignmentAvailability(t *testing.T) {
	store, err := realm.Open(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertNode(realm.Node{ID: "node", Address: "127.0.0.1", State: realm.NodeReady, Capabilities: []model.Capability{model.CapabilityExecution}, Resources: realm.Resources{MemoryBytes: 4 << 30, CPUMilliCapacity: 4000}}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutFleet(realm.Fleet{Name: "web", Instances: 1, MinimumAvailable: 1, Template: realm.UnitTemplate{Source: "source", Command: []string{"/bin/web"}, Health: unitruntime.Health{Readiness: &unitruntime.Probe{Protocol: "http", Port: 8080, Path: "/ready"}}}}); err != nil {
		t.Fatal(err)
	}
	node := &healthNode{}
	controller := Controller{Store: store, Nodes: node}
	for _, ready := range []bool{false, true, false} {
		node.ready = ready
		if err := controller.Once(); err != nil {
			t.Fatal(err)
		}
		state := store.Snapshot()
		if len(state.Assignments) != 1 {
			t.Fatal("unexpected assignments")
		}
		for _, a := range state.Assignments {
			if (a.State == realm.AssignmentActive) != ready {
				t.Fatalf("unready workload was considered available: %+v", a)
			}
		}
		if node.last.Health.Readiness == nil || node.last.Health.Readiness.Port != 8080 {
			t.Fatal("readiness lost in Fleet propagation")
		}
	}
}
