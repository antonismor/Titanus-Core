package realm

import (
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/model"
)

func TestPlacementSpreadsFleet(t *testing.T) {
	state := State{
		Name: "LAB",
		Nodes: map[string]Node{
			"n1": {ID: "n1", State: NodeReady, Capabilities: []model.Capability{model.CapabilityExecution}, Resources: Resources{CPUMilliCapacity: 8000, MemoryBytes: 16 << 30}, Labels: map[string]string{"rack": "a"}},
			"n2": {ID: "n2", State: NodeReady, Capabilities: []model.Capability{model.CapabilityExecution}, Resources: Resources{CPUMilliCapacity: 8000, MemoryBytes: 16 << 30}, Labels: map[string]string{"rack": "b"}},
			"n3": {ID: "n3", State: NodeReady, Capabilities: []model.Capability{model.CapabilityExecution}, Resources: Resources{CPUMilliCapacity: 8000, MemoryBytes: 16 << 30}, Labels: map[string]string{"rack": "c"}},
		},
	}
	fleet := Fleet{
		Name: "web", Instances: 3, SpreadLabel: "rack", Generation: 1,
		Template: UnitTemplate{Source: "web", Command: []string{"/bin/web"}, MemoryBytes: 512 << 20, CPUPercent: 100},
	}
	assignments, err := NewPlacementEngine().Plan(state, fleet)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, a := range assignments {
		seen[a.NodeID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("expected 3 nodes, got %#v", assignments)
	}
}

func TestHealthTransitions(t *testing.T) {
	store, err := Open(t.TempDir(), "LAB")
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Minute)
	if err := store.UpsertNode(Node{ID: "n1", State: NodeReady, LastPulse: past, Capabilities: []model.Capability{model.CapabilityExecution}}); err != nil {
		t.Fatal(err)
	}
	changed, err := store.EvaluateHealth(time.Now(), 30*time.Second, 60*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].State != NodeUnreachable {
		t.Fatalf("unexpected transition %#v", changed)
	}
}
