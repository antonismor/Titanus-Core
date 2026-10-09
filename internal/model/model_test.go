package model

import (
	"strings"
	"testing"
)

func TestValidRealmPlan(t *testing.T) {
	plan := RealmPlan{
		RealmName:   "LAB",
		FabricCIDR:  "10.210.0.0/16",
		ServiceCIDR: "10.220.0.0/16",
		Nodes: []NodeSpec{{
			Name:         "node01",
			ManagementIP: "192.0.2.10",
			FabricIP:     "10.210.1.10",
			SSHUser:      "root",
			SSHPort:      22,
			Capabilities: []Capability{CapabilityControl, CapabilityExecution},
		}},
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
}

func TestDuplicateIPRejected(t *testing.T) {
	plan := RealmPlan{
		RealmName:   "LAB",
		FabricCIDR:  "10.210.0.0/16",
		ServiceCIDR: "10.220.0.0/16",
		Nodes: []NodeSpec{
			{Name: "node01", ManagementIP: "192.0.2.10", SSHUser: "root", SSHPort: 22, Capabilities: []Capability{CapabilityControl}},
			{Name: "node02", ManagementIP: "192.0.2.10", SSHUser: "root", SSHPort: 22, Capabilities: []Capability{CapabilityExecution}},
		},
	}
	if err := plan.Validate(); err == nil {
		t.Fatal("expected duplicate IP validation error")
	}
}

func TestCephReplicationCannotExceedStorageNodes(t *testing.T) {
	plan := RealmPlan{
		RealmName:   "LAB",
		FabricCIDR:  "10.210.0.0/16",
		ServiceCIDR: "10.220.0.0/16",
		Nodes: []NodeSpec{{
			Name: "node01", ManagementIP: "192.0.2.10", FabricIP: "10.210.1.10",
			CephPublicIP: "10.230.0.10", CephClusterIP: "10.231.0.10",
			SSHUser: "root", SSHPort: 22,
			Capabilities: []Capability{CapabilityControl, CapabilityExecution, CapabilityStorage},
		}},
		Ceph: CephSpec{
			Enabled: true, PublicCIDR: "10.230.0.0/24", ClusterCIDR: "10.231.0.0/24",
			Replication: 3, EnableRBD: true,
		},
	}
	if err := plan.Validate(); err == nil {
		t.Fatal("expected replication/storage-node validation error")
	}
}

func TestVersionedPlanRejectsUnsafeNetworkIdentityAndLifecycle(t *testing.T) {
	base := RealmPlan{Version: "titanus-plan/v2", RealmName: "LAB", FabricCIDR: "10.210.0.0/16", ServiceCIDR: "10.220.0.0/16", Nodes: []NodeSpec{{Name: "node", ManagementIP: "192.0.2.1", FabricIP: "192.0.2.2", SSHUser: "root", SSHPort: 22, Capabilities: []Capability{CapabilityControl}}}, Installation: &Installation{Operation: "rollback", ReleaseVersion: "0.4.0-rc.4", Revision: strings.Repeat("a", 40), APIPort: 9443, RaftPort: 9444, NodePrefix: 24, VXLANID: 4242}}
	if e := base.Validate(); e != nil {
		t.Fatal(e)
	}
	cases := []func(*RealmPlan){
		func(p *RealmPlan) { p.Installation = nil },
		func(p *RealmPlan) { p.RealmName = "../escape" },
		func(p *RealmPlan) { p.Nodes[0].Name = "node\nENV=bad" },
		func(p *RealmPlan) { p.Nodes[0].SSHUser = "-oProxyCommand=bad" },
		func(p *RealmPlan) { p.Nodes[0].Capabilities = []Capability{CapabilityControl, CapabilityControl} },
		func(p *RealmPlan) { p.Nodes[0].Capabilities = append(p.Nodes[0].Capabilities, Capability("UNKNOWN")) },
		func(p *RealmPlan) { p.Nodes[0].FabricIP = "10.210.1.1" },
		func(p *RealmPlan) { p.Nodes[0].ManagementIP = "::1" },
		func(p *RealmPlan) { p.ServiceCIDR = "10.210.0.0/24" },
		func(p *RealmPlan) { p.Installation.NodePrefix = 16 },
		func(p *RealmPlan) { p.Installation.NodePrefix = 31 },
		func(p *RealmPlan) { p.Ceph.Provision = true },
	}
	for i, mutate := range cases {
		p := base
		inst := *base.Installation
		p.Installation = &inst
		p.Nodes = append([]NodeSpec(nil), base.Nodes...)
		p.Nodes[0].Capabilities = append([]Capability(nil), base.Nodes[0].Capabilities...)
		mutate(&p)
		if e := p.Validate(); e == nil {
			t.Fatalf("unsafe case %d accepted", i)
		}
	}
}
