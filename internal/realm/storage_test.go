package realm

import (
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/model"
	"testing"
	"time"
)

func storageFixture(t *testing.T) (*Store, Assignment, disk.Catalog, string) {
	t.Helper()
	root := t.TempDir()
	s, e := Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	c := disk.Catalog{Spec: disk.Spec{Name: "data", Provider: disk.ProviderCephFS, SizeBytes: 1 << 20, Initialized: true, LayoutVersion: 1, RemotePath: "/volumes/_nogroup/data/uuid"}, Backend: disk.Backend{FSID: "fsid", Filesystem: "cephfs", Object: "/volumes/_nogroup/data/uuid"}, Fleet: "db", AutoFailover: true}
	if e = s.PutDiskCatalog(c); e != nil {
		t.Fatal(e)
	}
	if e = s.PutFleet(Fleet{Name: "db", Instances: 1, Template: UnitTemplate{Source: "app", Command: []string{"app"}, Mounts: []disk.Mount{{Disk: "data", Target: "/data"}}}}); e != nil {
		t.Fatal(e)
	}
	a := Assignment{ID: "db-001-g1", Fleet: "db", NodeID: "old", Generation: 1, LeaseToken: "exact-token", LeaseExpiresAt: time.Now().Add(-5 * time.Second), CreatedAt: time.Now(), StorageWriters: map[string]disk.Writer{"data": {Disk: "data", CatalogID: c.ID(), Session: 9, Address: "v1:192.0.2.1:0/88"}}}
	if e = s.SetAssignments("db", []Assignment{a}); e != nil {
		t.Fatal(e)
	}
	if e = s.UpsertNode(Node{ID: "old", State: NodeUnreachable, Capabilities: []model.Capability{model.CapabilityExecution}}); e != nil {
		t.Fatal(e)
	}
	return s, a, c, root
}

func TestFailoverIntentSurvivesRestartAndQuarantinesPulse(t *testing.T) {
	s, a, _, root := storageFixture(t)
	m, e := s.UnitMapping(a.ID)
	if e != nil {
		t.Fatal(e)
	}
	intent, e := s.BeginStorageFailover(a)
	if e != nil {
		t.Fatal(e)
	}
	if intent.Completed || s.Snapshot().Assignments[a.ID].State == AssignmentStopped {
		t.Fatal("intent was mistaken for a fence")
	}
	s, e = Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	restored, e := s.BeginStorageFailover(a)
	if e != nil || restored.ID != intent.ID {
		t.Fatal("intent identity lost", e)
	}
	if e = s.Pulse("old", Resources{}); e != nil {
		t.Fatal(e)
	}
	if e = s.UpsertNode(Node{ID: "old", State: NodeReady, Capabilities: []model.Capability{model.CapabilityExecution}}); e != nil {
		t.Fatal(e)
	}
	if eligible(s.Snapshot().Nodes["old"], s.Snapshot().Fleets["db"]) {
		t.Fatal("Pulse/re-registration bypassed quarantine")
	}
	if e = s.CompleteStorageFailover(restored); e != nil {
		t.Fatal(e)
	}
	if s.Snapshot().Assignments[a.ID].State != AssignmentStopped || !s.Snapshot().StorageFailovers[intent.ID].Completed {
		t.Fatal("fence completion not committed")
	}
	if newMapping, e := s.UnitMapping(a.ID); e != nil || newMapping != m {
		t.Fatal("mapping identity lost", e)
	}
}

func TestFailoverRejectsUnknownWriterAndLiveLease(t *testing.T) {
	for _, mode := range []string{"live", "unknown", "wrong-object", "local"} {
		t.Run(mode, func(t *testing.T) {
			s, a, c, _ := storageFixture(t)
			switch mode {
			case "live":
				a.LeaseExpiresAt = time.Now().Add(time.Minute)
			case "unknown":
				a.StorageWriters = nil
			case "wrong-object":
				w := a.StorageWriters["data"]
				w.CatalogID = "wrong"
				a.StorageWriters["data"] = w
			case "local":
				c.Spec.Provider = disk.ProviderLocal
				c.AutoFailover = false
				c.LocalNode = "old"
				c.Backend = disk.Backend{}
				s.data.Disks["data"] = c
			}
			if e := s.SetAssignments("db", []Assignment{a}); e != nil {
				t.Fatal(e)
			}
			if _, e := s.BeginStorageFailover(a); e == nil {
				t.Fatal("unsafe automatic handoff accepted")
			}
			if len(s.Snapshot().StorageFailovers) != 0 {
				t.Fatal("invalid intent persisted")
			}
		})
	}
}

func TestCatalogOwnershipAndLocalPlacement(t *testing.T) {
	s, _, c, _ := storageFixture(t)
	if e := s.PutDiskCatalog(c); e == nil {
		t.Fatal("catalog changed during execution")
	}
	f := s.Snapshot().Fleets["db"]
	f.Instances = 2
	if e := s.PutFleet(f); e == nil {
		t.Fatal("exclusive Disk assigned to multiple instances")
	}
	if _, e := s.ScaleFleet("db", 2); e == nil {
		t.Fatal("scale bypassed Disk ownership")
	}
	f.Name = "other"
	f.Instances = 1
	if e := s.PutFleet(f); e == nil {
		t.Fatal("another Fleet stole catalog")
	}
	c.Spec.Provider = disk.ProviderLocal
	c.AutoFailover = false
	c.LocalNode = "old"
	c.Backend = disk.Backend{}
	state := s.Snapshot()
	state.Disks["data"] = c
	if catalogPlacement(state, Node{ID: "new"}, state.Fleets["db"]) {
		t.Fatal("local data relocated using only metadata")
	}
}

func TestFailedDurableIntentCannotAuthorizeFence(t *testing.T) {
	s, a, _, _ := storageFixture(t)
	original := s.path
	s.path = t.TempDir() // atomic publication onto a directory fails
	if _, e := s.BeginStorageFailover(a); e == nil {
		t.Fatal("storage error did not reject intent")
	}
	if len(s.Snapshot().StorageFailovers) != 0 || s.Snapshot().Nodes[a.NodeID].StorageQuarantined {
		t.Fatal("uncommitted candidate escaped into scheduling state")
	}
	if _, e := s.UnitMapping(a.ID); e == nil {
		t.Fatal("mapping publication failure accepted")
	}
	if len(s.Snapshot().UnitMappings) != 0 {
		t.Fatal("uncommitted mapping escaped into execution")
	}
	s.path = original
	if _, e := s.BeginStorageFailover(a); e != nil {
		t.Fatal(e)
	}
}
