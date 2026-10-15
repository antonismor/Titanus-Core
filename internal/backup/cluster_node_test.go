package backup

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/consensus"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/version"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nodeStageFixture(t *testing.T) (fixture, ClusterIntent, string) {
	f := newFixture(t)
	f.plan.Role = "node"
	a := identity.Authority{Dir: filepath.Join(f.plan.ConfigRoot, "pki"), CertPath: filepath.Join(f.plan.ConfigRoot, "pki", "ca.crt"), KeyPath: filepath.Join(f.plan.ConfigRoot, "pki", "ca.key"), Realm: f.plan.Realm}
	cert, key, e := a.Issue(f.plan.Node, []string{"127.0.0.1"}, identity.RoleNode, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	for name, path := range map[string]string{"node.crt": cert, "node.key": key} {
		data, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(f.plan.ConfigRoot, "pki", name), data, 0600); e != nil {
			t.Fatal(e)
		}
	}
	state := f.store.Snapshot()
	state.Revision += 10
	hosts := []Plan{f.plan}
	peers := []consensus.Peer{}
	for n, id := range []string{"ctl1", "ctl2", "ctl3"} {
		p := f.plan
		p.Node = id
		p.Role = "controller"
		p.StateRoot = filepath.Join(f.dir, id+"-state")
		p.ConfigRoot = filepath.Join(f.dir, id+"-configuration")
		p.LedgerRoot = filepath.Join(f.dir, id+"-ledger")
		hosts = append(hosts, p)
		peers = append(peers, consensus.Peer{ID: id, Address: fmt.Sprintf("127.0.0.1:%d", 19444+n), API: fmt.Sprintf("https://127.0.0.1:%d", 29443+n)})
		state.Nodes[id] = realm.Node{ID: id}
	}
	state.Nodes[f.plan.Node] = realm.Node{ID: f.plan.Node}
	intent := ClusterIntent{Format: "titanus-cluster-intent/v1", Realm: state.Name, State: state, Hosts: hosts, Build: version.Info(), Binding: ClusterBinding{ID: strings.Repeat("c", 64), RealmSHA256: stateDigest(state), Revision: state.Revision, Peers: peers, Catalogs: map[string]disk.Catalog{}}}
	path := filepath.Join(f.dir, "intent.json")
	if e := writeSigned(path, f.key, intent); e != nil {
		t.Fatal(e)
	}
	return f, intent, path
}
func TestSignedOfflineNonVoterCheckpointStaging(t *testing.T) {
	f, intent, path := nodeStageFixture(t)
	before, e := os.ReadFile(filepath.Join(f.plan.StateRoot, "realm", "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	previous := filepath.Join(f.dir, "previous.json")
	if e := stageClusterNode(f.plan, path, f.key, previous); e != nil {
		t.Fatal(e)
	}
	var state realm.State
	if e := loadJSON(filepath.Join(f.plan.StateRoot, "realm", "state.json"), &state); e != nil {
		t.Fatal(e)
	}
	if e := intent.Binding.checkState(state); e != nil {
		t.Fatal(e)
	}
	var saved realm.State
	if e := readSigned(previous, f.key, &saved); e != nil {
		t.Fatal(e)
	}
	if saved.Revision == state.Revision {
		t.Fatal("preimage lost")
	}
	if len(before) == 0 {
		t.Fatal("empty preimage")
	}
}
func TestNodeStagingRefusesVotersAndMissingSourcesWithoutMutation(t *testing.T) {
	for _, mode := range []string{"role", "consensus", "missing-source", "bad-signature"} {
		t.Run(mode, func(t *testing.T) {
			f, _, path := nodeStageFixture(t)
			statePath := filepath.Join(f.plan.StateRoot, "realm", "state.json")
			before, _ := os.ReadFile(statePath)
			switch mode {
			case "role":
				f.plan.Role = "controller"
			case "consensus":
				os.Mkdir(filepath.Join(f.plan.StateRoot, "realm", "consensus"), 0700)
			case "missing-source":
				os.RemoveAll(filepath.Join(f.plan.StateRoot, "sources", "app"))
			case "bad-signature":
				os.WriteFile(path, []byte("{}"), 0600)
			}
			if e := stageClusterNode(f.plan, path, f.key, filepath.Join(f.dir, "previous.json")); e == nil {
				t.Fatal("unsafe stage accepted")
			}
			after, _ := os.ReadFile(statePath)
			if string(after) != string(before) {
				t.Fatal("rejected stage changed local cache")
			}
		})
	}
}

func TestPublicNodeStagingRequiresRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root runner")
	}
	if err := StageClusterNode(Plan{}, "", "", ""); err == nil || !strings.Contains(err.Error(), "requires root") {
		t.Fatal(err)
	}
}
