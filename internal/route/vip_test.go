package route

import (
	"errors"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"testing"
)

type fakeVIP struct {
	present       bool
	adds, removes int
	deny          bool
}

func (f *fakeVIP) Add(realm.Gateway) error { f.present = true; f.adds++; return nil }
func (f *fakeVIP) Remove(realm.Gateway) error {
	if f.deny {
		return errors.New("kernel withdrawal failed")
	}
	f.present = false
	f.removes++
	return nil
}
func (f *fakeVIP) Has(realm.Gateway) (bool, error) { return f.present, nil }
func TestVIPWithdrawalPersistsEpochAcrossStaleStateAndRestart(t *testing.T) {
	g := realm.Gateway{Name: "edge", VIP: "192.0.2.99/32", Device: "eth0", Owner: "a", Epoch: 1}
	state := realm.State{Nodes: map[string]realm.Node{"a": {ID: "a", State: realm.NodeReady}}, Gateways: map[string]realm.Gateway{"edge": g}}
	root := t.TempDir()
	kernel := &fakeVIP{}
	m := NewManagerWithStateProvider(func() (realm.State, error) { return state, nil })
	if err := m.ConfigureVIP(root, "a", kernel); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if !kernel.present || kernel.adds != 1 {
		t.Fatal("VIP not acquired")
	}
	if err := m.Withdraw(g); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if kernel.present {
		t.Fatal("stale committed state re-acquired withdrawn epoch")
	}
	restarted := NewManagerWithStateProvider(func() (realm.State, error) { return state, nil })
	if err := restarted.ConfigureVIP(root, "a", kernel); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if kernel.present {
		t.Fatal("restart forgot durable withdrawal")
	}
	g.Epoch = 2
	state.Gateways["edge"] = g
	if err := restarted.Reconcile(); err != nil {
		t.Fatal(err)
	}
	if !kernel.present {
		t.Fatal("new committed epoch cannot acquire")
	}
	kernel.deny = true
	if err := restarted.Withdraw(g); err == nil {
		t.Fatal("acknowledged failed kernel withdrawal")
	}
}
func TestGatewayStateLossWithdrawsVIP(t *testing.T) {
	g := realm.Gateway{Name: "edge", VIP: "192.0.2.99/32", Device: "eth0", Owner: "a", Epoch: 1}
	unavailable := false
	m := NewManagerWithStateProvider(func() (realm.State, error) {
		if unavailable {
			return realm.State{}, errors.New("no quorum")
		}
		return realm.State{Gateways: map[string]realm.Gateway{"edge": g}, Nodes: map[string]realm.Node{"a": {ID: "a", State: realm.NodeReady}}}, nil
	})
	kernel := &fakeVIP{}
	if err := m.ConfigureVIP(t.TempDir(), "a", kernel); err != nil {
		t.Fatal(err)
	}
	if err := m.Reconcile(); err != nil {
		t.Fatal(err)
	}
	unavailable = true
	if err := m.Reconcile(); err == nil {
		t.Fatal("lost authority ignored")
	}
	if kernel.present {
		t.Fatal("VIP retained on quorum state loss")
	}
}
