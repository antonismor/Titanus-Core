package reconcile

import (
	"errors"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"testing"
	"time"
)

type failingFence struct {
	fail  bool
	calls int
}

func (f *failingFence) Fence(_ disk.Catalog, _ disk.Writer, check func() error) error {
	f.calls++
	if e := check(); e != nil {
		return e
	}
	if f.fail {
		return errors.New("OSD did not acknowledge fence")
	}
	return nil
}
func TestUnconfirmedStorageFenceCannotReleaseAssignment(t *testing.T) {
	root := t.TempDir()
	s, e := realm.Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	catalog := disk.Catalog{Spec: disk.Spec{Name: "data", Provider: disk.ProviderCephFS, SizeBytes: 1 << 20, Initialized: true, LayoutVersion: 1, RemotePath: "/volumes/_nogroup/data/uuid"}, Backend: disk.Backend{FSID: "fsid", Filesystem: "cephfs", Object: "/volumes/_nogroup/data/uuid"}, Fleet: "db", AutoFailover: true}
	if e = s.PutDiskCatalog(catalog); e != nil {
		t.Fatal(e)
	}
	if e = s.PutFleet(realm.Fleet{Name: "db", Instances: 1, Template: realm.UnitTemplate{Source: "app", Command: []string{"app"}, Mounts: []disk.Mount{{Disk: "data", Target: "/data"}}}}); e != nil {
		t.Fatal(e)
	}
	a := realm.Assignment{ID: "db-001-g1", Fleet: "db", NodeID: "old", Generation: 1, LeaseToken: "token", LeaseExpiresAt: time.Now().Add(-5 * time.Second), CreatedAt: time.Now(), StorageWriters: map[string]disk.Writer{"data": {Disk: "data", CatalogID: catalog.ID(), Session: 9, Address: "v1:192.0.2.1:0/88"}}}
	if e = s.SetAssignments("db", []realm.Assignment{a}); e != nil {
		t.Fatal(e)
	}
	if e = s.UpsertNode(realm.Node{ID: "old", State: realm.NodeUnreachable, Capabilities: []model.Capability{model.CapabilityExecution}}); e != nil {
		t.Fatal(e)
	}
	n := &healthNode{ready: true}
	f := &failingFence{fail: true}
	c := &Controller{Store: s, Nodes: n, Storage: f}
	if e = c.Once(); e != nil {
		t.Fatal(e)
	}
	state := s.Snapshot()
	if len(state.Assignments) != 1 || state.Assignments[a.ID].NodeID != "old" || f.calls != 1 {
		t.Fatal("unconfirmed fence released writable assignment", state.Assignments)
	}
	for _, intent := range state.StorageFailovers {
		if intent.Completed {
			t.Fatal("failed fence reported complete")
		}
	}
	reopened, e := realm.Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	c.Store = reopened
	f.fail = false
	if e = c.Once(); e != nil {
		t.Fatal(e)
	}
	if len(reopened.Snapshot().Assignments) != 0 || f.calls != 2 {
		t.Fatal("durable failover did not resume")
	}
	for _, intent := range reopened.Snapshot().StorageFailovers {
		if !intent.Completed {
			t.Fatal("successful fence not committed")
		}
	}
}
func TestStoragePhysicalActionsRequireQuorum(t *testing.T) {
	denied := errors.New("lost quorum")
	g := &GuardedNodes{Check: func() error { return denied }}
	if e := g.EnsureDisk("node", disk.Catalog{}); !errors.Is(e, denied) {
		t.Fatal(e)
	}
	if _, e := g.DiskWriter("node", "disk"); !errors.Is(e, denied) {
		t.Fatal(e)
	}
	if e := g.ReleaseDisk("node", "disk", "unit", "catalog"); !errors.Is(e, denied) {
		t.Fatal(e)
	}
}
