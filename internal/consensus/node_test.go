package consensus

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
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

	"github.com/antonismor/Titanus-Core/internal/controlapi"
	"github.com/antonismor/Titanus-Core/internal/controllerclient"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"github.com/hashicorp/raft"
)

type cluster struct {
	t                  *testing.T
	authority          *identity.Authority
	nodes              [3]*Node
	stores             [3]*realm.Store
	configs            [3]Config
	roots, certs, keys [3]string
	apis               [3]*httptest.Server
	seeds              [3]realm.State
}

// Keep each Raft endpoint bound while selecting the complete membership.
// Closing a :0 listener immediately lets the kernel return the same port for
// another peer/API, making a valid security fixture fail membership validation.
func reserveRaftAddress(t *testing.T) net.Listener {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}
func newCluster(t *testing.T) *cluster {
	t.Helper()
	a, e := identity.InitAuthority(t.TempDir(), "LAB")
	if e != nil {
		t.Fatal(e)
	}
	c := &cluster{t: t, authority: &a}
	peers := []Peer{}
	var reservations [3]net.Listener
	for i := 0; i < 3; i++ {
		c.roots[i] = t.TempDir()
		c.apis[i] = httptest.NewUnstartedServer(nil)
		id := fmt.Sprintf("c%d", i)
		c.certs[i], c.keys[i], e = a.Issue(id, []string{"127.0.0.1"}, identity.RoleController, time.Hour)
		if e != nil {
			t.Fatal(e)
		}
		reservations[i] = reserveRaftAddress(t)
		peers = append(peers, Peer{ID: id, Address: reservations[i].Addr().String(), API: "https://" + c.apis[i].Listener.Addr().String()})
		s, e := realm.Open(c.roots[i], "LAB")
		if e != nil {
			t.Fatal(e)
		}
		if e = s.ConfigureNetwork(realm.RealmNetwork{FabricCIDR: "10.240.0.0/16", ServiceCIDR: "10.250.0.0/16", NodePrefix: 24, VXLANID: 4242}); e != nil {
			t.Fatal(e)
		}
		c.seeds[i] = s.Snapshot()
	}
	t.Cleanup(func() {
		for i := 0; i < 3; i++ {
			if c.apis[i] != nil {
				c.apis[i].Close()
			}
			if c.nodes[i] != nil {
				_ = c.nodes[i].Close()
			}
		}
	})
	for i := 0; i < 3; i++ {
		c.configs[i] = Config{ID: peers[i].ID, Realm: "LAB", Peers: peers, Bootstrap: i == 0}
		// Release only this node's reserved socket immediately before Open;
		// all later peers and all API listeners remain bound throughout.
		if err := reservations[i].Close(); err != nil {
			t.Fatal(err)
		}
		c.open(i)
		mux := http.NewServeMux()
		api := controlapi.New(c.stores[i], nil, nil, nil)
		api.Consensus = c.nodes[i]
		api.Register(mux)
		c.apis[i].Config.Handler = identity.Authenticate(mux, a.CertPath)
		cfg, e := identity.TLSConfig(a.CertPath, c.certs[i], c.keys[i], true)
		if e != nil {
			t.Fatal(e)
		}
		pair, e := tls.LoadX509KeyPair(c.certs[i], c.keys[i])
		if e != nil {
			t.Fatal(e)
		}
		cfg.Certificates = []tls.Certificate{pair}
		c.apis[i].TLS = cfg
		c.apis[i].StartTLS()
	}
	_ = c.leader(-1)
	return c
}
func (c *cluster) open(i int) {
	c.t.Helper()
	n, e := Open(c.roots[i], c.configs[i], c.seeds[i], c.authority.CertPath, c.certs[i], c.keys[i])
	if e != nil {
		c.t.Fatal(e)
	}
	s, e := realm.Open(c.roots[i], "LAB")
	if e != nil {
		c.t.Fatal(e)
	}
	if e = s.EnableConsensus(n); e != nil {
		c.t.Fatal(e)
	}
	c.configs[i] = n.cfg
	c.nodes[i] = n
	c.stores[i] = s
}
func (c *cluster) leader(exclude int) int {
	c.t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		for i, n := range c.nodes {
			if i != exclude && n != nil && n.raft.State() == raft.Leader && n.Initialize() == nil && n.CheckLeader() == nil {
				return i
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	for i, n := range c.nodes {
		if n != nil {
			c.t.Log(i, n.Status())
		}
	}
	c.t.Fatal("no quorum leader")
	return -1
}
func (c *cluster) converge(revision uint64) {
	c.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		all := true
		for _, n := range c.nodes {
			if n != nil && n.Snapshot().Revision != revision {
				all = false
			}
		}
		if all {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatal("committed state did not converge")
}
func testFleet(name string) realm.Fleet {
	return realm.Fleet{Name: name, Instances: 1, MinimumAvailable: 1, Template: realm.UnitTemplate{Source: "app", Command: []string{"v1"}}}
}

// Uses actual loopback TLS transport and on-disk Raft logs, not mocked quorum.
func TestTLSQuorumLeaderLossPartitionRejoinAndRestart(t *testing.T) {
	c := newCluster(t)
	leader := c.leader(-1)
	if e := c.stores[leader].PutFleet(testFleet("web")); e != nil {
		t.Fatal(e)
	}
	c.converge(c.nodes[leader].Snapshot().Revision)
	follower := (leader + 1) % 3
	before := c.nodes[follower].Snapshot().Revision
	if e := c.stores[follower].PutFleet(testFleet("forbidden")); !errors.Is(e, ErrUnavailable) {
		t.Fatalf("follower accepted write: %v", e)
	}
	if _, ok := c.stores[follower].Snapshot().Fleets["forbidden"]; ok || c.nodes[follower].Snapshot().Revision != before {
		t.Fatal("follower leaked rejected candidate")
	}
	stale := c.nodes[leader].Snapshot()
	stale.Revision--
	if e := c.nodes[leader].Apply(stale); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale candidate accepted: %v", e)
	}

	// Isolate the live leader by closing its transport. The two remaining voters
	// retain their disks, elect a new leader and keep accepting committed writes.
	if e := c.nodes[leader].transport.Close(); e != nil {
		t.Fatal(e)
	}
	next := c.leader(leader)
	if e := c.nodes[leader].CheckLeader(); e == nil {
		t.Fatal("isolated leader retained authority")
	}
	if e := c.stores[leader].PutFleet(testFleet("minority")); e == nil {
		t.Fatal("minority write succeeded")
	}
	if _, ok := c.nodes[leader].Snapshot().Fleets["minority"]; ok {
		t.Fatal("minority published uncommitted state")
	}
	fleet := testFleet("web")
	fleet.Template.Command = []string{"v2"}
	if e := c.stores[next].PutFleet(fleet); e != nil {
		t.Fatal(e)
	}
	committed := c.nodes[next].Snapshot()
	if committed.Fleets["web"].Generation != 2 || len(committed.Fleets["web"].History) != 1 {
		t.Fatal("rollout history lost")
	}

	// Rejoin using the original log and membership, then prove it catches up.
	_ = c.nodes[leader].Close()
	c.nodes[leader] = nil
	c.open(leader)
	c.converge(committed.Revision)
	if _, e := c.stores[next].RollbackFleet("web", 1); e != nil {
		t.Fatal(e)
	}
	c.converge(c.nodes[next].Snapshot().Revision)

	// Stop two voters: no remaining controller may acknowledge a write.
	one := next
	for i := 0; i < 3; i++ {
		if i != one {
			_ = c.nodes[i].Close()
			c.nodes[i] = nil
		}
	}
	if e := c.stores[one].PutFleet(testFleet("no-quorum")); e == nil {
		t.Fatal("single voter acknowledged a write")
	}
	if _, ok := c.nodes[one].Snapshot().Fleets["no-quorum"]; ok {
		t.Fatal("minority commit visible")
	}
	_ = c.nodes[one].Close()
	c.nodes[one] = nil

	// Full restart replays committed log state, not legacy state.json. An
	// unacknowledged write is allowed to be committed later; never assert absence
	// after quorum returns. Acked history/rollback must always survive.
	for i := 0; i < 3; i++ {
		c.open(i)
	}
	recovered := c.leader(-1)
	state := c.nodes[recovered].Snapshot()
	web := state.Fleets["web"]
	if web.Generation != 3 || web.Template.Command[0] != "v1" || len(web.History) != 2 {
		t.Fatal("acknowledged rollback did not survive full restart")
	}
	c.converge(state.Revision)
	// Once managed, direct standalone writes/downgrades fail closed.
	local, e := realm.Open(c.roots[recovered], "LAB")
	if e != nil {
		t.Fatal(e)
	}
	if e = local.PutFleet(testFleet("bypass")); e == nil {
		t.Fatal("standalone CLI bypassed HA")
	}
}

func TestAPILeaderGateAndTrustedClientFailover(t *testing.T) {
	c := newCluster(t)
	leader := c.leader(-1)
	follower := (leader + 1) % 3
	// Admin request uses real mTLS; node role must remain denied before leader
	// discovery, and a server-supplied URL is never a redirect authority.
	cert, key, e := c.authority.Issue("operator", []string{"127.0.0.1"}, identity.RoleAdmin, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	cfg, e := identity.TLSConfig(c.authority.CertPath, cert, key, false)
	if e != nil {
		t.Fatal(e)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 15 * time.Second}
	resp, e := client.Post(c.configs[follower].Peers[follower].API+"/v1/realm/fleets", "application/json", strings.NewReader(`{"name":"denied"}`))
	if e != nil {
		t.Fatal(e)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 503 || resp.Header.Get("X-Titanus-Rejected") != "true" || resp.Header.Get("X-Titanus-Leader") != c.nodes[leader].LeaderAPI() {
		t.Fatal("follower did not reject before execution with leader hint")
	}
	origins := []string{c.configs[0].Peers[follower].API, c.configs[0].Peers[leader].API, c.configs[0].Peers[(follower+1)%3].API}
	failover, e := controllerclient.New(client.Transport, strings.Join(origins, ","))
	if e != nil {
		t.Fatal(e)
	}
	client.Transport = failover
	payload, _ := json.Marshal(testFleet("through-failover"))
	resp, e = client.Post(origins[0]+"/v1/realm/fleets", "application/json", bytes.NewReader(payload))
	if e != nil {
		t.Fatal(e)
	}
	data, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("failover did not reach leader: %d %s", resp.StatusCode, data)
	}
	if c.nodes[leader].Snapshot().Fleets["through-failover"].Generation != 1 {
		t.Fatal("failover replayed successful fleet write")
	}
}

func TestSnapshotsCatchUpLaggingVoter(t *testing.T) {
	c := newCluster(t)
	leader := c.leader(-1)
	offline := (leader + 1) % 3
	_ = c.nodes[offline].Close()
	c.nodes[offline] = nil
	for i := 0; i < 100; i++ {
		f := testFleet("app")
		f.Template.Command = []string{fmt.Sprintf("v%d", i)}
		if e := c.stores[leader].PutFleet(f); e != nil {
			t.Fatal(e)
		}
	}
	if e := c.nodes[leader].raft.Snapshot().Error(); e != nil {
		t.Fatal(e)
	}
	revision := c.nodes[leader].Snapshot().Revision
	c.open(offline)
	c.converge(revision)
	if c.nodes[offline].Snapshot().Fleets["app"].Generation != 100 {
		t.Fatal("snapshot lost Fleet state")
	}
	// Snapshot plus replay also survives restarting all voters.
	for i := 0; i < 3; i++ {
		_ = c.nodes[i].Close()
		c.nodes[i] = nil
	}
	for i := 0; i < 3; i++ {
		c.open(i)
	}
	leader = c.leader(-1)
	c.converge(c.nodes[leader].Snapshot().Revision)
	if c.nodes[leader].Snapshot().Fleets["app"].Generation != 100 {
		t.Fatal("snapshot replay lost state")
	}
}

func TestMembershipBindingAndOversizedCandidate(t *testing.T) {
	c := newCluster(t)
	leader := c.leader(-1)
	config := c.configs[leader]
	config.Peers = append([]Peer(nil), config.Peers...)
	config.Peers[0].API = "https://127.0.0.1:12345"
	if _, e := Open(c.roots[leader], config, c.seeds[leader], c.authority.CertPath, c.certs[leader], c.keys[leader]); e == nil {
		t.Fatal("changed persistent membership accepted")
	}
	candidate := c.nodes[leader].Snapshot()
	candidate.Revision++
	candidate.Fleets["huge"] = testFleet("huge")
	f := candidate.Fleets["huge"]
	f.Template.Environment = []string{strings.Repeat("x", MaxStateBytes)}
	candidate.Fleets["huge"] = f
	if e := c.nodes[leader].Apply(candidate); e == nil {
		t.Fatal("oversized consensus entry accepted")
	}
	if _, ok := c.nodes[leader].Snapshot().Fleets["huge"]; ok {
		t.Fatal("oversized candidate published")
	}
	if e := os.WriteFile(filepath.Join(c.roots[leader], "realm", "consensus", "membership.json"), []byte("garbage"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := Open(c.roots[leader], c.configs[leader], c.seeds[leader], c.authority.CertPath, c.certs[leader], c.keys[leader]); e == nil {
		t.Fatal("corrupt membership accepted")
	}
}

func TestRaftCertificateRoleAndIdentity(t *testing.T) {
	c := newCluster(t)
	target := c.configs[0].Peers[0].Address
	for _, tc := range []struct {
		id   string
		role identity.Role
	}{{"worker", identity.RoleNode}, {"operator", identity.RoleAdmin}, {"foreign-controller", identity.RoleController}} {
		cert, key, e := c.authority.Issue(tc.id, []string{"127.0.0.1"}, tc.role, time.Hour)
		if e != nil {
			t.Fatal(e)
		}
		cfg, e := identity.TLSConfig(c.authority.CertPath, cert, key, false)
		if e != nil {
			t.Fatal(e)
		}
		binding, _ := json.Marshal(struct {
			Realm string
			Peers []Peer
			Seed  string
		}{c.configs[0].Realm, c.configs[0].Peers, c.configs[0].SeedHash})
		cfg.NextProtos = []string{fmt.Sprintf("titanus-raft-%x", sha256.Sum256(binding))}
		conn, e := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", target, cfg)
		if e != nil {
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = conn.Write([]byte{0})
		one := make([]byte, 1)
		_, e = conn.Read(one)
		_ = conn.Close()
		if e == nil {
			t.Fatalf("unauthorized Raft identity %s was not disconnected", tc.id)
		}
	}
	// An otherwise valid controller certificate cannot claim another configured
	// controller's ID at startup or when dialing its endpoint.
	if _, e := newTLSStream(c.configs[1], c.authority.CertPath, c.certs[0], c.keys[0]); e == nil {
		t.Fatal("wrong local controller identity accepted")
	}
}

func TestRevokedControllerLosesExistingStreamAuthority(t *testing.T) {
	c := newCluster(t)
	leader := c.leader(-1)
	pair, e := tls.LoadX509KeyPair(c.certs[leader], c.keys[leader])
	if e != nil {
		t.Fatal(e)
	}
	leaf, e := x509.ParseCertificate(pair.Certificate[0])
	if e != nil {
		t.Fatal(e)
	}
	if _, e = c.authority.Revoke(leaf.SerialNumber); e != nil {
		t.Fatal(e)
	}
	next := c.leader(leader)
	if e = c.nodes[leader].CheckLeader(); e == nil {
		t.Fatal("revoked controller retained quorum authority on old streams")
	}
	if e = c.stores[next].PutFleet(testFleet("after-revocation")); e != nil {
		t.Fatal(e)
	}
	// A changed seed cannot rejoin the original persisted identity.
	seed := clone(c.seeds[next])
	seed.Network.VXLANID++
	if _, e = Open(c.roots[next], c.configs[next], seed, c.authority.CertPath, c.certs[next], c.keys[next]); e == nil {
		t.Fatal("changed initialization seed accepted")
	}
}

func TestOrchestrationStateReplicatesWithoutPlaintextAndSurvivesLeaderLoss(t *testing.T) {
	c := newCluster(t)
	leader := c.leader(-1)
	store := c.stores[leader]
	keyring := &secrets.Keyring{Active: "one", Keys: map[string][]byte{"one": bytes.Repeat([]byte{1}, 32)}}
	record, e := keyring.Encrypt("LAB", "db", 1, []byte("QUORUM_SECRET_CANARY"))
	if e != nil {
		t.Fatal(e)
	}
	if e = store.PutSecret(record); e != nil {
		t.Fatal(e)
	}
	f := testFleet("web")
	f.Template.CPUPercent = 50
	f.Template.Secrets = []secrets.Ref{{Name: "db", Version: 1, Environment: "PASSWORD"}}
	if e = store.PutFleet(f); e != nil {
		t.Fatal(e)
	}
	if e = store.PutAutoscaler(realm.Autoscaler{Fleet: "web", Min: 1, Max: 5, TargetCPU: 60, CooldownSeconds: 10, DownscaleSeconds: 30}); e != nil {
		t.Fatal(e)
	}
	if e = store.CreateTask(realm.Task{Name: "one", Template: f.Template}); e != nil {
		t.Fatal(e)
	}
	task := store.Snapshot().Tasks["one"]
	task.Phase = realm.TaskDispatched
	task.NodeID = "n1"
	task.LeaseToken = "private-token"
	if e = store.UpdateTask(task); e != nil {
		t.Fatal(e)
	}
	c.converge(store.Snapshot().Revision)
	before := c.stores[(leader+1)%3].Snapshot()
	if e = c.stores[(leader+1)%3].CreateTask(realm.Task{Name: "forbidden", Template: f.Template}); e == nil {
		t.Fatal("follower accepted Task")
	}
	if _, ok := c.stores[(leader+1)%3].Snapshot().Tasks["forbidden"]; ok {
		t.Fatal("uncommitted Task leaked")
	}
	raw, _ := json.Marshal(before)
	if bytes.Contains(raw, []byte("QUORUM_SECRET_CANARY")) {
		t.Fatal("plaintext replicated")
	}
	c.nodes[leader].transport.Close()
	next := c.leader(leader)
	state := c.stores[next].Snapshot()
	if state.Tasks["one"].Phase != realm.TaskDispatched || state.Autoscalers["web"].TargetCPU != 60 {
		t.Fatal("orchestration state lost on failover")
	}
	plain, e := keyring.Decrypt(state.Secrets["db"][0])
	if e != nil || string(plain) != "QUORUM_SECRET_CANARY" {
		t.Fatal("pinned ciphertext lost")
	}
	_ = c.nodes[leader].Close()
	c.nodes[leader] = nil
	c.open(leader)
	c.converge(state.Revision)
	// Force a persisted FSM snapshot, close every voter and restore all of them.
	for i := range c.nodes {
		if e = c.nodes[i].raft.Snapshot().Error(); e != nil && !errors.Is(e, raft.ErrNothingNewToSnapshot) {
			t.Fatal(e)
		}
		c.nodes[i].Close()
		c.nodes[i] = nil
	}
	for i := range c.nodes {
		c.open(i)
	}
	next = c.leader(-1)
	c.converge(c.nodes[next].Snapshot().Revision)
	if c.nodes[next].Snapshot().Tasks["one"].Phase != realm.TaskDispatched {
		t.Fatal("snapshot replay changed dispatch")
	}
}
