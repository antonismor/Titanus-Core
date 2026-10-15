package deploy

import (
    "strings"
    "testing"

    "github.com/antonismor/Titanus-Core/internal/model"
)

func TestCephMonitorAddressFallsBackToManagement(t *testing.T) {
    plan := model.RealmPlan{
        RealmName: "test-ceph",
        Nodes: []model.NodeSpec{
            {Name: "storage-a", ManagementIP: "10.11.101.15", Capabilities: []model.Capability{model.CapabilityStorage}},
            {Name: "storage-b", ManagementIP: "10.11.101.16", Capabilities: []model.Capability{model.CapabilityStorage}},
            {Name: "storage-c", ManagementIP: "10.11.101.17", Capabilities: []model.Capability{model.CapabilityStorage}},
        },
        Ceph: model.CephSpec{Enabled: true, PublicCIDR: "10.11.101.0/24", ClusterCIDR: "10.11.101.0/24", Replication: 3},
    }
    conf := buildCephConfig(plan, "4df38c4a-1c25-4017-90d7-b548ee8eac01")
    if !strings.Contains(conf, "mon_host = 10.11.101.15,10.11.101.16,10.11.101.17") {
        t.Fatalf("Ceph must not use empty monitor endpoints: %s", conf)
    }
    plan.Nodes[0].CephPublicIP = "10.11.101.115"
    conf = buildCephConfig(plan, "4df38c4a-1c25-4017-90d7-b548ee8eac01")
    if !strings.Contains(conf, "mon_host = 10.11.101.115,10.11.101.16,10.11.101.17") {
        t.Fatalf("explicit Ceph public IP should take precedence: %s", conf)
    }
}
