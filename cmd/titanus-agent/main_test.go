package main

import (
	"testing"

	"github.com/antonismor/Titanus-Core/internal/realm"
)

func TestServicesFromStateUsesOnlyActiveHealthyAssignments(t *testing.T) {
	state := realm.State{
		Nodes: map[string]realm.Node{
			"ready": {ID: "ready", State: realm.NodeReady},
			"bad":   {ID: "bad", State: realm.NodeUnreachable},
		},
		Routes: map[string]realm.Route{
			"web": {
				Name: "web", Fleet: "web", ServiceIP: "10.250.0.10",
				ListenPort: 8080, TargetPort: 80, Protocol: "tcp",
			},
		},
		Assignments: map[string]realm.Assignment{
			"a": {ID: "a", Fleet: "web", NodeID: "ready", State: realm.AssignmentActive, NetworkAddress: "10.240.1.10"},
			"b": {ID: "b", Fleet: "web", NodeID: "bad", State: realm.AssignmentActive, NetworkAddress: "10.240.2.10"},
			"c": {ID: "c", Fleet: "web", NodeID: "ready", State: realm.AssignmentImpaired, NetworkAddress: "10.240.1.11"},
		},
	}

	services := servicesFromState(state)
	if len(services) != 1 {
		t.Fatalf("expected one service, got %#v", services)
	}
	service := services[0]
	if service.Address != "10.250.0.10" || service.Port != 8080 || len(service.Backends) != 1 {
		t.Fatalf("unexpected service: %#v", service)
	}
	if service.Backends[0].Address != "10.240.1.10" || service.Backends[0].Port != 80 {
		t.Fatalf("unexpected backend: %#v", service.Backends[0])
	}
}

func TestServicesFromStateSkipsLegacyRouteWithoutServiceIP(t *testing.T) {
	state := realm.State{
		Routes: map[string]realm.Route{
			"legacy": {Name: "legacy", Fleet: "web", ListenPort: 8080, TargetPort: 80, Protocol: "tcp"},
		},
	}
	if services := servicesFromState(state); len(services) != 0 {
		t.Fatalf("expected no service for legacy Route, got %#v", services)
	}
}
