package disk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNativeCeph(t *testing.T) {
	if os.Getenv("TITANUS_CEPH_TEST") != "1" {
		t.Skip("native Ceph cluster required")
	}
	cfg := CephConfig{Cluster: "ceph", Pool: "titanus", FSName: "cephfs", Client: "client.admin", Conf: os.Getenv("TITANUS_CEPH_CONF")}
	for _, provider := range []Provider{ProviderCephRBD, ProviderCephFS} {
		t.Run(string(provider), func(t *testing.T) {
			m := NewManager(t.TempDir())
			if err := m.ConfigureCeph(cfg); err != nil {
				t.Fatal(err)
			}
			name := "disk-" + string(provider)
			spec, err := m.Create(Spec{Name: name, Provider: provider, SizeBytes: 128 << 20})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if e := m.detach(name); e != nil {
					t.Error(e)
				}
			})
			a, err := m.Acquire(name, "node-a", "run-a")
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			if err = os.WriteFile(filepath.Join(a.Path, "proof"), []byte("snapshot-data"), 0600); err != nil {
				t.Fatal(err)
			}
			f, err := os.OpenFile(filepath.Join(a.Path, "proof"), os.O_RDWR|os.O_SYNC, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err = f.Sync(); err != nil {
				t.Fatal(err)
			}
			second := NewManager(t.TempDir())
			if err = second.ConfigureCeph(cfg); err != nil {
				t.Fatal(err)
			}
			// After attachment/file defers close, remove the second client's native
			// mount before TempDir cleanup; its evicted CephFS lock cannot reopen.
			t.Cleanup(func() {
				if e := second.detach(name); e != nil {
					t.Error(e)
				}
			})
			// Pre-stage the SAME remote identity on an independently persisted node.
			spec, err = m.Inspect(name)
			if err != nil {
				t.Fatal(err)
			}
			if err = writeJSON(second.specPath(name), spec, 0600); err != nil {
				t.Fatal(err)
			}
			if competitor, e := second.Acquire(name, "node-b", "run-b"); e == nil {
				competitor.Close()
				t.Fatal("remote live writer takeover accepted")
			}
			if _, err = m.Snapshot(name, "live"); err == nil {
				t.Fatal("snapshot with active writer accepted")
			}
			f.Close()
			a.Close()
			if _, err = m.Snapshot(name, "backup"); err != nil {
				t.Fatal(err)
			}
			if _, err = m.Restore(name, "backup", name+"-restored"); err != nil {
				t.Fatal(err)
			}
			restored, err := m.Acquire(name+"-restored", "restore-check", "run")
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			proof, err := os.ReadFile(filepath.Join(restored.Path, "proof"))
			if err != nil || string(proof) != "snapshot-data" {
				t.Fatal("native snapshot restore lost data", err, string(proof))
			}
			restored.Close()
			if err = m.Detach(name + "-restored"); err != nil {
				t.Fatal(err)
			}
			list, err := m.Snapshots(name)
			if err != nil || len(list) != 1 || list[0].Name != "backup" {
				t.Fatal("native snapshots list", list, err)
			}
			if err = m.Detach(name); err != nil {
				t.Fatal(err)
			}
			if provider == ProviderCephRBD {
				// Exercise the snapshot gap with NO kernel mapping: the distributed
				// lifecycle guard must still exclude an independently persisted node.
				release, e := m.radosGuard(name)
				if e != nil {
					t.Fatal(e)
				}
				competitor, e := second.Acquire(name, "node-b", "maintenance-gap")
				release()
				if e == nil {
					competitor.Close()
					t.Fatal("writer entered unmapped maintenance window")
				}
			}
			if provider == ProviderCephFS {
				// Effective MDS settings can differ from monitor defaults.
				args := append(m.cephBaseArgs(cfg), "tell", "mds.cephfs:0", "config", "set", "mds_session_blocklist_on_evict")
				if _, e := commandOutput("ceph", append(args, "false")...); e != nil {
					t.Fatal(e)
				}
				competitor, e := second.Acquire(name, "node-b", "unsafe-policy")
				_, resetErr := commandOutput("ceph", append(args, "true")...)
				if resetErr != nil {
					t.Fatal(resetErr)
				}
				if e == nil {
					competitor.Close()
					t.Fatal("unsafe active MDS override accepted")
				}
			}
			successor, err := second.Acquire(name, "node-b", "run-b")
			if err != nil {
				t.Fatal("acknowledged failover failed", err)
			}
			defer successor.Close()
			if err = os.WriteFile(filepath.Join(successor.Path, "successor"), []byte("node-b"), 0600); err != nil {
				t.Fatal(err)
			}
			if provider == ProviderCephFS {
				// Retain a real open writer on node B. Native MDS eviction must invalidate
				// it before node A can obtain the same distributed ownership lock.
				stale, err := os.OpenFile(filepath.Join(successor.Path, "stale"), os.O_CREATE|os.O_RDWR|os.O_SYNC, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer stale.Close()
				out, err := commandOutput("ceph", append(m.cephBaseArgs(cfg), "tell", "mds.cephfs:0", "client", "ls", "--format", "json")...)
				if err != nil {
					t.Fatal(err)
				}
				var clients []cephSession
				if err = json.Unmarshal([]byte(out), &clients); err != nil {
					t.Fatal(err)
				}
				var session uint64
				candidates := 0
				address := ""
				for _, client := range clients {
					if client.Metadata.Root == spec.RemotePath {
						candidates++
						session = client.ID
						address = strings.TrimPrefix(client.Inst, "client."+strconv.FormatUint(client.ID, 10)+" ")
					}
				}
				if session == 0 || candidates != 1 {
					t.Fatalf("cannot identify exact kernel session: %s", out)
				}
				if err = m.FenceCephFS(name, session, address+"-wrong"); err == nil {
					t.Fatal("mismatched session fencing accepted")
				}
				if err = m.FenceCephFS(name, session, address); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(15 * time.Second)
				var fresh *Attachment
				for time.Now().Before(deadline) {
					fresh, err = m.Acquire(name, "node-a", "after-fence")
					if err == nil {
						break
					}
					time.Sleep(200 * time.Millisecond)
				}
				if err != nil {
					t.Fatal("fenced successor never acquired Disk", err)
				}
				defer fresh.Close()
				if _, err = stale.WriteAt([]byte("corrupt"), 0); err == nil {
					t.Fatal("fenced writer still wrote to real CephFS")
				}
				if data, e := os.ReadFile(filepath.Join(fresh.Path, "stale")); e != nil || len(data) != 0 {
					t.Fatal("stale writer changed successor data", e, string(data))
				}
				if err = os.WriteFile(filepath.Join(fresh.Path, "post-fence"), []byte("safe"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			successor.Close()
		})
	}
}
