package backup

import (
	"github.com/antonismor/Titanus-Core/internal/consensus"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/version"
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryIntentRejectsIncompleteMembershipAndUnknownExecution(t *testing.T) {
	f := newFixture(t)
	state := f.store.Snapshot()
	hosts := []Plan{f.plan, f.plan, f.plan}
	peers := []consensus.Peer{}
	for n := range hosts {
		hosts[n].Node = []string{"controller1", "controller2", "controller3"}[n]
		hosts[n].StateRoot = filepath.Join(f.dir, hosts[n].Node, "state")
		hosts[n].ConfigRoot = filepath.Join(f.dir, hosts[n].Node, "config")
		hosts[n].LedgerRoot = filepath.Join(f.dir, hosts[n].Node, "ledger")
		peers = append(peers, consensus.Peer{ID: hosts[n].Node, Address: []string{"127.0.0.1:10001", "127.0.0.1:10002", "127.0.0.1:10003"}[n], API: []string{"https://127.0.0.1:11001", "https://127.0.0.1:11002", "https://127.0.0.1:11003"}[n]})
	}
	intent := ClusterIntent{Format: "titanus-cluster-intent/v1", Realm: state.Name, State: state, Hosts: hosts, Build: version.Info(), Binding: ClusterBinding{ID: string64('a'), RealmSHA256: stateDigest(state), Revision: state.Revision, Peers: peers, Catalogs: map[string]disk.Catalog{}}}
	if e := intent.validate(); e != nil {
		t.Fatal(e)
	}
	missing := intent
	missing.Hosts = hosts[:2]
	if missing.validate() == nil {
		t.Fatal("missing voter accepted")
	}
	duplicate := intent
	duplicate.Hosts = append([]Plan(nil), hosts...)
	duplicate.Hosts[2] = duplicate.Hosts[1]
	if duplicate.validate() == nil {
		t.Fatal("duplicate voter accepted")
	}
	state.Nodes["worker1"] = realm.Node{ID: "worker1"}
	intent.State = state
	intent.Binding.RealmSHA256 = stateDigest(state)
	if intent.validate() == nil {
		t.Fatal("missing registered worker accepted")
	}
	delete(state.Nodes, "worker1")
	state.Tasks = map[string]realm.Task{"unobserved": {Name: "unobserved", Phase: realm.TaskUnknown}}
	intent.State = state
	intent.Binding.RealmSHA256 = stateDigest(state)
	if intent.validate() == nil {
		t.Fatal("unknown Task eligible for replay")
	}
}
func string64(c byte) string {
	b := make([]byte, 64)
	for n := range b {
		b[n] = c
	}
	return string(b)
}

func TestStorageVerificationRejectsExtraContent(t *testing.T) {
	f := newFixture(t)
	state := f.store.Snapshot()
	b := ClusterBinding{ID: string64('b'), RealmSHA256: stateDigest(state), Revision: state.Revision, Peers: []consensus.Peer{{ID: "c1", Address: "127.0.0.1:10001", API: "https://127.0.0.1:11001"}, {ID: "c2", Address: "127.0.0.1:10002", API: "https://127.0.0.1:11002"}, {ID: "c3", Address: "127.0.0.1:10003", API: "https://127.0.0.1:11003"}}, Catalogs: map[string]disk.Catalog{}}
	root := filepath.Join(f.dir, "storage")
	os.Mkdir(root, 0700)
	m := StorageManifest{Format: "titanus-ceph-data/v1", Binding: b, Entries: []Entry{}, RawImageSHA256: map[string]string{}}
	if e := writeSigned(filepath.Join(root, "manifest.json"), f.key, m); e != nil {
		t.Fatal(e)
	}
	if _, e := VerifyStorage(root, f.key); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(root, "unlisted"), []byte("untrusted"), 0600)
	if _, e := VerifyStorage(root, f.key); e == nil {
		t.Fatal("unsigned additional payload accepted")
	}
}
