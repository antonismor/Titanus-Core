package realm

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/version"
	"strings"
	"testing"
	"time"
)

func TestHAActivationReceiptIsRevisionBoundAndSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	s, e := Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	target := map[string][]model.Capability{}
	observed := map[string]version.Capabilities{}
	required := []string{}
	c := version.Compatible()
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("n%d", i)
		cap := model.CapabilityControl
		if i >= 3 {
			cap = model.CapabilityExecution
		}
		target[id] = []model.Capability{cap}
		observed[id] = c
		required = append(required, id)
		if e = s.UpsertNode(Node{ID: id, Address: fmt.Sprintf("192.0.2.%d:9443", i+1), Capabilities: target[id], Compatibility: &c, State: NodeReady, LastPulse: time.Now()}); e != nil {
			t.Fatal(e)
		}
	}
	id, backup := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if _, e = s.ActivateNativeHA(id, backup, s.Snapshot().Revision, target); e == nil {
		t.Fatal("legacy schema activated")
	}
	for i := 1; i <= 4; i++ {
		if _, e = s.TransitionSchemaTo(fmt.Sprintf("ha-test-schema-transition-%d", i), s.Snapshot().Revision, i, observed, required); e != nil {
			t.Fatal(e)
		}
	}
	before := s.Snapshot()
	if _, e = s.ActivateNativeHA(id, backup, before.Revision-1, target); e == nil {
		t.Fatal("stale revision activated")
	}
	stale := before.Nodes["n4"]
	stale.LastPulse = time.Now().Add(-time.Minute)
	if e = s.UpsertNode(stale); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ActivateNativeHA(id, backup, s.Snapshot().Revision, target); e == nil {
		t.Fatal("stale Worker activated")
	}
	stale.LastPulse = time.Now()
	if e = s.UpsertNode(stale); e != nil {
		t.Fatal(e)
	}
	before = s.Snapshot()
	receipt, e := s.ActivateNativeHA(id, backup, before.Revision, target)
	if e != nil {
		t.Fatal(e)
	}
	after := s.Snapshot()
	if receipt.BeforeRevision != before.Revision || after.Revision != before.Revision+1 {
		t.Fatal("unbound activation")
	}
	if e = ValidateSchemaChange(before, after); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ActivateNativeHA(id, backup, 1, target); e != nil || s.Snapshot().Revision != after.Revision {
		t.Fatal("lost response retry mutated state", e)
	}
	if _, e = s.ActivateNativeHA(strings.Repeat("c", 64), backup, after.Revision, target); e == nil {
		t.Fatal("different activation accepted")
	}
	changed := cloneState(after)
	copyReceipt := *changed.HAActivation
	copyReceipt.BackupSetID = strings.Repeat("d", 64)
	changed.HAActivation = &copyReceipt
	if ValidateSchemaChange(after, changed) == nil {
		t.Fatal("receipt mutated")
	}
	changed = cloneState(after)
	changed.HAActivation.BeforeRevision = 1
	if ValidateSchemaChange(before, changed) == nil {
		t.Fatal("wrong preimage revision accepted")
	}
	s, e = Open(root, "LAB")
	if e != nil || !s.Snapshot().NativeHA || s.Snapshot().HAActivation.ID != id {
		t.Fatal("activation lost on restart", e)
	}
	worker := s.Snapshot().Nodes["n4"]
	worker.State = NodeUnreachable
	worker.LastPulse = time.Now().Add(-time.Minute)
	if e = s.UpsertNode(worker); e != nil {
		t.Fatal(e)
	}
	a := Assignment{ID: "app-001-g1", Fleet: "app", NodeID: "n4", State: AssignmentActive, Generation: 1, CreatedAt: time.Now().Add(-time.Hour), LeaseToken: "original", LeaseExpiresAt: time.Now().Add(-time.Minute)}
	if e = s.SetAssignments("app", []Assignment{a}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.BeginWorkerFailover("n4"); e != nil {
		t.Fatal("schema-four Worker fencing disabled", e)
	}
}
