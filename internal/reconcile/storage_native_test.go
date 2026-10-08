//go:build linux

package reconcile

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/lease"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type nativeStorageNode struct {
	runtime *unitruntime.Manager
	disks   *disk.Manager
	leases  *lease.Manager
}
type nativeStorageNodes struct{ nodes map[string]*nativeStorageNode }

func (n *nativeStorageNodes) EnsureUnit(a string, s unitruntime.Spec) (unitruntime.State, error) {
	return n.nodes[a].runtime.Ensure(s)
}
func (n *nativeStorageNodes) StartUnit(a, id string) (unitruntime.State, error) {
	return n.nodes[a].runtime.EnsureRunning(id)
}
func (n *nativeStorageNodes) StopUnit(a, id string) (unitruntime.State, error) {
	return n.nodes[a].runtime.Stop(id, time.Second)
}
func (n *nativeStorageNodes) DeleteUnit(a, id string) error                      { return n.nodes[a].runtime.Delete(id) }
func (n *nativeStorageNodes) EnsureSource(string, string, *source.Manager) error { return nil }
func (n *nativeStorageNodes) RenewLease(a, id, token string, ttl time.Duration) (lease.Record, error) {
	return n.nodes[a].leases.Renew(id, token, ttl)
}
func (n *nativeStorageNodes) RevokeLease(a, id, token string) error {
	return n.nodes[a].leases.Revoke(id, token)
}
func (n *nativeStorageNodes) EnsureDisk(a string, c disk.Catalog) error {
	return n.nodes[a].disks.Adopt(c)
}
func (n *nativeStorageNodes) DiskWriter(a, name string) (disk.Writer, error) {
	return n.nodes[a].disks.Writer(name)
}
func (n *nativeStorageNodes) ReleaseDisk(a, name, unit, id string) error {
	return n.nodes[a].disks.Detach(name)
}

type interruptedNativeFence struct {
	manager   *disk.Manager
	completed bool
}

func (f *interruptedNativeFence) Fence(c disk.Catalog, w disk.Writer, check func() error) error {
	if e := f.manager.Fence(c, w, check); e != nil {
		return e
	}
	if !f.completed {
		f.completed = true
		return fmt.Errorf("injected controller loss after actual fence, before commit")
	}
	return nil
}

func TestNativeAutomaticStorageFailover(t *testing.T) {
	if os.Getenv("TITANUS_STORAGE_FAILOVER_TEST") != "1" {
		t.Skip("requires native Ceph, namespaces/cgroups and init")
	}
	cfg := disk.CephConfig{Cluster: "ceph", Pool: "titanus", FSName: "cephfs", Client: "client.admin", Conf: os.Getenv("TITANUS_CEPH_CONF")}
	controllerRoot := t.TempDir()
	for _, provider := range []disk.Provider{disk.ProviderCephRBD, disk.ProviderCephFS} {
		t.Run(string(provider), func(t *testing.T) {
			s, e := realm.Open(controllerRoot, "FAILOVER")
			if e != nil {
				t.Fatal(e)
			}
			// Keep mapping allocations across providers (the host mapping ledger is
			// shared), but isolate all scheduling state even after a fatal assertion.
			defer func() {
				cleanup, err := realm.Open(controllerRoot, "FAILOVER")
				if err != nil {
					t.Error(err)
					return
				}
				for name := range cleanup.Snapshot().Fleets {
					if err = cleanup.SetAssignments(name, nil); err != nil {
						t.Error(err)
					}
					if err = cleanup.DeleteFleet(name); err != nil {
						t.Error(err)
					}
				}
				for _, node := range cleanup.Snapshot().Nodes {
					node.State = realm.NodeDisabled
					if err = cleanup.UpsertNode(node); err != nil {
						t.Error(err)
					}
				}
			}()
			controllerDisks := disk.NewManager(controllerRoot)
			if e = controllerDisks.ConfigureCeph(cfg); e != nil {
				t.Fatal(e)
			}
			nodes := &nativeStorageNodes{nodes: map[string]*nativeStorageNode{}}
			for _, name := range []string{"a", "b"} {
				root := t.TempDir()
				d := disk.NewManager(root)
				if e = d.ConfigureCeph(cfg); e != nil {
					t.Fatal(e)
				}
				sources := source.NewManager(root)
				if e = sources.ImportDirectory("app", os.Getenv("TITANUS_FAILOVER_SOURCE")); e != nil {
					t.Fatal(e)
				}
				m := unitruntime.NewManager(unitruntime.Config{StateRoot: root, CgroupRoot: filepath.Join("/sys/fs/cgroup", "titanus-storage-"+string(provider)+"-"+name), InitBinary: os.Getenv("TITANUS_INIT_BINARY")})
				// Deliberately model a stalled node watchdog: neither a lost API nor a
				// scheduling deadline is allowed to substitute for a real Ceph fence.
				l := lease.NewManager(nil)
				nodes.nodes[name] = &nativeStorageNode{m, d, l}
				t.Cleanup(func() {
					states, _ := m.List()
					for _, state := range states {
						m.Stop(state.ID, time.Second)
						m.Delete(state.ID)
					}
					l.Close()
				})
				if e = s.UpsertNode(realm.Node{ID: name + "-" + string(provider), Address: name, State: realm.NodeReady, Capabilities: []model.Capability{model.CapabilityExecution}, Resources: realm.Resources{MemoryBytes: 4 << 30, CPUMilliCapacity: 4000}}); e != nil {
					t.Fatal(e)
				}
			}
			old := nodes.nodes["a"]
			name := "automatic-" + string(provider)
			if _, e = old.disks.Create(disk.Spec{Name: name, Provider: provider, SizeBytes: 128 << 20}); e != nil {
				t.Fatal(e)
			}
			// Initialize native data and provision the exact first committed cluster
			// mapping. The test never grants world-write permissions to storage.
			if e = old.disks.ProvisionOwnership(name, 1073741824+len(s.Snapshot().UnitMappings)*65536, 1073741824+len(s.Snapshot().UnitMappings)*65536); e != nil {
				t.Fatal(e)
			}
			if e = old.disks.Detach(name); e != nil {
				t.Fatal(e)
			}
			catalog, e := old.disks.Catalog(name)
			if e != nil {
				t.Fatal(e)
			}
			fleetName := "db-" + string(provider)
			catalog.Fleet = fleetName
			catalog.AutoFailover = true
			if e = controllerDisks.VerifyCatalog(catalog); e != nil {
				t.Fatal(e)
			}
			if e = s.PutDiskCatalog(catalog); e != nil {
				t.Fatal(e)
			}
			fleet := realm.Fleet{Name: fleetName, Instances: 1, Template: realm.UnitTemplate{Source: "app", Command: []string{"/bin/sh", "-ec", `if [ ! -e /data/proof ]; then echo PRESERVED > /data/proof; fi; echo STARTED; /bin/busybox sleep 180`}, MemoryBytes: 64 << 20, CPUPercent: 50, PidsMax: 64, Mounts: []disk.Mount{{Disk: name, Target: "/data"}}}}
			if e = s.PutFleet(fleet); e != nil {
				t.Fatal(e)
			}
			fence := &interruptedNativeFence{manager: controllerDisks}
			c := &Controller{Store: s, Nodes: nodes, Storage: fence}
			for i := 0; i < 2; i++ {
				if e = c.Once(); e != nil {
					t.Fatal(e)
				}
			}
			a := s.Snapshot().Assignments[fleetName+"-001-g1"]
			if a.State != realm.AssignmentActive || a.NodeID != "a-"+string(provider) || len(a.StorageWriters) != 1 {
				t.Fatalf("native writer not ready/captured: %+v", a)
			}
			_, oldState, e := old.runtime.Inspect(a.ID)
			if e != nil || oldState.PID == 0 {
				t.Fatal("old native workload missing", e)
			}
			path := filepath.Join(old.disks.StateRoot, "disks", name, "mount", "data")
			// Runtime readiness does not imply the application has written its
			// canary. Wait for it and explicitly acknowledge durable data before
			// simulating an unclean loss; fencing cannot preserve dirty pagecache.
			deadline := time.Now().Add(5 * time.Second)
			for {
				proof, err := os.ReadFile(filepath.Join(path, "proof"))
				if err == nil && string(proof) == "PRESERVED\n" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("original application did not write canary: %q %v", proof, err)
				}
				time.Sleep(20 * time.Millisecond)
			}
			proofFile, e := os.OpenFile(filepath.Join(path, "proof"), os.O_RDWR, 0)
			if e != nil {
				t.Fatal(e)
			}
			e = proofFile.Sync()
			proofFile.Close()
			if e != nil {
				t.Fatal(e)
			}
			stale, e := os.OpenFile(filepath.Join(path, "stale"), os.O_CREATE|os.O_RDWR|os.O_SYNC, 0600)
			if e != nil {
				t.Fatal(e)
			}
			defer stale.Close()
			if e = stale.Sync(); e != nil {
				t.Fatal(e)
			}
			directory, e := os.Open(path)
			if e != nil {
				t.Fatal(e)
			}
			e = directory.Sync()
			directory.Close()
			if e != nil {
				t.Fatal(e)
			}
			node := s.Snapshot().Nodes["a-"+string(provider)]
			node.State = realm.NodeUnreachable
			if e = s.UpsertNode(node); e != nil {
				t.Fatal(e)
			}
			if e = c.Once(); e != nil {
				t.Fatal(e)
			}
			for _, intent := range s.Snapshot().StorageFailovers {
				if intent.Assignment.ID == a.ID {
					t.Fatal("fenced before actual lease expiry")
				}
			}
			for time.Now().Before(a.LeaseExpiresAt.Add(2 * time.Second)) {
				time.Sleep(100 * time.Millisecond)
			}
			if e = c.Once(); e != nil {
				t.Fatal(e)
			}
			// Actual native fence succeeded, but controller result publication failed.
			// No successor can run until a reconstructed controller revalidates it.
			state := s.Snapshot()
			if !fence.completed || len(state.Assignments) != 1 || state.Assignments[a.ID].NodeID != "a-"+string(provider) {
				t.Fatal("uncommitted fence released assignment")
			}
			if _, e = stale.WriteAt([]byte("CORRUPT"), 0); e == nil {
				t.Fatal("actual stale native writer was not excluded")
			}
			if _, u, e := old.runtime.Inspect(a.ID); e != nil || u.PID != oldState.PID {
				t.Fatal("test accidentally relied on stopping old workload", e)
			}
			reopened, e := realm.Open(controllerRoot, state.Name)
			if e != nil {
				t.Fatal(e)
			}
			c.Store = reopened
			c.Storage = controllerDisks
			if e = c.Once(); e != nil {
				t.Fatal(e)
			}
			if len(reopened.Snapshot().Assignments) != 0 {
				t.Fatal("reconstructed controller did not finish committed handoff")
			}
			for i := 0; i < 2; i++ {
				if e = c.Once(); e != nil {
					t.Fatal(e)
				}
			}
			next := reopened.Snapshot().Assignments[a.ID]
			if next.NodeID != "b-"+string(provider) || next.State != realm.AssignmentActive {
				t.Fatalf("native successor did not start: %+v", next)
			}
			_, newState, e := nodes.nodes["b"].runtime.Inspect(a.ID)
			if e != nil || newState.UserMapping != oldState.UserMapping {
				t.Fatal("mapped file ownership changed on failover", e)
			}
			freshPath := filepath.Join(nodes.nodes["b"].disks.StateRoot, "disks", name, "mount", "data")
			proof, e := os.ReadFile(filepath.Join(freshPath, "proof"))
			if e != nil || string(proof) != "PRESERVED\n" {
				t.Fatal("successor lost native data", e, string(proof))
			}
			data, e := os.ReadFile(filepath.Join(freshPath, "stale"))
			if e != nil || len(data) != 0 {
				t.Fatal("stale writer corrupted successor data", e, string(data))
			}
			if e = reopened.Pulse("a-"+string(provider), realm.Resources{}); e != nil {
				t.Fatal(e)
			}
			if !reopened.Snapshot().Nodes["a-"+string(provider)].StorageQuarantined {
				t.Fatal("old node escaped quarantine")
			}
			// A genuine new native generation retains ownership of the same Disk set.
			fleet.Template.Command = []string{"/bin/sh", "-ec", `test "$(/bin/busybox cat /data/proof)" = PRESERVED; echo ROLLED; /bin/busybox sleep 180`}
			if e = reopened.PutFleet(fleet); e != nil {
				t.Fatal(e)
			}
			for i := 0; i < 4; i++ {
				if e = c.Once(); e != nil {
					t.Fatal(e)
				}
			}
			current := reopened.Snapshot().Assignments[fleetName+"-001-g2"]
			if current.NodeID != "b-"+string(provider) || current.State != realm.AssignmentActive {
				t.Fatalf("managed Disk rollout failed: %+v", current)
			}
			_, rolled, e := nodes.nodes["b"].runtime.Inspect(current.ID)
			if e != nil || rolled.UserMapping != oldState.UserMapping {
				t.Fatal("rollout changed persisted Disk ownership", e)
			}
			t.Log("TITANUS_NATIVE_AUTOMATIC_STORAGE_FAILOVER_OK", provider)
			// Close the open blocked client before fixture TempDir cleanup.
			stale.Close()
			old.runtime.Stop(a.ID, time.Second)
			if e = old.disks.Detach(name); e != nil {
				t.Fatal("quarantined native detach", e)
			}
			nodes.nodes["b"].runtime.Stop(current.ID, time.Second)
			if e = nodes.nodes["b"].disks.Detach(name); e != nil {
				t.Fatal(e)
			}
			if e = reopened.SetAssignments(fleetName, nil); e != nil {
				t.Fatal(e)
			}
			if e = reopened.DeleteFleet(fleetName); e != nil {
				t.Fatal(e)
			}
			for _, n := range reopened.Snapshot().Nodes {
				n.State = realm.NodeDisabled
				if e = reopened.UpsertNode(n); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}

func (n *nativeStorageNodes) DiskCatalog(a, name string) (disk.Catalog, error) {
	return n.nodes[a].disks.Catalog(name)
}
