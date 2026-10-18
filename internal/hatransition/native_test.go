package hatransition_test

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/backup"
	"github.com/antonismor/Titanus-Core/internal/consensus"
	"github.com/antonismor/Titanus-Core/internal/controlapi"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/fencing"
	"github.com/antonismor/Titanus-Core/internal/hatransition"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/realmclient"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/version"
)

// All five identities live in temporary directories on an isolated runner.
// No infrastructure provider is called: libvirt targets are packaged only.
func TestNativeAuthenticatedProfileTransition(t *testing.T) {
	if os.Getenv("TITANUS_HA_TRANSITION_NATIVE_TEST") != "1" {
		t.Skip("opt-in isolated native quorum/backup transition")
	}
	if os.Geteuid() != 0 {
		t.Fatal("private backup transition requires root")
	}
	must := func(e error) {
		t.Helper()
		if e != nil {
			t.Fatal(e)
		}
	}
	dir := t.TempDir()
	authority, e := identity.InitAuthority(filepath.Join(dir, "authority"), "NATIVE-TRANSITION")
	must(e)
	target := model.RealmPlan{Version: "titanus-plan/v2", RealmName: "NATIVE-TRANSITION", FabricCIDR: "10.210.0.0/16", ServiceCIDR: "10.220.0.0/16", Installation: &model.Installation{Profile: model.ProfileNativeHA, Operation: "install", ReleaseVersion: version.Version, Revision: version.Revision, APIPort: 9443, RaftPort: 9444, NodePrefix: 24, VXLANID: 4242, Archives: map[string]model.Archive{"amd64": {Path: "fixture-amd.tar.gz", SHA256: strings.Repeat("b", 64)}, "arm64": {Path: "fixture-arm.tar.gz", SHA256: strings.Repeat("c", 64)}}, PowerFencing: map[string]fencing.Target{}}}
	var apiListeners [5]net.Listener
	var raftListeners [3]net.Listener
	var plans [5]backup.Plan
	var stores [5]*realm.Store
	var voters [3]*consensus.Node
	var configs [3]consensus.Config
	var servers [5]*httptest.Server
	// Reserve common ports on distinct local IPs, preserving the actual static
	// identity/address model used by the installer and transition validator.
	for i := 0; i < 5; i++ {
		ip := fmt.Sprintf("127.0.0.%d", 31+i)
		port := 0
		if i > 0 {
			port = target.Installation.APIPort
		}
		apiListeners[i], e = net.Listen("tcp", fmt.Sprintf("%s:%d", ip, port))
		must(e)
		if i == 0 {
			target.Installation.APIPort = apiListeners[i].Addr().(*net.TCPAddr).Port
		}
		cap := model.CapabilityControl
		if i >= 3 {
			cap = model.CapabilityExecution
		}
		name := fmt.Sprintf("host%d", i+1)
		target.Nodes = append(target.Nodes, model.NodeSpec{Name: name, ManagementIP: ip, SSHUser: "root", SSHPort: 22, Capabilities: []model.Capability{cap}})
		if i >= 3 {
			target.Installation.PowerFencing[name] = fencing.Target{Provider: "libvirt", Address: "qemu+ssh://root@fixture.invalid/system", UUID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1)}
		}
	}
	peers := []consensus.Peer{}
	for i := 0; i < 3; i++ {
		port := 0
		if i > 0 {
			port = target.Installation.RaftPort
		}
		raftListeners[i], e = net.Listen("tcp", fmt.Sprintf("%s:%d", target.Nodes[i].ManagementIP, port))
		must(e)
		if i == 0 {
			target.Installation.RaftPort = raftListeners[i].Addr().(*net.TCPAddr).Port
		}
		peers = append(peers, consensus.Peer{ID: target.Nodes[i].Name, Address: raftListeners[i].Addr().String(), API: "https://" + apiListeners[i].Addr().String()})
	}
	t.Cleanup(func() {
		for _, s := range servers {
			if s != nil {
				s.Close()
			}
		}
		for _, v := range voters {
			if v != nil {
				v.Close()
			}
		}
		for _, l := range apiListeners {
			l.Close()
		}
		for _, l := range raftListeners {
			l.Close()
		}
	})
	must(target.Validate())
	for i, n := range target.Nodes {
		root := filepath.Join(dir, n.Name)
		role := "node"
		certRole := identity.RoleNode
		if i < 3 {
			role = "controller"
			certRole = identity.RoleController
		}
		p := backup.Plan{Format: "titanus-backup-plan/v1", Realm: target.RealmName, Node: n.Name, Role: role, StateRoot: filepath.Join(root, "state"), ConfigRoot: filepath.Join(root, "config"), LedgerRoot: filepath.Join(root, "ledger")}
		plans[i] = p
		for _, r := range []string{p.StateRoot, p.ConfigRoot, p.LedgerRoot, filepath.Join(p.ConfigRoot, "pki")} {
			must(os.MkdirAll(r, 0700))
		}
		cert, key, e := authority.Issue(n.Name, []string{n.ManagementIP}, certRole, time.Hour)
		must(e)
		files := map[string]string{"ca.crt": authority.CertPath, "ca.crl": filepath.Join(authority.Dir, "ca.crl"), "node.crt": cert, "node.key": key}
		if i < 3 {
			files["ca.key"] = authority.KeyPath
		}
		for name, src := range files {
			b, e := os.ReadFile(src)
			must(e)
			must(os.WriteFile(filepath.Join(p.ConfigRoot, "pki", name), b, 0600))
		}
		must(os.WriteFile(filepath.Join(p.ConfigRoot, "daemon.env"), []byte("TITANUS_NODE_ID="+n.Name+"\nOPERATOR_SETTING=preserved\n"), 0600))
		caps := "EXECUTION"
		if i < 3 {
			caps = "CONTROL,EXECUTION"
		}
		must(os.WriteFile(filepath.Join(p.ConfigRoot, "agent.env"), []byte("TITANUS_CAPABILITIES="+caps+"\nOPERATOR_SETTING=preserved\n"), 0600))
		stores[i], e = realm.Open(p.StateRoot, p.Realm)
		must(e)
		must(stores[i].ConfigureNetwork(realm.RealmNetwork{FabricCIDR: target.FabricCIDR, ServiceCIDR: target.ServiceCIDR, NodePrefix: 24, VXLANID: 4242}))
		src := filepath.Join(root, "source")
		must(os.Mkdir(src, 0700))
		must(os.WriteFile(filepath.Join(src, "proof"), []byte("SOURCE_BYTES_PRESERVED_"+n.Name), 0600))
		must(source.NewManager(p.StateRoot).ImportDirectory("app", src))
		if i < 3 {
			configs[i] = consensus.Config{ID: p.Node, Realm: p.Realm, Peers: peers, Bootstrap: i == 0}
			must(durable.WriteJSON(filepath.Join(p.ConfigRoot, "ha.json"), configs[i], 0600))
		}
	}
	open := func(i int) {
		t.Helper()
		p := plans[i]
		stores[i], e = realm.Open(p.StateRoot, p.Realm)
		must(e)
		pk := filepath.Join(p.ConfigRoot, "pki")
		voters[i], e = consensus.Open(p.StateRoot, configs[i], stores[i].Snapshot(), filepath.Join(pk, "ca.crt"), filepath.Join(pk, "node.crt"), filepath.Join(pk, "node.key"))
		must(e)
		must(stores[i].EnableConsensus(voters[i]))
	}
	leader := func() int {
		t.Helper()
		until := time.Now().Add(20 * time.Second)
		for time.Now().Before(until) {
			for i, v := range voters {
				if v != nil && v.Initialize() == nil && v.CheckLeader() == nil {
					return i
				}
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatal("no leader")
		return -1
	}
	converge := func(rev uint64) {
		t.Helper()
		until := time.Now().Add(10 * time.Second)
		for time.Now().Before(until) {
			ok := true
			for _, v := range voters {
				if v.Snapshot().Revision != rev {
					ok = false
				}
			}
			if ok {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("voters did not converge")
	}
	actor, e := offline.Shared()
	must(e)
	t.Cleanup(func() { actor.Close() })
	for i := range voters {
		raftListeners[i].Close()
		open(i)
	}
	l := leader()
	compat := version.Compatible()
	for i, n := range target.Nodes {
		caps := append([]model.Capability{}, n.Capabilities...)
		if i < 3 {
			caps = append(caps, model.CapabilityExecution)
		}
		must(stores[l].UpsertNode(realm.Node{ID: n.Name, Address: apiListeners[i].Addr().String(), Capabilities: caps, Compatibility: &compat, State: realm.NodeReady, LastPulse: time.Now().UTC()}))
	}
	policy, e := authority.InitialPolicy()
	must(e)
	_, e = stores[l].MutatePKI(func(identity.Policy) (identity.Policy, error) { return policy, nil })
	must(e)
	before := stores[l].Snapshot()
	converge(before.Revision)
	for i, v := range voters {
		must(v.Close())
		voters[i] = nil
	}
	must(actor.Close())
	keyPath := filepath.Join(dir, "external.key")
	must(backup.Keygen(keyPath))
	hostsPath := filepath.Join(dir, "hosts.json")
	must(durable.WriteJSON(hostsPath, plans, 0600))
	intentPath := filepath.Join(dir, "intent.json")
	_, e = backup.CreateClusterIntent(plans[0], hostsPath, keyPath, intentPath)
	must(e)
	storagePath := filepath.Join(dir, "storage")
	_, e = backup.ExportStorage(plans[0].StateRoot, intentPath, keyPath, storagePath)
	must(e)
	setPlan := backup.ClusterSetPlan{Intent: intentPath, Storage: storagePath}
	for i, p := range plans {
		if i >= 3 {
			must(backup.StageClusterNode(p, intentPath, keyPath, filepath.Join(dir, p.Node+"-previous.json")))
		}
		archive := filepath.Join(dir, p.Node+".tar.gz")
		_, e = backup.CreateClusterHost(p, archive, keyPath, intentPath, storagePath)
		must(e)
		setPlan.Hosts = append(setPlan.Hosts, backup.ClusterHost{Plan: p, Archive: archive})
	}
	setPlanPath := filepath.Join(dir, "set-plan.json")
	must(durable.WriteJSON(setPlanPath, setPlan, 0600))
	setPath := filepath.Join(dir, "set.json")
	_, e = backup.SealClusterSet(setPlanPath, keyPath, setPath)
	must(e)
	// Compare every consensus byte and protected environment across stage/rollback.
	original := map[string][]byte{}
	for i, p := range plans {
		for _, name := range []string{"daemon.env", "agent.env"} {
			path := filepath.Join(p.ConfigRoot, name)
			original[path], e = os.ReadFile(path)
			must(e)
		}
		if i < 3 {
			must(filepath.WalkDir(filepath.Join(p.StateRoot, "realm/consensus"), func(path string, d os.DirEntry, e error) error {
				if e != nil {
					return e
				}
				if !d.IsDir() {
					original[path], e = os.ReadFile(path)
				}
				return e
			}))
		}
	}
	preparedDir := filepath.Join(dir, "prepared-rollback")
	currentDir, e := os.Getwd()
	must(e)
	relativeOutput, e := filepath.Rel(currentDir, preparedDir)
	must(e)
	p, e := hatransition.Prepare(target, setPath, keyPath, relativeOutput)
	must(e)
	preparedPath := filepath.Join(preparedDir, "transition.json")
	// Recomputed artifact IDs cannot redirect authenticated backup roots.
	altered := p
	altered.Hosts = append(altered.Hosts[:0:0], p.Hosts...)
	altered.Hosts[0].ConfigRoot = filepath.Join(dir, "redirected")
	altered.ID = ""
	altered.ID = hatransition.Digest(altered)
	alteredPath := filepath.Join(dir, "altered.json")
	must(durable.WriteJSON(alteredPath, altered, 0600))
	if hatransition.Stage(alteredPath, setPath, keyPath, plans[0].Node) == nil {
		t.Fatal("edited host roots accepted")
	}
	fencePath := filepath.Join(preparedDir, "fencing/power-fencing.json")
	fenceBytes, e := os.ReadFile(fencePath)
	must(e)
	must(os.WriteFile(fencePath, []byte("{}"), 0600))
	if hatransition.Stage(preparedPath, setPath, keyPath, plans[0].Node) == nil {
		t.Fatal("changed prepared fencing accepted")
	}
	must(os.WriteFile(fencePath, fenceBytes, 0600))
	for _, h := range plans {
		must(hatransition.Stage(preparedPath, setPath, keyPath, h.Node))
		must(hatransition.Stage(preparedPath, setPath, keyPath, h.Node))
		_, e = hatransition.ReadReady(h.StateRoot, p.ID)
		must(e)
	}
	must(os.WriteFile(filepath.Join(plans[4].ConfigRoot, "agent.env"), []byte("operator conflict\n"), 0600))
	if _, e = hatransition.ReadReady(plans[4].StateRoot, p.ID); e == nil {
		t.Fatal("changed staged env accepted")
	}
	if hatransition.RollbackStaging(plans[4].StateRoot, p.ID, strings.Repeat("independent exclusion evidence ", 4)) == nil {
		t.Fatal("operator conflict overwritten")
	}
	rPath := filepath.Join(plans[4].StateRoot, "ha-transitions", p.ID, "receipt.json")
	var r struct {
		Files map[string]struct {
			After []byte `json:"after"`
		} `json:"files"`
	}
	b, e := os.ReadFile(rPath)
	must(e)
	must(json.Unmarshal(b, &r))
	must(os.WriteFile(filepath.Join(plans[4].ConfigRoot, "agent.env"), r.Files["agent.env"].After, 0600))
	for _, h := range plans {
		must(hatransition.RollbackStaging(h.StateRoot, p.ID, strings.Repeat("independently confirmed no activation and all actors excluded; ", 2)))
	}
	for path, before := range original {
		after, e := os.ReadFile(path)
		must(e)
		if !bytes.Equal(before, after) {
			t.Fatalf("rollback changed protected bytes %s", path)
		}
	}
	// Prepare a second immutable artifact and commit the real transition over mTLS.
	target.CreatedAt = time.Now().UTC()
	preparedDir = filepath.Join(dir, "prepared-activate")
	p, e = hatransition.Prepare(target, setPath, keyPath, preparedDir)
	must(e)
	preparedPath = filepath.Join(preparedDir, "transition.json")
	for _, h := range plans {
		must(hatransition.Stage(preparedPath, setPath, keyPath, h.Node))
	}
	actor, e = offline.Shared()
	must(e)
	for i := range voters {
		open(i)
	}
	l = leader()
	for i, n := range target.Nodes {
		must(stores[l].UpsertNode(realm.Node{ID: n.Name, Address: apiListeners[i].Addr().String(), Capabilities: n.Capabilities, Compatibility: &compat, State: realm.NodeReady, LastPulse: time.Now().UTC()}))
	}
	for i, h := range plans {
		if i >= 3 {
			stores[i], e = realm.Open(h.StateRoot, h.Realm)
			must(e)
		}
		pk := filepath.Join(h.ConfigRoot, "pki")
		client, e := realmclient.New(filepath.Join(pk, "ca.crt"), filepath.Join(pk, "node.crt"), filepath.Join(pk, "node.key"))
		must(e)
		api := controlapi.New(stores[i], nil, nil, nil)
		api.StateRoot = h.StateRoot
		api.NodeID = h.Node
		api.HATransitionClient = client
		api.TransitionClient = client
		if i < 3 {
			api.Consensus = voters[i]
			for _, peer := range peers {
				api.SchemaPeers = append(api.SchemaPeers, controlapi.SchemaPeer{ID: peer.ID, API: peer.API})
			}
			api.PowerFencer, e = fencing.Load(filepath.Join(h.ConfigRoot, "native-ha", p.ID, "power-fencing.json"))
			must(e)
		}
		mux := http.NewServeMux()
		api.Register(mux)
		srv := httptest.NewUnstartedServer(identity.Authenticate(mux, filepath.Join(pk, "ca.crt")))
		srv.Listener.Close()
		srv.Listener = apiListeners[i]
		srv.TLS, e = identity.TLSConfig(filepath.Join(pk, "ca.crt"), filepath.Join(pk, "node.crt"), filepath.Join(pk, "node.key"), true)
		must(e)
		pair, e := tls.LoadX509KeyPair(filepath.Join(pk, "node.crt"), filepath.Join(pk, "node.key"))
		must(e)
		srv.TLS.Certificates = []tls.Certificate{pair}
		srv.StartTLS()
		servers[i] = srv
	}
	cert, key, e := authority.Issue("admin", nil, identity.RoleAdmin, time.Hour)
	must(e)
	tlsCfg, e := identity.TLSConfig(authority.CertPath, cert, key, false)
	must(e)
	admin := http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg, DisableKeepAlives: true}, Timeout: 20 * time.Second}
	post := func(path string, payload any, want int) {
		t.Helper()
		b, e := json.Marshal(payload)
		must(e)
		resp, e := admin.Post(peers[l].API+path, "application/json", bytes.NewReader(b))
		must(e)
		defer resp.Body.Close()
		b, e = io.ReadAll(resp.Body)
		must(e)
		if resp.StatusCode != want {
			t.Fatalf("%s: %d: %s", path, resp.StatusCode, b)
		}
	}
	for schema := 1; schema <= 4; schema++ {
		post("/v1/realm/schema", controlapi.SchemaRequest{Target: schema, ID: fmt.Sprintf("native-transition-schema-%d", schema), ExpectedRevision: stores[l].Snapshot().Revision}, 200)
	}

	pendingPath := plans[4].StateRoot + ".recovery-pending"
	pending, e := os.ReadFile(pendingPath)
	must(e)
	must(os.WriteFile(pendingPath, []byte("interrupted or mismatched floor"), 0600))
	post("/v1/realm/ha/activate", controlapi.HAActivateRequest{Prepared: p, ExpectedRevision: stores[l].Snapshot().Revision}, 409)
	must(os.WriteFile(pendingPath, pending, 0600))
	post("/v1/realm/ha/activate", controlapi.HAActivateRequest{Prepared: p, ExpectedRevision: stores[l].Snapshot().Revision - 1}, 409)
	post("/v1/realm/ha/activate", controlapi.HAActivateRequest{Prepared: p, ExpectedRevision: stores[l].Snapshot().Revision}, 200)
	after := stores[l].Snapshot()
	if !after.NativeHA || after.HAActivation == nil || after.HAActivation.ID != p.ID {
		t.Fatal("quorum activation receipt missing")
	}
	converge(after.Revision)
	post("/v1/realm/ha/activate", controlapi.HAActivateRequest{Prepared: p, ExpectedRevision: 1}, 200)
	if stores[l].Snapshot().Revision != after.Revision {
		t.Fatal("retry duplicated activation")
	}
	for _, v := range voters {
		must(v.Close())
	}
	for i := range voters {
		voters[i] = nil
	}
	must(actor.Close())
	if hatransition.RollbackStaging(plans[4].StateRoot, p.ID, strings.Repeat("old cache cannot prove quorum did not activate; ", 3)) == nil {
		t.Fatal("non-voter activation floor bypassed")
	}
	if hatransition.RollbackStaging(plans[0].StateRoot, p.ID, strings.Repeat("old config requested after activation; ", 3)) == nil {
		t.Fatal("committed HA downgraded by config rollback")
	}
	// Membership and private identity are retained, and Sources still contain bytes.
	for i, h := range plans {
		b, e := os.ReadFile(filepath.Join(h.ConfigRoot, "pki/node.key"))
		must(e)
		if len(b) == 0 {
			t.Fatal("private key lost")
		}
		if i < 3 {
			path := filepath.Join(h.StateRoot, "realm/consensus/membership.json")
			b, e := os.ReadFile(path)
			must(e)
			if !bytes.Equal(b, original[path]) {
				t.Fatal("consensus membership changed")
			}
		}
		_, e = source.NewManager(h.StateRoot).Identity("app")
		must(e)
	}
}
