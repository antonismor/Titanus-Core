package route

import (
	"testing"

	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
)

func TestBackendsOnlyActiveFleetAssignments(t *testing.T) {
	store, err := realm.Open(t.TempDir(), "LAB")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfigureNetwork(realm.RealmNetwork{FabricCIDR: "10.240.0.0/16", ServiceCIDR: "10.250.0.0/16", NodePrefix: 24, VXLANID: 4242}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertNode(realm.Node{ID: "n1", Address: "127.0.0.1", State: realm.NodeReady, Capabilities: []model.Capability{model.CapabilityExecution}}); err != nil {
		t.Fatal(err)
	}
	fleet := realm.Fleet{Name: "web", Instances: 1, MinimumAvailable: 1, Template: realm.UnitTemplate{Source: "web", Command: []string{"/bin/web"}}}
	if err := store.PutFleet(fleet); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAssignments("web", []realm.Assignment{{ID: "web-001-g1", Fleet: "web", NodeID: "n1", State: realm.AssignmentActive, NetworkAddress: "10.240.1.10"}}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(store)
	backends := manager.backends(realm.Route{Name: "public", Fleet: "web", TargetPort: 8080})
	if len(backends) != 1 || backends[0] != "10.240.1.10:8080" {
		t.Fatalf("unexpected backends %#v", backends)
	}
}


func TestBackendsExcludeAssignmentsOnUnhealthyNodes(t *testing.T) {
	store, err := realm.Open(t.TempDir(), "LAB")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfigureNetwork(realm.RealmNetwork{FabricCIDR: "10.240.0.0/16", ServiceCIDR: "10.250.0.0/16", NodePrefix: 24, VXLANID: 4242}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertNode(realm.Node{ID: "n1", Address: "127.0.0.1", State: realm.NodeUnreachable, Capabilities: []model.Capability{model.CapabilityExecution}}); err != nil {
		t.Fatal(err)
	}
	fleet := realm.Fleet{Name: "web", Instances: 1, MinimumAvailable: 1, Template: realm.UnitTemplate{Source: "web", Command: []string{"/bin/web"}}}
	if err := store.PutFleet(fleet); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAssignments("web", []realm.Assignment{{ID: "web-001-g1", Fleet: "web", NodeID: "n1", State: realm.AssignmentActive, NetworkAddress: "10.240.1.10"}}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(store)
	backends := manager.backends(realm.Route{Name: "public", Fleet: "web", TargetPort: 8080})
	if len(backends) != 0 {
		t.Fatalf("expected no unhealthy backends, got %#v", backends)
	}
}

func TestRemoteStateProviderFeedsGatewayBackends(t *testing.T) {
	state := realm.State{
		Nodes: map[string]realm.Node{
			"n1": {ID: "n1", State: realm.NodeReady},
		},
		Assignments: map[string]realm.Assignment{
			"web-001-g1": {
				ID: "web-001-g1", Fleet: "web", NodeID: "n1",
				State: realm.AssignmentActive, NetworkAddress: "10.240.1.10",
			},
		},
	}
	manager := NewManagerWithStateProvider(func() (realm.State, error) {
		return state, nil
	})
	backends := manager.backends(realm.Route{Name: "public", Fleet: "web", TargetPort: 8080})
	if len(backends) != 1 || backends[0] != "10.240.1.10:8080" {
		t.Fatalf("unexpected remote backends %#v", backends)
	}
}
