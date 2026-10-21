package model

import "testing"

func TestHostMayReuseManagementIPForFabricAndCephButNotAcrossHosts(t *testing.T) {
	p := RealmPlan{Version: "titanus-plan/v1", RealmName: "LAB", FabricCIDR: "10.210.0.0/16", ServiceCIDR: "10.220.0.0/16", Nodes: []NodeSpec{{Name: "first", ManagementIP: "192.168.77.10", FabricIP: "192.168.77.10", CephPublicIP: "192.168.77.10", CephClusterIP: "192.168.77.10", SSHUser: "root", SSHPort: 22, Capabilities: []Capability{CapabilityControl}}}}
	if err := p.Validate(); err != nil {
		t.Fatal("single-interface host rejected", err)
	}
	other := p.Nodes[0]
	other.Name = "second"
	p.Nodes = append(p.Nodes, other)
	if err := p.Validate(); err == nil {
		t.Fatal("two hosts sharing the same address accepted")
	}
}
