package model

import (
	"reflect"
	"testing"
)

func TestNativeHACephRequiresThreeStorageHostsAndCopies(t *testing.T) {
	p := nativeHAPlan()
	p.Ceph = CephSpec{Enabled: true, PublicCIDR: "10.230.0.0/24", ClusterCIDR: "10.231.0.0/24", Replication: 3, EnableRBD: true}
	for i := 0; i < 3; i++ {
		p.Nodes[i].Capabilities = append(p.Nodes[i].Capabilities, CapabilityStorage)
	}
	if e := p.Validate(); e != nil {
		t.Fatal(e)
	}
	p.Ceph.Replication = 2
	if p.Validate() == nil {
		t.Fatal("two copies admitted HA")
	}
	p.Ceph.Replication = 3
	p.Nodes[2].Capabilities = p.Nodes[2].Capabilities[:1]
	if p.Validate() == nil {
		t.Fatal("two storage hosts admitted")
	}
	p.Nodes[2].Capabilities = append(p.Nodes[2].Capabilities, CapabilityStorage)
	p.Nodes[3].Capabilities = append(p.Nodes[3].Capabilities, CapabilityStorage)
	if len(p.CephMonitorNodes()) != 3 {
		t.Fatal("four storage hosts changed three-monitor policy")
	}
	p.Ceph.EnableRGW = true
	if p.Validate() == nil {
		t.Fatal("unimplemented RGW HA advertised")
	}
	p.Ceph.EnableRGW = false
	p.Installation.Profile = ProfileCustom
	p.Installation.PowerFencing = nil
	p.Ceph.Replication = 1
	if e := p.Validate(); e != nil {
		t.Fatal("legacy Custom replication rejected", e)
	}
	if !reflect.DeepEqual(p.CephMonitorNodes(), p.StorageNodes()) {
		t.Fatal("legacy monitor policy changed")
	}
}
