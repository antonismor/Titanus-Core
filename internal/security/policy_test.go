package security

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestPolicyDefaultsAndRoundTrip(t *testing.T) {
	var p Policy
	if err := json.Unmarshal([]byte(`{}`), &p); err != nil {
		t.Fatal(err)
	}
	p.Normalize()
	if p.Seccomp != DefaultProfile || !*p.NoNewPrivileges || p.capabilityMask() != 0 {
		t.Fatalf("unsafe defaults: %+v", p)
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var restored Policy
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored, p) {
		t.Fatal("policy changed during persistence")
	}
}

func TestPolicyRejectsBypasses(t *testing.T) {
	no := false
	for _, p := range []Policy{
		{Seccomp: "unconfined"}, {Seccomp: "custom"}, {NoNewPrivileges: &no},
		{Capabilities: []string{"SYS_ADMIN"}}, {Capabilities: []string{"SETPCAP"}},
		{Capabilities: []string{"NET_ADMIN"}}, {Capabilities: []string{"NET_RAW"}},
		{Capabilities: []string{"BOGUS"}}, {Capabilities: []string{"CHOWN", "CAP_CHOWN"}},
	} {
		if err := p.Validate(); err == nil {
			t.Fatalf("accepted unsafe policy: %+v", p)
		}
	}
}

func TestExplicitCapabilityNormalization(t *testing.T) {
	p := Policy{Capabilities: []string{" net_bind_service ", "chown"}}
	p.Normalize()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.capabilityMask() != (1 | 1<<10) {
		t.Fatalf("wrong capability mask: %x", p.capabilityMask())
	}
}
