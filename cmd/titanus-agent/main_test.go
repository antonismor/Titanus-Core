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


func TestPoliciesFromStateProtectsImpairedDestinationAndAllowsHealthySource(t *testing.T) {
	state := realm.State{
		Nodes: map[string]realm.Node{
			"api-ready": {ID: "api-ready", State: realm.NodeReady},
			"api-bad":   {ID: "api-bad", State: realm.NodeUnreachable},
			"web-bad":   {ID: "web-bad", State: realm.NodeUnreachable},
		},
		Policies: map[string]realm.NetworkPolicy{
			"web-ingress": {
				Name: "web-ingress", Fleet: "web", DefaultDeny: true,
				Ingress: []realm.NetworkPolicyRule{{
					FromFleet: "api", FromCIDR: "192.0.2.0/24",
					Protocol: "tcp", Ports: []int{443},
				}},
			},
		},
		Assignments: map[string]realm.Assignment{
			"web": {
				ID: "web", Fleet: "web", NodeID: "web-bad",
				State: realm.AssignmentImpaired, NetworkAddress: "10.240.2.10",
			},
			"api-good": {
				ID: "api-good", Fleet: "api", NodeID: "api-ready",
				State: realm.AssignmentActive, NetworkAddress: "10.240.1.10",
			},
			"api-bad": {
				ID: "api-bad", Fleet: "api", NodeID: "api-bad",
				State: realm.AssignmentActive, NetworkAddress: "10.240.3.10",
			},
		},
	}

	policies := policiesFromState(state)
	if len(policies) != 1 {
		t.Fatalf("expected one policy, got %#v", policies)
	}
	policy := policies[0]
	if len(policy.Destinations) != 1 || policy.Destinations[0] != "10.240.2.10" {
		t.Fatalf("impaired destination must remain protected: %#v", policy.Destinations)
	}
	if len(policy.Rules) != 1 {
		t.Fatalf("unexpected rules: %#v", policy.Rules)
	}
	rule := policy.Rules[0]
	if rule.AnySource {
		t.Fatal("Fleet/CIDR rule must not become any-source")
	}
	if len(rule.Sources) != 2 ||
		(rule.Sources[0] != "10.240.1.10/32" && rule.Sources[1] != "10.240.1.10/32") ||
		(rule.Sources[0] != "192.0.2.0/24" && rule.Sources[1] != "192.0.2.0/24") {
		t.Fatalf("expected only healthy Fleet source plus explicit CIDR, got %#v", rule.Sources)
	}
}

func TestPoliciesFromStateSupportsAnySourceRule(t *testing.T) {
	state := realm.State{
		Policies: map[string]realm.NetworkPolicy{
			"dns": {
				Name: "dns", Fleet: "resolver", DefaultDeny: true,
				Ingress: []realm.NetworkPolicyRule{{Protocol: "udp", Ports: []int{53}}},
			},
		},
		Assignments: map[string]realm.Assignment{
			"dns-1": {
				ID: "dns-1", Fleet: "resolver", State: realm.AssignmentActive,
				NetworkAddress: "10.240.4.10",
			},
		},
	}
	policies := policiesFromState(state)
	if len(policies) != 1 || len(policies[0].Rules) != 1 || !policies[0].Rules[0].AnySource {
		t.Fatalf("expected any-source policy rule, got %#v", policies)
	}
}

func TestHealthySourceFleetWithoutReadyAssignmentsFailsClosed(t *testing.T) {
	state := realm.State{
		Nodes: map[string]realm.Node{
			"bad": {ID: "bad", State: realm.NodeUnreachable},
		},
		Policies: map[string]realm.NetworkPolicy{
			"web": {
				Name: "web", Fleet: "web", DefaultDeny: true,
				Ingress: []realm.NetworkPolicyRule{{FromFleet: "api", Protocol: "tcp", Ports: []int{80}}},
			},
		},
		Assignments: map[string]realm.Assignment{
			"web": {ID: "web", Fleet: "web", State: realm.AssignmentActive, NetworkAddress: "10.240.2.10"},
			"api": {ID: "api", Fleet: "api", NodeID: "bad", State: realm.AssignmentActive, NetworkAddress: "10.240.3.10"},
		},
	}
	policies := policiesFromState(state)
	if len(policies) != 1 || len(policies[0].Destinations) != 1 {
		t.Fatalf("expected protected destination, got %#v", policies)
	}
	if len(policies[0].Rules) != 1 || len(policies[0].Rules[0].Sources) != 0 || policies[0].Rules[0].AnySource {
		t.Fatalf("unhealthy source must not be permitted: %#v", policies[0].Rules)
	}
}

func TestPoliciesFromStateProjectsEgressFleetAndCIDR(t *testing.T) {
	state := realm.State{
		Nodes: map[string]realm.Node{
			"api": {ID: "api", State: realm.NodeReady},
			"db":  {ID: "db", State: realm.NodeReady},
		},
		Policies: map[string]realm.NetworkPolicy{
			"api-egress": {
				Name: "api-egress", Fleet: "api", DefaultDenyEgress: true,
				Egress: []realm.NetworkPolicyEgressRule{{
					ToFleet: "db", ToCIDR: "192.0.2.0/24",
					Protocol: "tcp", Ports: []int{5432},
				}},
			},
		},
		Assignments: map[string]realm.Assignment{
			"api-1": {ID: "api-1", Fleet: "api", NodeID: "api", State: realm.AssignmentActive, NetworkAddress: "10.240.1.10"},
			"db-1":  {ID: "db-1", Fleet: "db", NodeID: "db", State: realm.AssignmentActive, NetworkAddress: "10.240.2.10"},
		},
	}
	policies := policiesFromState(state)
	if len(policies) != 1 {
		t.Fatalf("expected one policy, got %#v", policies)
	}
	policy := policies[0]
	if !policy.DefaultDenyEgress || len(policy.Sources) != 1 || policy.Sources[0] != "10.240.1.10" {
		t.Fatalf("unexpected protected egress sources: %#v", policy)
	}
	if len(policy.Egress) != 1 {
		t.Fatalf("unexpected egress rules: %#v", policy.Egress)
	}
	rule := policy.Egress[0]
	if rule.AnyDestination {
		t.Fatal("Fleet/CIDR egress rule must not become any-destination")
	}
	if len(rule.Destinations) != 2 ||
		(rule.Destinations[0] != "10.240.2.10/32" && rule.Destinations[1] != "10.240.2.10/32") ||
		(rule.Destinations[0] != "192.0.2.0/24" && rule.Destinations[1] != "192.0.2.0/24") {
		t.Fatalf("unexpected egress destinations: %#v", rule.Destinations)
	}
}

