package model

import (
    "fmt"
    "path/filepath"
    "strings"
    "testing"
)

func communityTestPlan(count int, ceph bool) RealmPlan {
    nodes := make([]NodeSpec, 0, count)
    for i := 0; i < count; i++ {
        caps := []Capability{CapabilityExecution}
        if i == 0 || (ceph && i < 3) {
            caps = append(caps, CapabilityControl)
        }
        n := NodeSpec{
            Name: fmt.Sprintf("node%02d", i+1),
            ManagementIP: fmt.Sprintf("192.0.2.%d", i+10),
            FabricIP: fmt.Sprintf("10.210.1.%d", i+10),
            SSHUser: "root",
            SSHPort: 22,
            Capabilities: caps,
        }
        if ceph && i < 3 {
            n.Capabilities = append(n.Capabilities, CapabilityStorage)
            n.CephPublicIP = fmt.Sprintf("10.230.0.%d", i+10)
            n.CephClusterIP = fmt.Sprintf("10.231.0.%d", i+10)
        }
        nodes = append(nodes, n)
    }
    p := RealmPlan{
        Version: "titanus-plan/v1",
        RealmName: "COMMUNITY",
        FabricCIDR: "10.210.0.0/16",
        ServiceCIDR: "10.220.0.0/16",
        Nodes: nodes,
    }
    if ceph {
        p.Ceph = CephSpec{
            Enabled: true,
            PublicCIDR: "10.230.0.0/24",
            ClusterCIDR: "10.231.0.0/24",
            Replication: 3,
            EnableRBD: true,
            EnableCephFS: true,
        }
    }
    return p
}

func TestCommunityNodeLimitAppliesToAllTopologies(t *testing.T) {
    for _, topology := range []struct{name string; ceph bool}{
        {"standard", false},
        {"with-ceph-ha-layout", true},
    } {
        t.Run(topology.name, func(t *testing.T) {
            nine := communityTestPlan(CommunityNodeLimit, topology.ceph)
            if err := nine.Validate(); err != nil {
                t.Fatalf("9 node Community plan rejected: %v", err)
            }
            ten := communityTestPlan(CommunityNodeLimit+1, topology.ceph)
            err := ten.Validate()
            if err == nil || !strings.Contains(err.Error(), "Community Edition") {
                t.Fatalf("expected Community limit rejection, got %v", err)
            }
        })
    }
}

func TestCommunityNodeLimitCannotBeBypassedBySavedPlan(t *testing.T) {
    p := communityTestPlan(CommunityNodeLimit+1, false)
    path := filepath.Join(t.TempDir(), "plan.json")
    if err := p.Save(path); err != nil {
        t.Fatal(err)
    }
    if _, err := LoadPlan(path); err == nil || !strings.Contains(err.Error(), "Community Edition") {
        t.Fatalf("LoadPlan should reject 10-node Community plan; got %v", err)
    }
}
