package realm

import (
	"github.com/antonismor/Titanus-Core/internal/model"
	"testing"
	"time"
)

func gatewayTransferStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	store, err := Open(root, "LAB")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if err := store.UpsertNode(Node{ID: id, State: NodeReady, LastPulse: time.Now(), Capabilities: []model.Capability{model.CapabilityGateway}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutGateway(Gateway{Name: "web", VIP: "192.0.2.10/32", Device: "eth0", Owner: "a", Standby: "b", Epoch: 1}); err != nil {
		t.Fatal(err)
	}
	return store, root
}

func TestGatewayConsecutiveWithdrawnTransfersRetainStandbyAcrossRestart(t *testing.T) {
	store, root := gatewayTransferStore(t)
	for index, destination := range []string{"b", "a", "b"} {
		oldOwner := store.Snapshot().Gateways["web"].Owner
		intent, err := store.BeginGatewayTransfer("web", destination)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.CompleteGatewayTransfer(intent, false); err != nil {
			t.Fatal(err)
		}
		store, err = Open(root, "LAB")
		if err != nil {
			t.Fatal(err)
		}
		got := store.Snapshot().Gateways["web"]
		if got.Owner != destination || got.Standby != oldOwner || got.Epoch != uint64(index+2) {
			t.Fatalf("transfer/restart lost ready standby: %+v", got)
		}
	}
}

func TestGatewayTransferNeverPromotesFencedOrUnhealthyFormerOwner(t *testing.T) {
	for _, scenario := range []struct {
		name                           string
		fenced, unhealthy, quarantined bool
	}{
		{name: "power-fenced", fenced: true},
		{name: "unreachable", unhealthy: true},
		{name: "storage-quarantined", quarantined: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			store, _ := gatewayTransferStore(t)
			intent, err := store.BeginGatewayTransfer("web", "b")
			if err != nil {
				t.Fatal(err)
			}
			node := store.Snapshot().Nodes["a"]
			if scenario.unhealthy {
				node.State = NodeUnreachable
			}

			if err := store.UpsertNode(node); err != nil {
				t.Fatal(err)
			}
			if scenario.quarantined {
				// Quarantine is controller-owned state; registration deliberately strips it.
				store.lock()
				node = store.data.Nodes["a"]
				node.StorageQuarantined = true
				store.data.Nodes["a"] = node
				err := store.commitLocked()
				store.unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := store.CompleteGatewayTransfer(intent, scenario.fenced); err != nil {
				t.Fatal(err)
			}
			state := store.Snapshot()
			if state.Gateways["web"].Standby != "" {
				t.Fatal("unsafe former owner made standby")
			}
			if scenario.fenced && !state.Nodes["a"].PowerQuarantined {
				t.Fatal("power fencing quarantine lost")
			}
		})
	}
}
