package consensus

import (
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/version"
	"testing"
	"time"
)

// Real TLS, durable replicated state and a real majority election. Physical
// fences are not performed on this same-host fixture.
func TestNativeWorkerFenceIntentSurvivesMajorityElection(t *testing.T) {
	c := newClusterSeed(t, true)
	leader := c.leader(-1)
	s := c.stores[leader]
	caps := version.Compatible()
	if e := s.UpsertNode(realm.Node{ID: "worker", Address: "192.0.2.5:9443", Capabilities: []model.Capability{model.CapabilityExecution}, State: realm.NodeUnreachable, Compatibility: &caps}); e != nil {
		t.Fatal(e)
	}
	a := realm.Assignment{ID: "web-001-g1", Fleet: "web", NodeID: "worker", CreatedAt: time.Now().Add(-time.Hour), LeaseToken: "old-token"}
	if e := s.SetAssignments("web", []realm.Assignment{a}); e != nil {
		t.Fatal(e)
	}
	f, e := s.BeginWorkerFailover("worker")
	if e != nil {
		t.Fatal(e)
	}
	c.converge(c.nodes[leader].Snapshot().Revision)
	if e = c.nodes[leader].transport.Close(); e != nil {
		t.Fatal(e)
	}
	next := c.leader(leader)
	if s.CheckWorkerFailover(f) == nil || s.CompleteWorkerFailover(f) == nil {
		t.Fatal("isolated former leader authorized fencing or replacement")
	}
	retry, e := c.stores[next].BeginWorkerFailover("worker")
	if e != nil || retry.ID != f.ID {
		t.Fatal("new leader lost durable fencing intent", e)
	}
	if !c.stores[next].Snapshot().Nodes["worker"].PowerQuarantined {
		t.Fatal("majority election lost quarantine")
	}
	if e = c.stores[next].CompleteWorkerFailover(retry); e != nil {
		t.Fatal(e)
	}
	if !realm.WorkerFenced(c.stores[next].Snapshot(), a) {
		t.Fatal("majority fence commit missing")
	}
	if realm.WorkerFenced(s.Snapshot(), a) {
		t.Fatal("minority saw uncommitted completion")
	}
	revision := c.stores[next].Snapshot().Revision
	if e = c.nodes[leader].Close(); e != nil {
		t.Fatal(e)
	}
	c.nodes[leader] = nil
	c.open(leader)
	c.converge(revision)
	if !realm.WorkerFenced(c.stores[leader].Snapshot(), a) {
		t.Fatal("rejoined voter lost completed fence")
	}

}
