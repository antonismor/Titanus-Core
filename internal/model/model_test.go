package model

import "testing"

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
