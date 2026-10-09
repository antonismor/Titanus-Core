package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"github.com/antonismor/Titanus-Core/internal/version"
)

type fixture struct {
	plan                     Plan
	dir, key, archive, fence string
	store                    *realm.Store
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	p := Plan{"titanus-backup-plan/v1", "BACKUP-LAB", "controller1", "controller", filepath.Join(dir, "state"), filepath.Join(dir, "configuration"), filepath.Join(dir, "ledger")}
	for _, root := range p.roots() {
		if e := os.Mkdir(root, 0700); e != nil {
			t.Fatal(e)
		}
	}
	a, e := identity.InitAuthority(filepath.Join(p.ConfigRoot, "pki"), p.Realm)
	if e != nil {
		t.Fatal(e)
	}
	cert, key, e := a.Issue(p.Node, []string{"127.0.0.1"}, identity.RoleController, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	for name, file := range map[string]string{"node.crt": cert, "node.key": key} {
		data, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(p.ConfigRoot, "pki", name), data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e = os.WriteFile(filepath.Join(p.ConfigRoot, "daemon.env"), []byte("TITANUS_REALM_NAME="+p.Realm+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	store, e := realm.Open(p.StateRoot, p.Realm)
	if e != nil {
		t.Fatal(e)
	}
	policy, e := a.InitialPolicy()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = store.MutatePKI(func(identity.Policy) (identity.Policy, error) { return policy, nil }); e != nil {
		t.Fatal(e)
	}
	keyring := secrets.Keyring{Active: "one", Keys: map[string][]byte{"one": bytes.Repeat([]byte{9}, 32)}}
	if e = durable.WriteJSON(filepath.Join(p.ConfigRoot, "secrets.json"), keyring, 0600); e != nil {
		t.Fatal(e)
	}
	record, e := keyring.Encrypt(p.Realm, "credential", 1, []byte("RECOVERED_SECRET_VALUE"))
	if e != nil {
		t.Fatal(e)
	}
	if e = store.PutSecret(record); e != nil {
		t.Fatal(e)
	}
	src := filepath.Join(dir, "source-input")
	os.Mkdir(src, 0755)
	if e = os.WriteFile(filepath.Join(src, "proof"), []byte("SOURCE_BYTES_OK"), 0644); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink("proof", filepath.Join(src, "alias")); e != nil {
		t.Fatal(e)
	}
	sm := source.NewManager(p.StateRoot)
	if e = sm.ImportDirectory("app", src); e != nil {
		t.Fatal(e)
	}
	digest, e := sm.Identity("app")
	if e != nil {
		t.Fatal(e)
	}
	if e = store.PutSourceRecord(realm.SourceRecord{Name: "app", Digest: digest}); e != nil {
		t.Fatal(e)
	}
	dm := disk.NewManager(p.StateRoot)
	if _, e = dm.Create(disk.Spec{Name: "data", Provider: disk.ProviderLocal, SizeBytes: 64 << 20}); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(p.StateRoot, "disks/data/data/proof"), []byte("LOCAL_DISK_BYTES_OK"), 0640); e != nil {
		t.Fatal(e)
	}
	runtime := unitruntime.NewManager(unitruntime.Config{StateRoot: p.StateRoot})
	if _, e = runtime.Create(unitruntime.Spec{ID: "saved", Source: "app", Command: []string{"/bin/sh"}}); e != nil {
		t.Fatal(e)
	}
	keyPath := filepath.Join(dir, "external-backup.key")
	if e = Keygen(keyPath); e != nil {
		t.Fatal(e)
	}
	return fixture{p, dir, keyPath, filepath.Join(dir, "backup.tar.gz"), filepath.Join(dir, "fence.json"), store}
}

func (f fixture) capture(t *testing.T) Manifest {
	t.Helper()
	m, e := create(f.plan, f.archive, f.key)
	if e != nil {
		t.Fatal(e)
	}
	return m
}

func (f fixture) attest(t *testing.T) {
	t.Helper()
	record := filepath.Join(f.dir, "independent-exclusion.json")
	if e := durable.WriteJSON(record, FenceRecord{f.plan.Realm, f.plan.Node, true, true, true, "Disposable fixture: all previous actors stopped; no external gateways/storage clients."}, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := AttestFence(f.archive, f.key, record, f.fence); e != nil {
		t.Fatal(e)
	}
}

func (f fixture) loseRoots(t *testing.T) {
	t.Helper()
	for _, p := range f.plan.roots() {
		if e := os.RemoveAll(p); e != nil {
			t.Fatal(e)
		}
	}
}

func TestFunctionalOfflineRestore(t *testing.T) {
	f := newFixture(t)
	m := f.capture(t)
	f.attest(t)
	if _, e := restore(f.archive, f.key, f.fence, f.plan, nil); e == nil {
		t.Fatal("overwrote existing identity/data")
	}
	f.loseRoots(t)
	recovered, e := restore(f.archive, f.key, f.fence, f.plan, nil)
	if e != nil {
		t.Fatal(e)
	}
	if recovered.ID != m.ID {
		t.Fatal("wrong backup restored")
	}
	store, e := realm.Open(f.plan.StateRoot, f.plan.Realm)
	if e != nil {
		t.Fatal(e)
	}
	keyring, e := secrets.Load(filepath.Join(f.plan.ConfigRoot, "secrets.json"))
	if e != nil {
		t.Fatal(e)
	}
	plain, e := keyring.Decrypt(store.Snapshot().Secrets["credential"][0])
	if e != nil || string(plain) != "RECOVERED_SECRET_VALUE" {
		t.Fatal("secret not functionally reconstructed", e)
	}
	digest, e := source.NewManager(f.plan.StateRoot).Identity("app")
	if e != nil || digest != store.Snapshot().Sources["app"].Digest {
		t.Fatal("Source identity not reconstructed")
	}
	if _, e := identity.TLSConfig(filepath.Join(f.plan.ConfigRoot, "pki/ca.crt"), filepath.Join(f.plan.ConfigRoot, "pki/node.crt"), filepath.Join(f.plan.ConfigRoot, "pki/node.key"), false); e != nil {
		t.Fatal("PKI not functional", e)
	}
	dataRoot, e := disk.NewManager(f.plan.StateRoot).Resolve("data")
	if e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(filepath.Join(dataRoot, "proof"))
	if e != nil || string(data) != "LOCAL_DISK_BYTES_OK" {
		t.Fatal("Disk bytes missing", e)
	}
	if e := os.WriteFile(filepath.Join(dataRoot, "new-proof"), []byte("RECOVERED_DATA_WRITABLE"), 0600); e != nil {
		t.Fatal(e)
	}
	manager := unitruntime.NewManager(unitruntime.Config{StateRoot: f.plan.StateRoot})
	if _, state, e := manager.Inspect("saved"); e != nil || state.Status != unitruntime.StatusCreated {
		t.Fatal("Unit catalog missing", e)
	}
	if e := store.UpsertNode(realm.Node{ID: "after-restore", Address: "127.0.0.1"}); e != nil {
		t.Fatal("recovered Realm not writable", e)
	}
	if e := offline.CheckStartup(f.plan.StateRoot); e != nil {
		t.Fatal(e)
	}
	f.loseRoots(t)
	if _, e := restore(f.archive, f.key, f.fence, f.plan, nil); e == nil {
		t.Fatal("silently replayed a consumed recovery")
	}
}

func TestBackupRefusesIncompleteOrBusyState(t *testing.T) {
	for _, kind := range []string{"live-unit", "missing-keyring", "wrong-node", "ledger-conflict", "remote-data", "private-key-mode", "hardlink", "xattr", "busy-disk", "symlink-root"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			switch kind {
			case "live-unit":
				var s unitruntime.State
				path := filepath.Join(f.plan.StateRoot, "units/saved/state.json")
				loadJSON(path, &s)
				s.PID = os.Getpid()
				s.Status = unitruntime.StatusActive
				durable.WriteJSON(path, s, 0600)
			case "missing-keyring":
				os.Remove(filepath.Join(f.plan.ConfigRoot, "secrets.json"))
			case "wrong-node":
				f.plan.Node = "other"
			case "ledger-conflict":
				durable.WriteJSON(filepath.Join(f.plan.LedgerRoot, "allocations.json"), map[string]unitruntime.IDMapping{"a": {Base: 1048576, Size: 65536}, "b": {Base: 1048576, Size: 65536}}, 0600)
			case "remote-data":
				var d disk.Spec
				p := filepath.Join(f.plan.StateRoot, "disks/data/disk.json")
				loadJSON(p, &d)
				d.Provider = disk.ProviderCephRBD
				durable.WriteJSON(p, d, 0600)
			case "private-key-mode":
				os.Chmod(filepath.Join(f.plan.ConfigRoot, "pki/node.key"), 0644)
			case "hardlink":
				if e := os.Link(filepath.Join(f.plan.StateRoot, "disks/data/data/proof"), filepath.Join(f.plan.StateRoot, "disks/data/data/hardlink")); e != nil {
					t.Fatal(e)
				}
			case "xattr":
				if e := syscall.Setxattr(filepath.Join(f.plan.StateRoot, "disks/data/data/proof"), "user.test", []byte("required-metadata"), 0); e != nil {
					t.Skipf("xattr unavailable: %v", e)
				}
			case "busy-disk":
				dir := filepath.Join(f.plan.StateRoot, "disks/data/control")
				os.Mkdir(dir, 0700)
				lock, e := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
				if e != nil {
					t.Fatal(e)
				}
				defer lock.Close()
				if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
					t.Fatal(e)
				}
			case "symlink-root":
				original := f.plan.StateRoot
				os.Rename(original, original+"-actual")
				os.Symlink(original+"-actual", original)
			}
			if _, e := create(f.plan, f.archive, f.key); e == nil {
				t.Fatal("unsafe backup accepted")
			}
			if _, e := os.Lstat(f.archive); !os.IsNotExist(e) {
				t.Fatal("failed backup published")
			}
		})
	}
}

// Rewrite a payload without changing the signed manifest, or construct an
// authenticated malformed inventory to exercise extraction's structural gate.
func rewriteArchive(t *testing.T, f fixture, modify func(*Manifest, *tar.Header, []byte) (*tar.Header, []byte)) string {
	t.Helper()
	data, e := os.ReadFile(f.archive)
	if e != nil {
		t.Fatal(e)
	}
	gz, e := gzip.NewReader(bytes.NewReader(data))
	if e != nil {
		t.Fatal(e)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	out := bytes.Buffer{}
	writer := gzip.NewWriter(&out)
	tw := tar.NewWriter(writer)
	key, e := loadKey(f.key)
	if e != nil {
		t.Fatal(e)
	}
	defer clear(key)
	var manifest Manifest
	for {
		h, e := reader.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		content, e := io.ReadAll(reader)
		if e != nil {
			t.Fatal(e)
		}
		if h.Name == "manifest.json" {
			if e = authenticate(key, content, &manifest); e != nil {
				t.Fatal(e)
			}
		}
		h, content = modify(&manifest, h, content)
		if h == nil {
			continue
		}
		h.Size = int64(len(content))
		if e = tw.WriteHeader(h); e != nil {
			t.Fatal(e)
		}
		if _, e = tw.Write(content); e != nil {
			t.Fatal(e)
		}
	}
	if e = tw.Close(); e != nil {
		t.Fatal(e)
	}
	if e = writer.Close(); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(f.dir, "modified.tar.gz")
	if e = os.WriteFile(path, out.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	return path
}

func TestIntegrityAuthenticationAndTraversalDenial(t *testing.T) {
	for _, kind := range []string{"data", "mode", "unlisted", "traversal", "wrong-key", "gzip-trailer", "compressed-tail"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.capture(t)
			archive := f.archive
			keyPath := f.key
			switch kind {
			case "wrong-key":
				keyPath = filepath.Join(f.dir, "wrong.key")
				if e := Keygen(keyPath); e != nil {
					t.Fatal(e)
				}
			case "gzip-trailer":
				data, _ := os.ReadFile(archive)
				data[len(data)-6] ^= 1
				os.WriteFile(archive, data, 0600)
			case "compressed-tail":
				file, _ := os.OpenFile(archive, os.O_APPEND|os.O_WRONLY, 0600)
				file.Write([]byte("TRAILING_DATA"))
				file.Close()
			default:
				archive = rewriteArchive(t, f, func(m *Manifest, h *tar.Header, data []byte) (*tar.Header, []byte) {
					if h.Name == "state/disks/data/data/proof" {
						switch kind {
						case "data":
							data = []byte("LOCAL_DISK_BYTES_NO")
						case "mode":
							h.Mode = 0777
						case "unlisted":
							h.Name = "state/unlisted"
						case "traversal":
							h.Name = "../../escaped"
						}
					}
					return h, data
				})
			}
			if _, e := Verify(archive, keyPath); e == nil {
				t.Fatal("tampered backup verified")
			}
		})
	}
	for _, entry := range []Entry{{Path: "state/../outside", Type: "file", SHA256: strings.Repeat("a", 64)}, {Path: "state/link", Type: "symlink", Link: "/tmp/outside"}, {Path: "state/link/child", Type: "file", SHA256: strings.Repeat("a", 64)}} {
		f := newFixture(t)
		m := f.capture(t)
		m.Entries = append(m.Entries, entry)
		if entry.Path == "state/link" {
			m.Entries = append(m.Entries, Entry{Path: "state/link/child", Type: "file", SHA256: strings.Repeat("a", 64)})
		}
		if e := m.validate(); e == nil {
			t.Fatal("unsafe signed parent/path accepted")
		}
	}
}

func TestFenceBindingAndInterruptedRestore(t *testing.T) {
	for _, kind := range []string{"missing", "wrong-backup", "expired", "wrong-plan", "interrupted"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			f.capture(t)
			f.attest(t)
			f.loseRoots(t)
			p := f.plan
			fencePath := f.fence
			if kind == "missing" {
				fencePath = filepath.Join(f.dir, "missing")
			}
			if kind == "wrong-plan" {
				p.Node = "controller2"
			}
			if kind == "wrong-backup" || kind == "expired" {
				key, _ := loadKey(f.key)
				data, _ := os.ReadFile(f.fence)
				var fence Fence
				authenticate(key, data, &fence)
				if kind == "wrong-backup" {
					fence.BackupID = strings.Repeat("1", 64)
				} else {
					fence.IssuedAt = time.Now().Add(-time.Hour)
					fence.ExpiresAt = time.Now().Add(-time.Minute)
				}
				data, _ = sign(key, fence)
				clear(key)
				os.WriteFile(f.fence, data, 0600)
			}
			var hook func(int) error
			if kind == "interrupted" {
				hook = func(int) error { return fmt.Errorf("simulated power loss after first rename") }
			}
			if _, e := restore(f.archive, f.key, fencePath, p, hook); e == nil {
				t.Fatal("unsafe recovery accepted")
			}
			if kind == "interrupted" {
				if e := offline.CheckStartup(p.StateRoot); e == nil {
					t.Fatal("partial recovery did not block startup")
				}
				if _, e := restore(f.archive, f.key, f.fence, p, nil); e == nil {
					t.Fatal("partial restore silently replayed")
				}
			} else {
				if _, e := os.Lstat(p.StateRoot + ".recovery-pending"); !os.IsNotExist(e) {
					t.Fatal("failed preflight touched recovery targets")
				}
			}
		})
	}
}

func TestMappedOwnershipLedgerPreserved(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("numeric ownership needs root")
	}
	f := newFixture(t)
	statePath := filepath.Join(f.plan.StateRoot, "units/saved/state.json")
	var state unitruntime.State
	if e := loadJSON(statePath, &state); e != nil {
		t.Fatal(e)
	}
	state.UserMapping = unitruntime.IDMapping{Base: 1048576, Size: 65536}
	key := filepath.Join(f.plan.StateRoot, "units", state.ID) + "@" + state.CreatedAt.Format("20060102T150405.000000000Z")
	if e := durable.WriteJSON(statePath, state, 0600); e != nil {
		t.Fatal(e)
	}
	if e := durable.WriteJSON(filepath.Join(f.plan.LedgerRoot, "allocations.json"), map[string]unitruntime.IDMapping{key: state.UserMapping, "retired": {Base: 1114112, Size: 65536}}, 0600); e != nil {
		t.Fatal(e)
	}
	dataPath := filepath.Join(f.plan.StateRoot, "disks/data/data/proof")
	if e := os.Chown(dataPath, state.UserMapping.Base+100, state.UserMapping.Base+200); e != nil {
		if os.Getenv("TITANUS_BACKUP_NATIVE_TEST") == "1" {
			t.Fatal(e)
		}
		t.Skipf("mapped numeric ownership is unavailable in this environment: %v", e)
	}
	f.capture(t)
	f.attest(t)
	f.loseRoots(t)
	if _, e := restore(f.archive, f.key, f.fence, f.plan, nil); e != nil {
		t.Fatal(e)
	}
	st, e := os.Stat(dataPath)
	if e != nil {
		t.Fatal(e)
	}
	owner := st.Sys().(*syscall.Stat_t)
	if owner.Uid != 1048676 || owner.Gid != 1048776 || st.Mode().Perm() != 0640 {
		t.Fatal("numeric owner/mode not preserved")
	}
	var ledger map[string]unitruntime.IDMapping
	if e := loadJSON(filepath.Join(f.plan.LedgerRoot, "allocations.json"), &ledger); e != nil || ledger[key] != state.UserMapping || ledger["retired"].Base != 1114112 {
		t.Fatal("live/retired mapping identities lost", e)
	}
	var receipt map[string]any
	data, _ := os.ReadFile(f.plan.StateRoot + ".recovery-receipt.json")
	json.Unmarshal(data, &receipt)
	if receipt["status"] != "complete" {
		t.Fatal("missing completed receipt")
	}
	t.Log("TITANUS_NATIVE_BACKUP_NUMERIC_OWNERSHIP_OK")
}

func TestMigratedBackupRestoresRollbackFloorAndImmutableUnknownTask(t *testing.T) {
	f := newFixture(t)
	if e := f.store.CreateTask(realm.Task{Name: "unknown-proof", Template: realm.UnitTemplate{Source: "app", Command: []string{"/bin/sh"}}}); e != nil {
		t.Fatal(e)
	}
	task := f.store.Snapshot().Tasks["unknown-proof"]
	task.Phase = realm.TaskUnknown
	task.RunID = "uncertain-original"
	if e := f.store.UpdateTask(task); e != nil {
		t.Fatal(e)
	}
	id := "backup-migration-proof-001"
	if _, e := f.store.TransitionSchema(id, f.store.Snapshot().Revision, map[string]version.Capabilities{f.plan.Node: version.Compatible()}, []string{f.plan.Node}); e != nil {
		t.Fatal(e)
	}
	before := f.store.Snapshot()
	f.capture(t)
	f.attest(t)
	f.loseRoots(t)
	if e := os.Remove(f.plan.StateRoot + ".recovery-pending"); e != nil {
		t.Fatal(e)
	}
	if _, e := restore(f.archive, f.key, f.fence, f.plan, nil); e != nil {
		t.Fatal(e)
	}
	if e := offline.CheckCommittedFloor(f.plan.StateRoot, f.plan.Realm, id, 1); e != nil {
		t.Fatal(e)
	}
	restored, e := realm.Open(f.plan.StateRoot, f.plan.Realm)
	if e != nil {
		t.Fatal(e)
	}
	got := restored.Snapshot().Tasks["unknown-proof"]
	if got.Phase != realm.TaskUnknown || got.RunID != task.RunID || got.ExecutionID != before.Tasks["unknown-proof"].ExecutionID {
		t.Fatal("restore lost immutable uncertain execution")
	}
	if e := os.Remove(f.plan.StateRoot + ".recovery-pending"); e != nil {
		t.Fatal(e)
	}
	if _, e = realm.Open(f.plan.StateRoot, f.plan.Realm); e == nil {
		t.Fatal("migrated state without legacy blocker opened")
	}
}
