package realm

import (
	"encoding/json"
	"os"
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


func TestRouteGetsStableUniqueServiceAddress(t *testing.T) {
	store, err := Open(t.TempDir(), "LAB")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfigureNetwork(RealmNetwork{
		FabricCIDR: "10.240.0.0/16", ServiceCIDR: "10.250.0.0/24",
		NodePrefix: 24, VXLANID: 4242,
	}); err != nil {
		t.Fatal(err)
	}
	for _, fleetName := range []string{"web", "api"} {
		if err := store.PutFleet(Fleet{
			Name: fleetName, Instances: 1, MinimumAvailable: 1,
			Template: UnitTemplate{Source: fleetName, Command: []string{"/bin/app"}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	web, err := store.PutRoute(Route{Name: "web", Fleet: "web", ListenPort: 8080, TargetPort: 80})
	if err != nil {
		t.Fatal(err)
	}
	api, err := store.PutRoute(Route{Name: "api", Fleet: "api", ListenPort: 8081, TargetPort: 8080})
	if err != nil {
		t.Fatal(err)
	}
	if web.ServiceIP != "10.250.0.10" || api.ServiceIP != "10.250.0.11" {
		t.Fatalf("unexpected Service IPs: web=%s api=%s", web.ServiceIP, api.ServiceIP)
	}
	updated, err := store.PutRoute(Route{
		Name: "web", Fleet: "web", ListenPort: 9090, TargetPort: 80,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ServiceIP != web.ServiceIP {
		t.Fatalf("Route Service IP changed from %s to %s", web.ServiceIP, updated.ServiceIP)
	}
}

func TestRouteRejectsServiceAddressOutsideServiceCIDR(t *testing.T) {
	store, err := Open(t.TempDir(), "LAB")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfigureNetwork(RealmNetwork{
		FabricCIDR: "10.240.0.0/16", ServiceCIDR: "10.250.0.0/24",
		NodePrefix: 24, VXLANID: 4242,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutFleet(Fleet{
		Name: "web", Instances: 1, MinimumAvailable: 1,
		Template: UnitTemplate{Source: "web", Command: []string{"/bin/app"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutRoute(Route{
		Name: "web", Fleet: "web", ServiceIP: "10.251.0.10",
		ListenPort: 8080, TargetPort: 80,
	}); err == nil {
		t.Fatal("expected out-of-range Service IP to be rejected")
	}
}

func TestRealmNetworkRejectsOverlappingFabricAndServiceCIDRs(t *testing.T) {
	store, err := Open(t.TempDir(), "LAB")
	if err != nil {
		t.Fatal(err)
	}
	err = store.ConfigureNetwork(RealmNetwork{
		FabricCIDR: "10.240.0.0/16", ServiceCIDR: "10.240.10.0/24",
		NodePrefix: 24, VXLANID: 4242,
	})
	if err == nil {
		t.Fatal("expected overlapping Fabric and Service CIDRs to be rejected")
	}
}


func TestOpenMigratesLegacyRoutesToServiceCIDR(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root, "LAB")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfigureNetwork(RealmNetwork{
		FabricCIDR: "10.240.0.0/16", ServiceCIDR: "10.250.0.0/24",
		NodePrefix: 24, VXLANID: 4242,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutFleet(Fleet{
		Name: "web", Instances: 1, MinimumAvailable: 1,
		Template: UnitTemplate{Source: "web", Command: []string{"/bin/app"}},
	}); err != nil {
		t.Fatal(err)
	}
	route, err := store.PutRoute(Route{Name: "web", Fleet: "web", ListenPort: 8080, TargetPort: 80})
	if err != nil {
		t.Fatal(err)
	}

	legacy := store.Snapshot()
	r := legacy.Routes["web"]
	r.ServiceIP = ""
	legacy.Routes["web"] = r
	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(root, "LAB")
	if err != nil {
		t.Fatal(err)
	}
	migrated := reopened.Snapshot().Routes["web"]
	if migrated.ServiceIP != route.ServiceIP {
		t.Fatalf("expected migrated Service IP %s, got %s", route.ServiceIP, migrated.ServiceIP)
	}
}
