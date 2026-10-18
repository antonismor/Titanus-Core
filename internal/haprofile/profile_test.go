package haprofile

import (
	"encoding/json"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/fencing"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"strings"
	"testing"
)

func profileFixture() (realm.State, model.RealmPlan) {
	p := model.RealmPlan{Version: "titanus-plan/v2", RealmName: "LAB", FabricCIDR: "10.210.0.0/16", ServiceCIDR: "10.220.0.0/16", Installation: &model.Installation{Profile: model.ProfileNativeHA, Operation: "install", ReleaseVersion: "0.4.0-rc.13", Revision: strings.Repeat("a", 40), APIPort: 9443, RaftPort: 9444, NodePrefix: 24, VXLANID: 4242, Archives: map[string]model.Archive{"amd64": {Path: "fixture", SHA256: strings.Repeat("b", 64)}, "arm64": {Path: "fixture-arm", SHA256: strings.Repeat("c", 64)}}, PowerFencing: map[string]fencing.Target{}}}
	s := realm.State{Name: p.RealmName, Network: realm.RealmNetwork{FabricCIDR: p.FabricCIDR, ServiceCIDR: p.ServiceCIDR, NodePrefix: 24, VXLANID: 4242}, Nodes: map[string]realm.Node{}}
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("n%d", i)
		cap := model.CapabilityControl
		if i > 3 {
			cap = model.CapabilityExecution
			p.Installation.PowerFencing[id] = fencing.Target{Provider: "libvirt", Address: "qemu+ssh://root@fixture.invalid/system", UUID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i)}
		}
		ip := fmt.Sprintf("192.0.2.%d", i)
		p.Nodes = append(p.Nodes, model.NodeSpec{Name: id, ManagementIP: ip, SSHUser: "root", SSHPort: 22, Capabilities: []model.Capability{cap}})
		caps := []model.Capability{cap}
		if i <= 3 {
			caps = append(caps, model.CapabilityExecution)
		}
		s.Nodes[id] = realm.Node{ID: id, Address: ip + ":9443", Capabilities: caps, State: realm.NodeReady}
	}
	return s, p
}
func TestTransitionPreflightPreservesExistingIdentityAndQuiescence(t *testing.T) {
	s, p := profileFixture()
	if e := ValidateTarget(s, p); e != nil {
		t.Fatal(e)
	}
	cases := []func(*realm.State, *model.RealmPlan){
		func(s *realm.State, p *model.RealmPlan) { s.Name = "other" },
		func(s *realm.State, p *model.RealmPlan) { s.Network.VXLANID++ },
		func(s *realm.State, p *model.RealmPlan) {
			n := s.Nodes["n1"]
			n.Address = "192.0.2.99:9443"
			s.Nodes["n1"] = n
		},
		func(s *realm.State, p *model.RealmPlan) {
			n := s.Nodes["n1"]
			n.Capabilities = []model.Capability{model.CapabilityExecution}
			s.Nodes["n1"] = n
		},
		func(s *realm.State, p *model.RealmPlan) {
			n := s.Nodes["n4"]
			n.PowerQuarantined = true
			s.Nodes["n4"] = n
		},
		func(s *realm.State, p *model.RealmPlan) {
			s.Fleets = map[string]realm.Fleet{"app": {Name: "app", Instances: 1}}
		},
		func(s *realm.State, p *model.RealmPlan) { s.Assignments = map[string]realm.Assignment{"a": {ID: "a"}} },
		func(s *realm.State, p *model.RealmPlan) {
			s.Tasks = map[string]realm.Task{"uncertain": {Phase: realm.TaskUnknown}}
		},
		func(s *realm.State, p *model.RealmPlan) {
			s.Disks = map[string]disk.Catalog{"local": {Spec: disk.Spec{Provider: disk.ProviderLocal}}}
		},
		func(s *realm.State, p *model.RealmPlan) { p.Ceph.Provision = true },
	}
	for i, change := range cases {
		s, p := profileFixture()
		change(&s, &p)
		if ValidateTarget(s, p) == nil {
			t.Fatalf("unsafe preflight %d accepted", i)
		}
	}
}
func TestPreparedIdentityBindsInventoryAndFencing(t *testing.T) {
	_, plan := profileFixture()
	p := Prepared{Format: "titanus-ha-transition/v1", Plan: plan, BackupSetID: strings.Repeat("a", 64), BackupRevision: 1, BackupStateSHA256: strings.Repeat("b", 64), FencingSHA256: map[string]string{"power-fencing.json": strings.Repeat("c", 64)}}
	for _, n := range plan.Nodes {
		role := "node"
		if n.Capabilities[0] == model.CapabilityControl {
			role = "controller"
		}
		p.Hosts = append(p.Hosts, HostPlan{Format: "titanus-backup-plan/v1", Realm: "LAB", Node: n.Name, Role: role, StateRoot: "/private/" + n.Name + "/state", ConfigRoot: "/private/" + n.Name + "/config", LedgerRoot: "/private/" + n.Name + "/ledger"})
	}
	p.ID = Digest(p)
	if e := p.Validate(); e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(p)
	var changed Prepared
	json.Unmarshal(b, &changed)
	changed.BackupSetID = strings.Repeat("d", 64)
	if changed.Validate() == nil {
		t.Fatal("altered artifact retained ID")
	}
	changed.ID = ""
	changed.Hosts[0].ConfigRoot = "/"
	changed.ID = Digest(changed)
	if changed.Validate() == nil {
		t.Fatal("unsafe root accepted")
	}
}
