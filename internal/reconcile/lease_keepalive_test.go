package reconcile

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/lease"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"sync"
	"testing"
	"time"
)

type slowLeaseNode struct {
	healthNode
	mu        sync.Mutex
	block     bool
	entered   chan struct{}
	release   chan struct{}
	renewals  map[string]int
	stopped   map[string]bool
	afterStop bool
	refuse    bool
}

func (n *slowLeaseNode) EnsureSource(_, name string, _ *source.Manager) error {
	n.mu.Lock()
	block := n.block && name == "slow"
	entered := n.entered
	release := n.release
	n.mu.Unlock()
	if block {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
	}
	return nil
}
func (n *slowLeaseNode) RenewLease(_, id, token string, ttl time.Duration) (lease.Record, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.stopped[id] {
		n.afterStop = true
	}
	if n.refuse {
		return lease.Record{}, fmt.Errorf("no quorum authority")
	}
	n.renewals[id]++
	return lease.Record{Token: token, ExpiresAt: time.Now().Add(ttl)}, nil
}
func (n *slowLeaseNode) StopUnit(_, id string) (unitruntime.State, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.stopped[id] = true
	return unitruntime.State{}, nil
}
func leaseKeeperFixture(t *testing.T) (*realm.Store, *Controller, *slowLeaseNode) {
	t.Helper()
	s, e := realm.Open(t.TempDir(), "test")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.UpsertNode(realm.Node{ID: "node", Address: "node", State: realm.NodeReady, Capabilities: []model.Capability{model.CapabilityExecution}, Resources: realm.Resources{MemoryBytes: 4 << 30, CPUMilliCapacity: 4000}}); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"fast", "slow"} {
		if e = s.PutFleet(realm.Fleet{Name: name, Instances: 1, Template: realm.UnitTemplate{Source: name, Command: []string{"/bin/app"}}}); e != nil {
			t.Fatal(e)
		}
	}
	n := &slowLeaseNode{healthNode: healthNode{ready: true}, entered: make(chan struct{}, 1), release: make(chan struct{}), renewals: map[string]int{}, stopped: map[string]bool{}}
	c := &Controller{Store: s, Nodes: n, leaseRefreshInterval: 5 * time.Millisecond}
	for i := 0; i < 2; i++ {
		if e = c.Once(); e != nil {
			t.Fatal(e)
		}
	}
	return s, c, n
}
func TestSlowStorageOrRuntimeKeepsExactHealthyLeases(t *testing.T) {
	s, c, n := leaseKeeperFixture(t)
	var fast string
	for id, a := range s.Snapshot().Assignments {
		if a.Fleet == "fast" {
			fast = id
		}
	}
	n.mu.Lock()
	n.block = true
	before := n.renewals[fast]
	n.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- c.Once() }()
	select {
	case <-n.entered:
	case <-time.After(time.Second):
		t.Fatal("slow action not reached")
	}
	deadline := time.After(time.Second)
	for {
		n.mu.Lock()
		count := n.renewals[fast]
		n.mu.Unlock()
		if count >= before+3 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("healthy lease was starved by slow action")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	a := s.Snapshot().Assignments[fast]
	if a.LeaseToken == "" || !a.LeaseExpiresAt.After(time.Now()) {
		t.Fatal("lease not retained in quorum state")
	}
	close(n.release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	n.mu.Lock()
	after := n.renewals[fast]
	n.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.renewals[fast] != after {
		t.Fatal("keeper outlived orchestration lock")
	}
}
func TestLeaseKeeperJoinsBeforeRetirement(t *testing.T) {
	s, c, n := leaseKeeperFixture(t)
	if e := s.DeleteFleet("fast"); e != nil {
		t.Fatal(e)
	}
	if e := c.Once(); e != nil {
		t.Fatal(e)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.afterStop {
		t.Fatal("assignment renewed after Stop/Revoke/Delete began")
	}
}
func TestLeaseKeeperSkipsQuarantinedOwner(t *testing.T) {
	s, c, _, _ := nativeRecoveryFixture(t)
	n := &slowLeaseNode{healthNode: healthNode{ready: true}, renewals: map[string]int{}, stopped: map[string]bool{}}
	c.Nodes = n
	c.leaseRefreshInterval = 5 * time.Millisecond
	var owner string
	for _, a := range s.Snapshot().Assignments {
		owner = a.NodeID
	}
	node := s.Snapshot().Nodes[owner]
	node.State = realm.NodeUnreachable
	if e := s.UpsertNode(node); e != nil {
		t.Fatal(e)
	}
	if _, e := s.BeginWorkerFailover(owner); e != nil {
		t.Fatal(e)
	}
	node.State = realm.NodeReady
	if e := s.UpsertNode(node); e != nil {
		t.Fatal(e)
	}
	if !s.Snapshot().Nodes[owner].PowerQuarantined {
		t.Fatal("fixture did not retain quarantine")
	}
	c.leaseKeepers = map[string]*assignmentLeaseKeeper{}
	n.mu.Lock()
	before := 0
	for _, v := range n.renewals {
		before += v
	}
	n.mu.Unlock()
	s.Orchestration.Lock()
	for id := range s.Snapshot().Assignments {
		c.keepAssignmentLease(id)
	}
	time.Sleep(25 * time.Millisecond)
	c.stopAssignmentLeases()
	s.Orchestration.Unlock()
	n.mu.Lock()
	defer n.mu.Unlock()
	after := 0
	for _, v := range n.renewals {
		after += v
	}
	if after != before {
		t.Fatal("quarantined incarnation received scheduling lease")
	}
}
