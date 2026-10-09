package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/consensus"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/version"
)

// This is a disposable native-runner recovery, sharing one kernel. It destroys
// all three original voter states, PKI, keyrings, Sources and local Disk bytes,
// then reconstructs solely from independently authenticated per-host archives.
func TestNativeOfflineQuorumRecovery(t *testing.T) {
	if os.Getenv("TITANUS_BACKUP_NATIVE_TEST") != "1" {
		t.Skip("opt-in disposable native recovery")
	}
	if os.Geteuid() != 0 {
		t.Fatal("native recovery requires root")
	}
	runNativeOfflineQuorumRecovery(t, false, false)
}

func TestNativeClusterRecovery(t *testing.T) {
	if os.Getenv("TITANUS_BACKUP_NATIVE_TEST") != "1" {
		t.Skip("opt-in native cluster recovery")
	}
	runNativeOfflineQuorumRecovery(t, true, false)
}

func TestNativeMigratedQuorumRecovery(t *testing.T) {
	if os.Getenv("TITANUS_BACKUP_NATIVE_TEST") != "1" {
		t.Skip("opt-in migrated native quorum recovery")
	}
	runNativeOfflineQuorumRecovery(t, false, true)
}

func runNativeOfflineQuorumRecovery(t *testing.T, clusterMode, migrated bool) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "external.key")
	if e := Keygen(keyPath); e != nil {
		t.Fatal(e)
	}
	authority, e := identity.InitAuthority(filepath.Join(dir, "original-authority"), "NATIVE-DR")
	if e != nil {
		t.Fatal(e)
	}
	var fixtures [3]fixture
	var configs [3]consensus.Config
	var nodes [3]*consensus.Node
	var stores [3]*realm.Store
	var raftReservations, apiReservations [3]net.Listener
	peers := []consensus.Peer{}
	for i := 0; i < 3; i++ {
		raftReservations[i], e = net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		apiReservations[i], e = net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		peers = append(peers, consensus.Peer{ID: fmt.Sprintf("controller%d", i+1), Address: raftReservations[i].Addr().String(), API: "https://" + apiReservations[i].Addr().String()})
	}
	t.Cleanup(func() {
		for i := range nodes {
			if nodes[i] != nil {
				nodes[i].Close()
			}
			raftReservations[i].Close()
			apiReservations[i].Close()
		}
	})
	for i := range fixtures {
		host := filepath.Join(dir, peers[i].ID)
		if e = os.Mkdir(host, 0700); e != nil {
			t.Fatal(e)
		}
		plan := Plan{"titanus-backup-plan/v1", "NATIVE-DR", peers[i].ID, "controller", filepath.Join(host, "state"), filepath.Join(host, "configuration"), filepath.Join(host, "ledger")}
		for _, root := range plan.roots() {
			if e = os.Mkdir(root, 0700); e != nil {
				t.Fatal(e)
			}
		}
		pki := filepath.Join(plan.ConfigRoot, "pki")
		os.Mkdir(pki, 0700)
		cert, key, e := authority.Issue(plan.Node, []string{"127.0.0.1"}, identity.RoleController, time.Hour)
		if e != nil {
			t.Fatal(e)
		}
		for name, file := range map[string]string{"ca.crt": authority.CertPath, "ca.key": authority.KeyPath, "ca.crl": filepath.Join(authority.Dir, "ca.crl"), "node.crt": cert, "node.key": key} {
			data, e := os.ReadFile(file)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(pki, name), data, 0600); e != nil {
				t.Fatal(e)
			}
		}
		configs[i] = consensus.Config{ID: plan.Node, Realm: plan.Realm, Peers: peers, Bootstrap: i == 0}
		if e = durable.WriteJSON(filepath.Join(plan.ConfigRoot, "ha.json"), configs[i], 0600); e != nil {
			t.Fatal(e)
		}
		store, e := realm.Open(plan.StateRoot, plan.Realm)
		if e != nil {
			t.Fatal(e)
		}
		if e = store.ConfigureNetwork(realm.RealmNetwork{FabricCIDR: "10.210.0.0/16", ServiceCIDR: "10.220.0.0/16", NodePrefix: 24, VXLANID: 4242}); e != nil {
			t.Fatal(e)
		}
		stores[i] = store
		keyring := secrets.Keyring{Active: "one", Keys: map[string][]byte{"one": bytes.Repeat([]byte{7}, 32)}}
		if e = durable.WriteJSON(filepath.Join(plan.ConfigRoot, "secrets.json"), keyring, 0600); e != nil {
			t.Fatal(e)
		}
		input := filepath.Join(host, "original-source")
		os.Mkdir(input, 0755)
		os.WriteFile(filepath.Join(input, "proof"), []byte("NATIVE_SOURCE_RECOVERED"), 0644)
		if e = source.NewManager(plan.StateRoot).ImportDirectory("app", input); e != nil {
			t.Fatal(e)
		}
		if _, e = disk.NewManager(plan.StateRoot).Create(disk.Spec{Name: "data", Provider: disk.ProviderLocal, SizeBytes: 64 << 20}); e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(plan.StateRoot, "disks/data/data/proof"), []byte("NATIVE_LOCAL_DATA_"+plan.Node), 0600); e != nil {
			t.Fatal(e)
		}
		fixtures[i] = fixture{plan, host, keyPath, filepath.Join(host, "archive.tar.gz"), filepath.Join(host, "fence.json"), store}
	}
	open := func(i int) {
		t.Helper()
		p := fixtures[i].plan
		pki := filepath.Join(p.ConfigRoot, "pki")
		cfg, e := consensus.LoadConfig(filepath.Join(p.ConfigRoot, "ha.json"))
		if e != nil {
			t.Fatal(e)
		}
		stores[i], e = realm.Open(p.StateRoot, p.Realm)
		if e != nil {
			t.Fatal(e)
		}
		nodes[i], e = consensus.Open(p.StateRoot, cfg, stores[i].Snapshot(), filepath.Join(pki, "ca.crt"), filepath.Join(pki, "node.crt"), filepath.Join(pki, "node.key"))
		if e != nil {
			t.Fatal(e)
		}
		if e = stores[i].EnableConsensus(nodes[i]); e != nil {
			t.Fatal(e)
		}
	}
	leader := func() int {
		t.Helper()
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			for i, n := range nodes {
				if n != nil && n.Initialize() == nil && n.CheckLeader() == nil {
					return i
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("no functional quorum after recovery")
		return -1
	}
	converge := func(revision uint64) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			ok := true
			for _, n := range nodes {
				if n == nil || n.Snapshot().Revision != revision {
					ok = false
				}
			}
			if ok {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatal("all three voter identities did not converge")
	}
	actorLock, e := offline.Shared()
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { actorLock.Close() })
	for i := range fixtures {
		raftReservations[i].Close()
		open(i)
	}
	l := leader()

	remoteCatalogs := map[string]disk.Catalog{}
	if clusterMode && os.Getenv("TITANUS_CEPH_TEST") == "1" {
		dm := disk.NewManager(fixtures[0].plan.StateRoot)
		confData, e := os.ReadFile(os.Getenv("TITANUS_CEPH_CONF"))
		if e != nil {
			t.Fatal(e)
		}
		confPath := filepath.Join(fixtures[0].plan.ConfigRoot, "ceph.conf")
		if e = os.WriteFile(confPath, confData, 0600); e != nil {
			t.Fatal(e)
		}
		if e = dm.ConfigureCeph(disk.CephConfig{Cluster: "ceph", Pool: "titanus", FSName: "cephfs", Client: "client.admin", Conf: confPath}); e != nil {
			t.Fatal(e)
		}
		for _, provider := range []disk.Provider{disk.ProviderCephRBD, disk.ProviderCephFS} {
			name := "dr-" + string(provider)
			if _, e = dm.Create(disk.Spec{Name: name, Provider: provider, SizeBytes: 64 << 20}); e != nil {
				t.Fatal(e)
			}
			a, e := dm.Acquire(name, "recovery-fixture", "before-backup")
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(filepath.Join(a.Path, "proof"), []byte("CEPH_DR_BYTES_"+name), 0640); e != nil {
				t.Fatal(e)
			}
			a.Close()
			if e = dm.Detach(name); e != nil {
				t.Fatal(e)
			}
			c, e := dm.Catalog(name)
			if e != nil {
				t.Fatal(e)
			}
			c.Fleet = "recovery-app"
			remoteCatalogs[name] = c
			if e = stores[l].PutDiskCatalog(c); e != nil {
				t.Fatal(e)
			}
		}
	}
	policy, e := authority.InitialPolicy()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = stores[l].MutatePKI(func(identity.Policy) (identity.Policy, error) { return policy, nil }); e != nil {
		t.Fatal(e)
	}
	digest, e := source.NewManager(fixtures[l].plan.StateRoot).Identity("app")
	if e != nil {
		t.Fatal(e)
	}
	if e = stores[l].PutSourceRecord(realm.SourceRecord{Name: "app", Digest: digest}); e != nil {
		t.Fatal(e)
	}
	keyring, e := secrets.Load(filepath.Join(fixtures[l].plan.ConfigRoot, "secrets.json"))
	if e != nil {
		t.Fatal(e)
	}
	r, e := keyring.Encrypt("NATIVE-DR", "recovery", 1, []byte("NATIVE_QUORUM_SECRET_RECOVERED"))
	if e != nil {
		t.Fatal(e)
	}
	if e = stores[l].PutSecret(r); e != nil {
		t.Fatal(e)
	}
	if migrated {
		id := "native-backup-migration-001"
		if e = stores[l].CreateTask(realm.Task{Name: "unknown-proof", Template: realm.UnitTemplate{Source: "app", Command: []string{"/bin/sh"}}}); e != nil {
			t.Fatal(e)
		}
		task := stores[l].Snapshot().Tasks["unknown-proof"]
		task.Phase = realm.TaskUnknown
		task.RunID = "uncertain-do-not-replay"
		if e = stores[l].UpdateTask(task); e != nil {
			t.Fatal(e)
		}
		observed := map[string]version.Capabilities{}
		required := []string{}
		for _, f := range fixtures {
			if e = offline.PrepareSchemaFloor(f.plan.StateRoot, offline.NewSchemaFloor("NATIVE-DR", id, 1)); e != nil {
				t.Fatal(e)
			}
			observed[f.plan.Node] = version.Compatible()
			required = append(required, f.plan.Node)
		}
		if _, e = stores[l].TransitionSchema(id, stores[l].Snapshot().Revision, observed, required); e != nil {
			t.Fatal(e)
		}

		if version.MaxSchema >= 2 {
			nextID := "native-backup-schema-two-001"
			for _, f := range fixtures {
				if e = offline.PrepareSchemaFloor(f.plan.StateRoot, offline.NewSchemaFloor("NATIVE-DR", nextID, 2)); e != nil {
					t.Fatal(e)
				}
				keys, e := secrets.Load(filepath.Join(f.plan.ConfigRoot, "secrets.json"))
				if e != nil {
					t.Fatal(e)
				}
				keys.Keys["two"] = bytes.Repeat([]byte{7}, 32)
				keys.Active = "two"
				if e = durable.WriteJSON(filepath.Join(f.plan.ConfigRoot, "secrets.json"), keys, 0600); e != nil {
					t.Fatal(e)
				}
			}
			if _, e = stores[l].TransitionSchemaTo(nextID, stores[l].Snapshot().Revision, 2, observed, required); e != nil {
				t.Fatal(e)
			}
			j := realm.TaskSchedule{Name: "restored-future", Template: realm.UnitTemplate{Source: "app", Command: []string{"/bin/sh"}}, DueAt: time.Now().Add(time.Hour), MaxRuns: 1, MaxAttempts: 2, RetrySeconds: 1}
			if e = stores[l].CreateTaskSchedule(j); e != nil {
				t.Fatal(e)
			}
			keys, e := secrets.Load(filepath.Join(fixtures[l].plan.ConfigRoot, "secrets.json"))
			if e != nil {
				t.Fatal(e)
			}
			if _, e = stores[l].RotateSecrets("native-backup-rotation-001", "two", stores[l].Snapshot().Revision, keys); e != nil {
				t.Fatal(e)
			}
		}
	}
	before := stores[l].Snapshot()
	converge(before.Revision)
	if _, e = Create(fixtures[0].plan, fixtures[0].archive, keyPath); e == nil {
		t.Fatal("host backup ignored active supported actors")
	}
	for i := range nodes {
		if e = nodes[i].Close(); e != nil {
			t.Fatal(e)
		}
		nodes[i] = nil
	}
	actorLock.Close()

	if !clusterMode {
		for i, f := range fixtures {
			m, e := Create(f.plan, f.archive, keyPath)
			if e != nil {
				t.Fatal(e)
			}
			if m.RealmRevision != before.Revision {
				t.Fatal("stale seed was backed up instead of committed Raft state")
			}
			f.attest(t)
			f.loseRoots(t)
			if migrated {
				if e = os.Remove(f.plan.StateRoot + ".recovery-pending"); e != nil {
					t.Fatal(e)
				}
			}
			os.RemoveAll(filepath.Join(f.dir, "original-source"))
			if _, e = Restore(f.archive, keyPath, f.fence, f.plan); e != nil {
				t.Fatalf("restore voter %d: %v", i, e)
			}
		}

	} else {
		plans := []Plan{}
		for _, f := range fixtures {
			plans = append(plans, f.plan)
		}
		hostsPath := filepath.Join(dir, "hosts.json")
		durable.WriteJSON(hostsPath, plans, 0600)
		intentPath := filepath.Join(dir, "intent.json")
		if _, e = CreateClusterIntent(fixtures[0].plan, hostsPath, keyPath, intentPath); e != nil {
			t.Fatal(e)
		}
		storagePath := filepath.Join(dir, "ceph-data")
		if _, e = ExportStorage(fixtures[0].plan.StateRoot, intentPath, keyPath, storagePath); e != nil {
			t.Fatal(e)
		}
		setPlan := ClusterSetPlan{Intent: intentPath, Storage: storagePath}
		for _, f := range fixtures {
			if _, e = CreateClusterHost(f.plan, f.archive, keyPath, intentPath, storagePath); e != nil {
				t.Fatal(e)
			}
			f.attest(t)
			setPlan.Hosts = append(setPlan.Hosts, ClusterHost{Plan: f.plan, Archive: f.archive})
		}
		setPlanPath := filepath.Join(dir, "set-plan.json")
		durable.WriteJSON(setPlanPath, setPlan, 0600)
		setPath := filepath.Join(dir, "recovery-set.json")
		if _, e = SealClusterSet(setPlanPath, keyPath, setPath); e != nil {
			t.Fatal(e)
		}
		// Reject incomplete and mixed archives before any restored root exists.
		missing := setPlan
		missing.Hosts = missing.Hosts[:2]
		badPath := filepath.Join(dir, "missing.json")
		durable.WriteJSON(badPath, missing, 0600)
		if _, e = SealClusterSet(badPath, keyPath, filepath.Join(dir, "must-not-exist")); e == nil {
			t.Fatal("incomplete voter backup accepted")
		}
		for _, f := range fixtures {
			f.loseRoots(t)
			os.RemoveAll(filepath.Join(f.dir, "original-source"))
			if _, e = Restore(f.archive, keyPath, f.fence, f.plan); e == nil {
				t.Fatal("cluster backup restored without set gate")
			}
		}
		proofs := []string{}
		for _, f := range fixtures {
			if _, e = RestoreClusterHost(setPath, keyPath, f.plan.Node, f.fence); e != nil {
				t.Fatal(e)
			}
			if e = offline.CheckStartup(f.plan.StateRoot); e == nil {
				t.Fatal("partial cluster restore permits startup")
			}
			proof := filepath.Join(f.dir, "host-proof.json")
			if e = ProveClusterHost(setPath, keyPath, f.plan.Node, proof); e != nil {
				t.Fatal(e)
			}
			proofs = append(proofs, proof)
		}
		dm := disk.NewManager(fixtures[0].plan.StateRoot)
		// Destroy backend DATA while preserving exact native image/subvolume IDs.
		// A fresh FSID or replacement object is intentionally a different profile.
		for name, c := range remoteCatalogs {
			if e = dm.WithOfflineData(c, func(path string) error { return os.Remove(filepath.Join(path, "proof")) }); e != nil {
				t.Fatal(e)
			}
			_ = name
		}
		recordPath := filepath.Join(dir, "storage-exclusion.json")
		durable.WriteJSON(recordPath, FenceRecord{"NATIVE-DR", "storage", true, true, true, "All disposable native actors stopped; original Ceph IDs retained and old external writers excluded."}, 0600)
		storageFence := filepath.Join(dir, "storage-fence.json")
		if e = AttestStorageFence(setPath, keyPath, recordPath, storageFence); e != nil {
			t.Fatal(e)
		}
		storageProof := filepath.Join(dir, "storage-proof.json")
		if e = ImportStorage(fixtures[0].plan.StateRoot, setPath, keyPath, storageFence, storageProof); e != nil {
			t.Fatal(e)
		}
		completionPath := filepath.Join(dir, "completion.json")
		proofPaths := filepath.Join(dir, "proof-paths.json")
		durable.WriteJSON(proofPaths, proofs, 0600)
		if e = CompleteClusterRecovery(setPath, keyPath, proofPaths, completionPath); e == nil {
			t.Fatal("missing storage completion accepted")
		}
		proofs = append(proofs, storageProof)
		durable.WriteJSON(proofPaths, proofs, 0600)
		if e = CompleteClusterRecovery(setPath, keyPath, proofPaths, completionPath); e != nil {
			t.Fatal(e)
		}
		for _, f := range fixtures {
			if e = FinalizeClusterHost(setPath, keyPath, f.plan.Node, completionPath); e != nil {
				t.Fatal(e)
			}
			if e = offline.CheckStartup(f.plan.StateRoot); e != nil {
				t.Fatal(e)
			}
		}
		for name, c := range remoteCatalogs {
			if e = dm.WithOfflineData(c, func(path string) error {
				data, e := os.ReadFile(filepath.Join(path, "proof"))
				if e != nil {
					return e
				}
				if string(data) != "CEPH_DR_BYTES_"+name {
					return fmt.Errorf("Ceph restore lost actual bytes")
				}
				return nil
			}); e != nil {
				t.Fatal(e)
			}
		}
		t.Log("TITANUS_NATIVE_COORDINATED_CLUSTER_RECOVERY_OK", len(remoteCatalogs))
	}
	if e = os.RemoveAll(authority.Dir); e != nil {
		t.Fatal(e)
	}
	for i := range fixtures {
		open(i)
	}
	l = leader()
	converge(before.Revision)
	for i, f := range fixtures {
		data, e := os.ReadFile(filepath.Join(f.plan.StateRoot, "disks/data/data/proof"))
		if e != nil || string(data) != "NATIVE_LOCAL_DATA_"+f.plan.Node {
			t.Fatal("independent local bytes were not recovered", e)
		}
		keys, e := secrets.Load(filepath.Join(f.plan.ConfigRoot, "secrets.json"))
		if e != nil {
			t.Fatal(e)
		}
		plain, e := keys.Decrypt(nodes[i].Snapshot().Secrets["recovery"][0])
		if e != nil || string(plain) != "NATIVE_QUORUM_SECRET_RECOVERED" {
			t.Fatal("private keyring did not reconstruct functional secret", e)
		}
		got, e := source.NewManager(f.plan.StateRoot).Identity("app")
		if e != nil || got != digest {
			t.Fatal("Source bytes/identity not reconstructed", e)
		}
		if nodes[i].Status()["id"] != f.plan.Node {
			t.Fatal("restored voter has wrong logical identity")
		}
	}
	pki := filepath.Join(fixtures[l].plan.ConfigRoot, "pki")
	restoredAuthority := identity.Authority{Dir: pki, Realm: "NATIVE-DR", CertPath: filepath.Join(pki, "ca.crt"), KeyPath: filepath.Join(pki, "ca.key")}
	if _, e = stores[l].MutatePKI(func(p identity.Policy) (identity.Policy, error) { return restoredAuthority.SignPolicy(p, nil) }); e != nil {
		t.Fatal("restored signer cannot commit quorum PKI policy", e)
	}
	if e = stores[l].UpsertNode(realm.Node{ID: "after-recovery", Address: "127.0.0.1", Compatibility: func() *version.Capabilities { c := version.Compatible(); return &c }()}); e != nil {
		t.Fatal("restored Realm cannot commit new state", e)
	}
	converge(stores[l].Snapshot().Revision)
	for _, n := range nodes {
		if n.Snapshot().Nodes["after-recovery"].ID != "after-recovery" {
			t.Fatal("new post-restore state did not reach all voters")
		}
	}
	// Subsequent startup changes the original database; the old export must fail.
	if _, e = consensus.ReadOfflineState(fixtures[l].plan.StateRoot); e == nil {
		t.Fatal("stale graceful checkpoint validated a running/changed database")
	}
	if migrated {
		for _, f := range fixtures {
			if e = offline.CheckCommittedFloor(f.plan.StateRoot, "NATIVE-DR", before.SchemaMigrations[len(before.SchemaMigrations)-1].ID, before.SchemaVersion); e != nil {
				t.Fatal(e)
			}
		}
		task := stores[l].Snapshot().Tasks["unknown-proof"]
		if task.Phase != realm.TaskUnknown || task.RunID != "uncertain-do-not-replay" || task.ExecutionID != before.Tasks["unknown-proof"].ExecutionID {
			t.Fatal("restored migration replayed or changed UNKNOWN execution")
		}
		if before.SchemaVersion == 2 {
			state := stores[l].Snapshot()
			if state.TaskSchedules["restored-future"].Name == "" || len(state.SecretRotations) != 1 {
				t.Fatal("M6 scheduling/rotation state lost in DR")
			}
			t.Log("TITANUS_NATIVE_SCHEMA_TWO_BACKUP_RESTORE_OK")
		}
		t.Log("TITANUS_NATIVE_MIGRATED_QUORUM_RESTORE_OK")
	}
	t.Log("TITANUS_NATIVE_OFFLINE_QUORUM_RESTORE_OK")
	evidence := map[string]any{"architecture": runtime.GOARCH, "revision": os.Getenv("TITANUS_BACKUP_TEST_REVISION"), "voters": 3, "original_revision": before.Revision, "restored_revision": stores[l].Snapshot().Revision, "sources_verified": true, "secrets_decrypted": true, "local_bytes_verified": true, "original_roots_destroyed": true}
	encoded, _ := json.Marshal(evidence)
	t.Log(string(encoded))
}

func TestNativePendingRestoreBlocksDaemon(t *testing.T) {
	if os.Getenv("TITANUS_BACKUP_NATIVE_TEST") != "1" {
		t.Skip("opt-in disposable native daemon")
	}
	binary := os.Getenv("TITANUS_DAEMON_BINARY")
	if binary == "" {
		t.Skip("native daemon path supplied by CI")
	}
	root := filepath.Join(t.TempDir(), "state")
	if e := createPrivate(root+".recovery-pending", []byte("INJECTED_INCOMPLETE_RESTORE")); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary)
	command.Env = append(os.Environ(), "TITANUS_STATE_ROOT="+root)
	output, e := command.CombinedOutput()
	if e == nil || ctx.Err() != nil || !strings.Contains(string(output), "unfinished offline maintenance blocks startup") {
		t.Fatalf("partial restore did not stop actual daemon: %v %s", e, output)
	}
	if _, e := os.Lstat(root); !os.IsNotExist(e) {
		t.Fatal("blocked startup mutated host state")
	}
	t.Log("TITANUS_NATIVE_PENDING_RECOVERY_DAEMON_BLOCKED")
}
