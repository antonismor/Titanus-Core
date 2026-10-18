package model

import (
	"encoding/json"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/fencing"
	"strings"
	"testing"
)

func nativeHAPlan() RealmPlan {
	p := RealmPlan{Version: "titanus-plan/v2", RealmName: "LAB", FabricCIDR: "10.210.0.0/16", ServiceCIDR: "10.220.0.0/16", Installation: &Installation{Profile: ProfileNativeHA, Operation: "install", ReleaseVersion: "0.4.0-rc.13", Revision: strings.Repeat("a", 40), Archives: map[string]Archive{"amd64": {Path: "amd.tar.gz", SHA256: strings.Repeat("b", 64)}, "arm64": {Path: "arm.tar.gz", SHA256: strings.Repeat("c", 64)}}, APIPort: 9443, RaftPort: 9444, NodePrefix: 24, VXLANID: 4242, PowerFencing: map[string]fencing.Target{}}}
	for i := 1; i <= 5; i++ {
		cap := CapabilityControl
		if i > 3 {
			cap = CapabilityExecution
		}
		name := fmt.Sprintf("n%d", i)
		p.Nodes = append(p.Nodes, NodeSpec{Name: name, ManagementIP: fmt.Sprintf("192.0.2.%d", i), SSHUser: "root", SSHPort: 22, Capabilities: []Capability{cap}})
		if i > 3 {
			p.Installation.PowerFencing[name] = fencing.Target{Provider: "libvirt", UUID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i), Address: "qemu+ssh://root@hypervisor.example/system"}
		}
	}
	return p
}
func TestNativeHAPlanRequiresSeparateVotersAndBoundFencing(t *testing.T) {
	if e := nativeHAPlan().Validate(); e != nil {
		t.Fatal(e)
	}
	cases := []func(*RealmPlan){
		func(p *RealmPlan) { p.Nodes = p.Nodes[:4]; delete(p.Installation.PowerFencing, "n5") },
		func(p *RealmPlan) { p.Nodes[0].Capabilities = append(p.Nodes[0].Capabilities, CapabilityExecution) },
		func(p *RealmPlan) { delete(p.Installation.PowerFencing, "n4") },
		func(p *RealmPlan) { v := p.Installation.PowerFencing["n4"]; p.Installation.PowerFencing["ghost"] = v },
		func(p *RealmPlan) { v := p.Installation.PowerFencing["n4"]; p.Installation.PowerFencing["n5"] = v },
		func(p *RealmPlan) {
			v := p.Installation.PowerFencing["n4"]
			v.Provider = "shell"
			p.Installation.PowerFencing["n4"] = v
		},
		func(p *RealmPlan) {
			v := p.Installation.PowerFencing["n4"]
			v.CredentialFile = "relative-secret"
			p.Installation.PowerFencing["n4"] = v
		},
		func(p *RealmPlan) { p.Installation.Operation = "upgrade" },
		func(p *RealmPlan) { p.Installation.Profile = ProfileStandard },
	}
	for i, change := range cases {
		p := nativeHAPlan()
		change(&p)
		if p.Validate() == nil {
			t.Fatalf("unsafe HA plan %d admitted", i)
		}
	}
	p := nativeHAPlan()
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "password") {
		t.Fatal("plan embeds password")
	}
}
