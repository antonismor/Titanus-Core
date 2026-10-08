package controlapi

import (
	"errors"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"testing"
)

type deniedGateway struct{}

func (deniedGateway) WithdrawGateway(string, realm.Gateway) error {
	return errors.New("old node unreachable")
}

type confirmedPower struct {
	called bool
	deny   bool
}

func (f *confirmedPower) Fence(string, func() error) error {
	f.called = true
	if f.deny {
		return errors.New("power-off unknown")
	}
	return nil
}
func TestGatewayFailoverRequiresConfirmedExclusionAndQuarantinesOldNode(t *testing.T) {
	s, err := realm.Open(t.TempDir(), "LAB")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err = s.UpsertNode(realm.Node{ID: id, State: realm.NodeReady, Capabilities: []model.Capability{model.CapabilityGateway, model.CapabilityExecution}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.PutGateway(realm.Gateway{Name: "edge", VIP: "192.0.2.99/32", Device: "eth0", Owner: "a", Standby: "b"}); err != nil {
		t.Fatal(err)
	}
	node := s.Snapshot().Nodes["a"]
	node.State = realm.NodeUnreachable
	s.UpsertNode(node)
	api := New(s, nil, nil, nil)
	api.GatewayClient = deniedGateway{}
	if err = api.ReconcileGateways(); err == nil {
		t.Fatal("transferred VIP without a fence")
	}
	if s.Snapshot().Gateways["edge"].Owner != "a" {
		t.Fatal("unconfirmed transfer committed")
	}
	power := &confirmedPower{deny: true}
	api.PowerFencer = power
	if err = api.ReconcileGateways(); err == nil {
		t.Fatal("unconfirmed power fence accepted")
	}
	power.deny = false
	if err = api.ReconcileGateways(); err != nil {
		t.Fatal(err)
	}
	state := s.Snapshot()
	if !power.called || state.Gateways["edge"].Owner != "b" || state.Gateways["edge"].Epoch != 2 || !state.Nodes["a"].PowerQuarantined {
		t.Fatal("fenced transfer state incorrect")
	}
	if err = s.Pulse("a", realm.Resources{}); err != nil {
		t.Fatal(err)
	}
	node = s.Snapshot().Nodes["a"]
	node.PowerQuarantined = false
	s.UpsertNode(node)
	if !s.Snapshot().Nodes["a"].PowerQuarantined {
		t.Fatal("re-registration cleared power quarantine")
	}
}
