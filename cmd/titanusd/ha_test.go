package main

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/consensus"
	"github.com/antonismor/Titanus-Core/internal/controllerclient"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/localclient"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/source"
)

// Native CI runs this against the built production daemon on AMD64 and ARM64.
// It kills real processes, rather than gracefully closing Raft fixture objects.
func TestNativeHADaemonCrash(t *testing.T) {
	if os.Getenv("TITANUS_HA_DAEMON_TEST") != "1" {
		t.Skip("requires explicit native daemon integration mode")
	}
	if os.Geteuid() != 0 {
		t.Fatal("native daemon integration requires root")
	}
	binary := os.Getenv("TITANUS_DAEMON_BINARY")
	if !filepath.IsAbs(binary) {
		t.Fatal("absolute daemon binary required")
	}
	ca, err := identity.InitAuthority(t.TempDir(), "HA-CI")
	if err != nil {
		t.Fatal(err)
	}
	address := func() string {
		t.Helper()
		l, e := net.Listen("tcp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		a := l.Addr().String()
		_ = l.Close()
		return a
	}
	peers := []consensus.Peer{}
	roots, sockets, configs, certs, keys, pkis, logs := [3]string{}, [3]string{}, [3]string{}, [3]string{}, [3]string{}, [3]string{}, [3]string{}
	processes := [3]*exec.Cmd{}
	write := func(path string, data []byte, mode os.FileMode) {
		t.Helper()
		if e := os.WriteFile(path, data, mode); e != nil {
			t.Fatal(e)
		}
	}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("control-%d", i)
		roots[i] = t.TempDir()
		sockets[i] = filepath.Join(roots[i], "daemon.sock")
		configs[i] = filepath.Join(roots[i], "ha.json")
		logs[i] = filepath.Join(roots[i], "daemon.log")
		pkis[i] = filepath.Join(roots[i], "pki")
		if e := os.Mkdir(pkis[i], 0700); e != nil {
			t.Fatal(e)
		}
		certs[i], keys[i], err = ca.Issue(id, []string{"127.0.0.1"}, identity.RoleController, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"ca.crt", "ca.crl"} {
			b, e := os.ReadFile(filepath.Join(ca.Dir, name))
			if e != nil {
				t.Fatal(e)
			}
			write(filepath.Join(pkis[i], name), b, 0600)
		}
		{
			b, e := os.ReadFile(ca.KeyPath)
			if e != nil {
				t.Fatal(e)
			}
			write(filepath.Join(pkis[i], "ca.key"), b, 0600)
		}
		peers = append(peers, consensus.Peer{ID: id, Address: address(), API: "https://" + address()})
		store, e := realm.Open(roots[i], "HA-CI")
		if e != nil {
			t.Fatal(e)
		}
		if e = store.ConfigureNetwork(realm.RealmNetwork{FabricCIDR: "10.241.0.0/16", ServiceCIDR: "10.251.0.0/16", NodePrefix: 24, VXLANID: 4242}); e != nil {
			t.Fatal(e)
		}
	}
	stop := func(i int) {
		if processes[i] != nil {
			_ = processes[i].Process.Kill()
			_ = processes[i].Wait()
			processes[i] = nil
		}
	}
	t.Cleanup(func() {
		for i := 0; i < 3; i++ {
			stop(i)
		}
		if t.Failed() {
			for _, path := range logs {
				data, _ := os.ReadFile(path)
				t.Logf("%s:\n%s", path, data)
			}
		}
	})
	for i := 0; i < 3; i++ {
		data, _ := json.Marshal(consensus.Config{ID: peers[i].ID, Realm: "HA-CI", Peers: peers, Bootstrap: i == 0})
		write(configs[i], data, 0600)
	}
	start := func(i int) {
		t.Helper()
		file, e := os.OpenFile(logs[i], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		cmd := exec.Command(binary)
		// Replace inherited task configuration so the fixture cannot address a real
		// Realm, a host Unix socket or user cgroups. No Units are created in this test.
		env := []string{}
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "TITANUS_") {
				env = append(env, value)
			}
		}
		env = append(env, "TITANUS_STATE_ROOT="+roots[i], "TITANUS_SOCKET="+sockets[i], "TITANUS_REALM_NAME=HA-CI", "TITANUS_NODE_ID="+peers[i].ID, "TITANUS_CONTROLLER_MODE=true", "TITANUS_GATEWAY_MODE=false", "TITANUS_HA_CONFIG="+configs[i], "TITANUS_CLUSTER_LISTEN="+strings.TrimPrefix(peers[i].API, "https://"), "TITANUS_CA="+filepath.Join(pkis[i], "ca.crt"), "TITANUS_CERT="+certs[i], "TITANUS_KEY="+keys[i], "TITANUS_CGROUP_ROOT="+filepath.Join(roots[i], "unused-cgroups"))
		cmd.Env = env
		cmd.Stdout = file
		cmd.Stderr = file
		e = cmd.Start()
		_ = file.Close()
		if e != nil {
			t.Fatal(e)
		}
		processes[i] = cmd
	}
	clients := [3]*localclient.Client{}
	for i := 0; i < 3; i++ {
		clients[i] = localclient.New(sockets[i])
		start(i)
	}
	leader := func(exclude int) int {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			for i, c := range clients {
				if processes[i] == nil || i == exclude {
					continue
				}
				status, e := c.ConsensusStatus()
				if e == nil && status["state"] == "Leader" {
					if _, e = c.RealmState(); e == nil {
						return i
					}
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("real daemons did not elect a quorum-ready leader")
		return -1
	}
	current := leader(-1)
	input := t.TempDir()
	write(filepath.Join(input, "app"), []byte("native-source-data"), 0755)
	if e := source.NewManager(roots[current]).ImportDirectory("app", input); e != nil {
		t.Fatal(e)
	}
	ref, e := clients[current].PublishSource("app")
	if e != nil {
		t.Fatal("controller Source publication", e)
	}
	for _, root := range roots {
		digest, e := source.NewManager(root).Identity("app")
		if e != nil || digest != ref.Digest {
			t.Fatal("native controller Source not replicated", e)
		}
	}
	renewCert, renewKey, e := ca.Issue("renew-client", nil, identity.RoleNode, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	revokedCert, _, e := ca.Issue("revoked-client", nil, identity.RoleNode, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	revokedPEM, e := os.ReadFile(revokedCert)
	if e != nil {
		t.Fatal(e)
	}
	block, _ := pem.Decode(revokedPEM)
	revoked, e := x509.ParseCertificate(block.Bytes)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = clients[current].RevokeCertificate(revoked.SerialNumber.Text(16)); e != nil {
		t.Fatal("native quorum revocation", e)
	}
	fleet := realm.Fleet{Name: "crash-proof", Instances: 1, MinimumAvailable: 1, Template: realm.UnitTemplate{Source: "app", Command: []string{"v1"}}}
	if _, e := clients[current].CreateFleet(fleet); e != nil {
		t.Fatal(e)
	}
	// SIGKILL skips daemon Shutdown and Bolt Close. The majority must elect and
	// preserve acknowledged desired state and rollback history from fsynced logs.
	stop(current)
	next := leader(current)
	state, e := clients[next].RealmState()
	if e != nil {
		t.Fatal(e)
	}
	if state.Fleets[fleet.Name].Generation != 1 {
		t.Fatal("acknowledged state lost after SIGKILL")
	}
	if !state.PKI.Revoked(revoked.SerialNumber) {
		t.Fatal("native signer failover lost committed revocation")
	}
	cfg, e := identity.TLSConfig(ca.CertPath, renewCert, renewKey, false)
	if e != nil {
		t.Fatal(e)
	}
	origins := []string{}
	for _, peer := range peers {
		origins = append(origins, peer.API)
	}
	failover, e := controllerclient.New(&http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}, strings.Join(origins, ","))
	if e != nil {
		t.Fatal(e)
	}
	if e = identity.RenewIfNeeded(&http.Client{Transport: failover, Timeout: 20 * time.Second}, peers[current].API, ca.CertPath, renewCert, renewKey); e != nil {
		t.Fatal("native leaf renewal after signer SIGKILL", e)
	}
	state, e = clients[next].RealmState()
	if e != nil || state.PKI.Issued == 0 {
		t.Fatal("renewed certificate returned without issuance commit", e)
	}
	// Recover an absent controller-local payload against its committed digest
	// while one controller remains killed. No fabricated metadata is enough.
	if e = os.RemoveAll(filepath.Join(roots[next], "sources", "app")); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		digest, err := source.NewManager(roots[next]).Identity("app")
		if err == nil && digest == ref.Digest {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native leader failed to restore committed Source", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	fleet.Template.Command = []string{"v2"}
	if _, e = clients[next].CreateFleet(fleet); e != nil {
		t.Fatal(e)
	}
	if _, e = clients[next].RollbackFleet(fleet.Name, 1); e != nil {
		t.Fatal(e)
	}
	start(current)
	// Kill every process and restart its original disk/identity. Successful API
	// writes must survive even without graceful shutdown on any voter.
	for i := 0; i < 3; i++ {
		stop(i)
	}
	for i := 0; i < 3; i++ {
		start(i)
	}
	recovered := leader(-1)
	state, e = clients[recovered].RealmState()
	if e != nil {
		t.Fatal(e)
	}
	f := state.Fleets[fleet.Name]
	if f.Generation != 3 || len(f.History) != 2 || f.Template.Command[0] != "v1" {
		t.Fatalf("rollback lost after full SIGKILL/restart: %+v", f)
	}
	if state.Sources["app"] != ref || !state.PKI.Revoked(revoked.SerialNumber) {
		t.Fatal("HA restart lost Source/PKI state")
	}
	// Isolating the last running voter must reject a write through the production
	// Unix API rather than leak a mutated candidate into local committed state.
	for i := 0; i < 3; i++ {
		if i != recovered {
			stop(i)
		}
	}
	if _, e = clients[recovered].CreateFleet(realm.Fleet{Name: "minority", Instances: 1, Template: fleet.Template}); e == nil {
		t.Fatal("real minority daemon acknowledged a write")
	}
	if _, e = clients[recovered].RevokeCertificate(""); e == nil {
		t.Fatal("minority signer published a CRL")
	}
}
