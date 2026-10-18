package realm

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/version"
)

func nativeWorkerStore(t *testing.T) (*Store, string, Assignment) {
	t.Helper()
	root := t.TempDir()
	s, e := Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SeedNativeHA(); e != nil {
		t.Fatal(e)
	}
	caps := version.Compatible()
	if e = s.UpsertNode(Node{ID: "worker", Address: "192.0.2.10:9443", Capabilities: []model.Capability{model.CapabilityExecution}, Compatibility: &caps, State: NodeUnreachable}); e != nil {
		t.Fatal(e)
	}
	a := Assignment{ID: "app-001-g1", Fleet: "app", NodeID: "worker", State: AssignmentActive, Generation: 1, CreatedAt: time.Now().Add(-time.Hour), LeaseToken: "original", LeaseExpiresAt: time.Now().Add(-time.Minute)}
	if e = s.SetAssignments("app", []Assignment{a}); e != nil {
		t.Fatal(e)
	}
	return s, root, a
}

func TestWorkerFenceCrashResumeAndExplicitRecovery(t *testing.T) {
	s, root, a := nativeWorkerStore(t)
	f, e := s.BeginWorkerFailover("worker")
	if e != nil {
		t.Fatal(e)
	}
	if !s.Snapshot().Nodes["worker"].PowerQuarantined || WorkerFenced(s.Snapshot(), a) {
		t.Fatal("unconfirmed power fence admitted retirement")
	}
	s, e = Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	retry, e := s.BeginWorkerFailover("worker")
	if e != nil || retry.ID != f.ID {
		t.Fatal("restart lost intent", e)
	}
	if e = s.Pulse("worker", Resources{}); e != nil {
		t.Fatal(e)
	}
	n := s.Snapshot().Nodes["worker"]
	n.PowerQuarantined = false
	if e = s.UpsertNode(n); e != nil {
		t.Fatal(e)
	}
	if eligible(s.Snapshot().Nodes["worker"], Fleet{}) {
		t.Fatal("Pulse/registration cleared quarantine")
	}
	if e = s.RecoverPowerFencedGatewayNode("worker", strings.Repeat("evidence", 12)); e == nil {
		t.Fatal("recovered pending power fence")
	}
	if e = s.CompleteWorkerFailover(retry); e != nil {
		t.Fatal(e)
	}
	if !WorkerFenced(s.Snapshot(), a) {
		t.Fatal("confirmed fence missing")
	}
	unrelated := a
	unrelated.LeaseToken = "successor"
	if WorkerFenced(s.Snapshot(), unrelated) {
		t.Fatal("fence admitted different assignment incarnation")
	}
	if e = s.RecoverPowerFencedGatewayNode("worker", strings.Repeat("evidence", 12)); e == nil {
		t.Fatal("recovered node retaining assignment")
	}
	if e = s.SetAssignments("app", nil); e != nil {
		t.Fatal(e)
	}
	if e = s.RecoverPowerFencedGatewayNode("worker", "short"); e == nil {
		t.Fatal("empty recovery proof")
	}
	if e = s.RecoverPowerFencedGatewayNode("worker", strings.Repeat("reviewed clean runtime and old mounts excluded; ", 2)); e != nil {
		t.Fatal(e)
	}
	s, e = Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	if s.Snapshot().Nodes["worker"].PowerQuarantined || s.Snapshot().WorkerFailovers[f.ID].RecoveredAt.IsZero() || WorkerFenced(s.Snapshot(), a) {
		t.Fatal("recovery not durable or old fence reusable")
	}
	n = s.Snapshot().Nodes["worker"]
	n.State = NodeUnreachable
	if e = s.UpsertNode(n); e != nil {
		t.Fatal(e)
	}
	a.LeaseToken = "second-run"
	if e = s.SetAssignments("app", []Assignment{a}); e != nil {
		t.Fatal(e)
	}
	next, e := s.BeginWorkerFailover("worker")
	if e != nil || next.ID == f.ID {
		t.Fatal("second failure reused old exclusion", e)
	}
}

func TestNativeHASeedProtectsLegacyStateAndSchema(t *testing.T) {
	s, _, _ := nativeWorkerStore(t)
	if s.Snapshot().SchemaVersion != 3 {
		t.Fatal("missing rollback floor")
	}
	before := s.Snapshot()
	if e := s.SeedNativeHA(); e == nil {
		t.Fatal("seed admitted existing installation")
	}
	if s.Snapshot().Revision != before.Revision {
		t.Fatal("rejected seed mutated existing state")
	}
	old := version.Compatible()
	old.ReadSchemaMax = 2
	old.WriteSchemaMax = 2
	if e := s.UpsertNode(Node{ID: "old", Compatibility: &old}); e == nil {
		t.Fatal("old binary admitted Native HA schema")
	}
	if e := s.UpsertNode(Node{ID: "combined", Compatibility: ptrCaps(), Capabilities: []model.Capability{model.CapabilityControl, model.CapabilityExecution}}); e == nil {
		t.Fatal("voter eligible for Worker fencing")
	}
	invalid := s.Snapshot()
	invalid.SchemaVersion = 2
	if ValidateSchema(invalid) == nil {
		t.Fatal("new HA policy admitted old schema")
	}
	before = s.Snapshot()
	after := cloneState(before)
	after.NativeHA = false
	if ValidateSchemaChange(before, after) == nil {
		t.Fatal("HA policy downgraded")
	}
}
func ptrCaps() *version.Capabilities { v := version.Compatible(); return &v }

type workerConsensus struct {
	state     State
	deny      bool
	failApply bool
}

func (f *workerConsensus) Snapshot() State { return cloneState(f.state) }
func (f *workerConsensus) CheckLeader() error {
	if f.deny {
		return fmt.Errorf("no quorum")
	}
	return nil
}
func (f *workerConsensus) Apply(s State) error {
	if f.deny || f.failApply {
		return fmt.Errorf("commit rejected")
	}
	f.state = cloneState(s)
	return nil
}

func TestWorkerFenceQuorumLossAndFailedCommit(t *testing.T) {
	s, _, a := nativeWorkerStore(t)
	gate := &workerConsensus{state: s.Snapshot()}
	if e := s.EnableConsensus(gate); e != nil {
		t.Fatal(e)
	}
	gate.deny = true
	if _, e := s.BeginWorkerFailover("worker"); e == nil || len(s.Snapshot().WorkerFailovers) > 0 {
		t.Fatal("minority created fence intent")
	}
	gate.deny = false
	gate.failApply = true
	if _, e := s.BeginWorkerFailover("worker"); e == nil || s.Snapshot().Nodes["worker"].PowerQuarantined {
		t.Fatal("rejected intent leaked quarantine")
	}
	gate.failApply = false
	f, e := s.BeginWorkerFailover("worker")
	if e != nil {
		t.Fatal(e)
	}
	gate.deny = true
	if s.CheckWorkerFailover(f) == nil || s.CompleteWorkerFailover(f) == nil || WorkerFenced(s.Snapshot(), a) {
		t.Fatal("lost quorum admitted fence completion")
	}
	gate.deny = false
	gate.failApply = true
	if s.CompleteWorkerFailover(f) == nil || WorkerFenced(s.Snapshot(), a) {
		t.Fatal("uncommitted physical fence admitted handoff")
	}
	gate.failApply = false
	if e = s.CompleteWorkerFailover(f); e != nil {
		t.Fatal(e)
	}
	after := s.Snapshot()
	forged := cloneState(after)
	delete(forged.WorkerFailovers, f.ID)
	if ValidateSchemaChange(after, forged) == nil {
		t.Fatal("fence receipt removed")
	}
}
