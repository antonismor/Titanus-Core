package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/observe"
	"github.com/antonismor/Titanus-Core/internal/secrets"
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
	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/version"
)

// Native CI runs this against the built production daemon on AMD64 and ARM64.
// It kills real processes, rather than gracefully closing Raft fixture objects.
func TestNativeHADaemonCrash(t *testing.T)    { runNativeHADaemon(t, false) }
func TestNativeSchemaTransition(t *testing.T) { runNativeHADaemon(t, true) }
func runNativeHADaemon(t *testing.T, transition bool) {
	mode := "TITANUS_HA_DAEMON_TEST"
	if transition {
		mode = "TITANUS_SCHEMA_NATIVE_TEST"
	}
	if os.Getenv(mode) != "1" {
		t.Skip("requires explicit native daemon integration mode")
	}
	if os.Geteuid() != 0 {
		t.Fatal("native daemon integration requires root")
	}
	binary := os.Getenv("TITANUS_DAEMON_BINARY")
	if !filepath.IsAbs(binary) {
		t.Fatal("absolute daemon binary required")
	}
	binaries := [3]string{binary, binary, binary}
	if transition {
		old := os.Getenv("TITANUS_SCHEMA_OLD_BINARY")
		if !filepath.IsAbs(old) {
			t.Fatal("pinned legacy daemon binary required")
		}
		binaries = [3]string{old, old, old}
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
		if e := durable.WriteJSON(filepath.Join(roots[i], "private-keys.json"), secrets.Keyring{Active: "one", Keys: map[string][]byte{"one": bytes.Repeat([]byte{1}, 32), "two": bytes.Repeat([]byte{2}, 32)}}, 0600); e != nil {
			t.Fatal(e)
		}
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
		cmd := exec.Command(binaries[i])
		// Replace inherited task configuration so the fixture cannot address a real
		// Realm, a host Unix socket or user cgroups. No Units are created in this test.
		env := []string{}
		for _, value := range os.Environ() {
			if !strings.HasPrefix(value, "TITANUS_") {
				env = append(env, value)
			}
		}
		env = append(env, "TITANUS_SECRET_KEYRING="+filepath.Join(roots[i], "private-keys.json"), "TITANUS_STATE_ROOT="+roots[i], "TITANUS_SOCKET="+sockets[i], "TITANUS_REALM_NAME=HA-CI", "TITANUS_NODE_ID="+peers[i].ID, "TITANUS_CONTROLLER_MODE=true", "TITANUS_GATEWAY_MODE=false", "TITANUS_HA_CONFIG="+configs[i], "TITANUS_CLUSTER_LISTEN="+strings.TrimPrefix(peers[i].API, "https://"), "TITANUS_CA="+filepath.Join(pkis[i], "ca.crt"), "TITANUS_CERT="+certs[i], "TITANUS_KEY="+keys[i], "TITANUS_CGROUP_ROOT="+filepath.Join(roots[i], "unused-cgroups"))
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

	if transition {
		migration := "native-schema-transition-001"
		for i := 0; i < 3; i++ {
			stop(i)
			binaries[i] = binary
			start(i)
			current = leader(-1)
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				info, e := clients[i].Compatibility()
				if e == nil && info.Capabilities.Supports(1) {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			state, e := clients[current].RealmState()
			if e != nil {
				t.Fatal(e)
			}
			if state.SchemaVersion != 0 {
				t.Fatal("binary upgrade migrated data automatically")
			}
			if i < 2 {
				// Target migration must be denied while even one original voter runs the
				// actual legacy binary. Writes remain possible under the legacy schema.
				if _, e = clients[i].TransitionSchema(migration, state.Revision); e == nil {
					t.Fatal("migration accepted legacy voter")
				}
				if _, e = clients[current].CreateFleet(realm.Fleet{Name: fmt.Sprintf("mixed-%d", i), Instances: 0, Template: realm.UnitTemplate{Source: "app", Command: []string{"v1"}}}); e != nil {
					t.Fatal("mixed-version quorum write", e)
				}
			}
		}
		current = leader(-1)
		// Simulate an interrupted preparation on one host. Restart preserves the
		// prepared floor but does not manufacture a committed migration.
		partial := (current + 1) % 3
		if e := offline.PrepareSchemaFloor(roots[partial], offline.NewSchemaFloor("HA-CI", migration, 1)); e != nil {
			t.Fatal(e)
		}
		stop(partial)
		start(partial)
		current = leader(-1)
		before, e := clients[current].RealmState()
		if e != nil {
			t.Fatal(e)
		}
		if before.SchemaVersion != 0 {
			t.Fatal("interrupted preparation altered quorum schema")
		}
		// Unreachable original voter rejects full prepare without committing.
		stop(partial)
		if _, e = clients[current].TransitionSchema(migration, before.Revision); e == nil {
			t.Fatal("migration accepted absent original voter")
		}
		after, e := clients[current].RealmState()
		if e != nil || after.SchemaVersion != 0 {
			t.Fatal("failed prepare committed schema", e)
		}
		start(partial)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, e = clients[partial].Compatibility(); e == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		current = leader(-1)
		before, e = clients[current].RealmState()
		if e != nil {
			t.Fatal(e)
		}
		committed, e := clients[current].TransitionSchema(migration, before.Revision)
		if e != nil {
			t.Fatal("prepared quorum migration", e)
		}
		state, e := clients[current].RealmState()
		if e != nil || state.SchemaVersion != 1 {
			t.Fatal("migration not committed", e)
		}
		if len(state.Fleets) != 2 {
			t.Fatal("mixed-version state lost")
		}
		// Lost-response retry returns the same identity/revision, even with a stale
		// expected revision, without proposing another schema change.
		again, e := clients[current].TransitionSchema(migration, before.Revision)
		if e != nil || again.ID != committed.ID || again.Revision != committed.Revision {
			t.Fatal("migration replay not idempotent", e)
		}
		stop(current)
		current = leader(current)
		state, e = clients[current].RealmState()
		if e != nil || state.SchemaVersion != 1 {
			t.Fatal("schema lost after controller crash", e)
		}
		for i := 0; i < 3; i++ {
			stop(i)
		}
		for i := 0; i < 3; i++ {
			if e := offline.CheckSchemaFloor(roots[i]); e != nil {
				t.Fatal("floor missing on voter", e)
			}
			binaries[i] = os.Getenv("TITANUS_SCHEMA_OLD_BINARY")
			start(i)
			proc := processes[i]
			if e := proc.Wait(); e == nil {
				t.Fatal("legacy daemon accepted migrated data")
			}
			processes[i] = nil
			logdata, _ := os.ReadFile(logs[i])
			if !strings.Contains(string(logdata), "unfinished offline maintenance") {
				t.Fatal("old binary refused for an unrelated reason")
			}
			binaries[i] = binary
			start(i)
		}
		current = leader(-1)
		state, e = clients[current].RealmState()
		if e != nil || state.SchemaVersion != 1 {
			t.Fatal("migrated quorum did not restart", e)
		}
		if _, e = clients[current].CreateFleet(realm.Fleet{Name: "schema1-proof", Instances: 0, Template: realm.UnitTemplate{Source: "app", Command: []string{"v1"}}}); e != nil {
			t.Fatal("migrated quorum write", e)
		}

		if version.MaxSchema >= 2 {
			state, e = clients[current].RealmState()
			if e != nil {
				t.Fatal(e)
			}
			if _, e = clients[current].TransitionSchemaTo("native-schema-two-proof-001", state.Revision, 2); e != nil {
				t.Fatal("schema-two quorum transition", e)
			}
			if _, e = clients[current].Orchestration(http.MethodPut, "/v1/realm/secrets/rotation-proof", map[string][]byte{"value": []byte("NATIVE_ROTATED_VALUE")}); e != nil {
				t.Fatal(e)
			}
			for i := range roots {
				if e = secrets.Provision(filepath.Join(roots[i], "private-keys.json"), "two", nil, true); e != nil {
					t.Fatal(e)
				}
			}
			state, e = clients[current].RealmState()
			if e != nil {
				t.Fatal(e)
			}
			rotation := map[string]any{"id": "native-rotation-proof-001", "target": "two", "expected_revision": state.Revision}
			raw, e := clients[current].Orchestration(http.MethodPost, "/v1/realm/secret-rotation", rotation)
			if e != nil {
				t.Fatal("native readiness-gated rotation", e)
			}
			var marker realm.SecretRotation
			if e = json.Unmarshal(raw, &marker); e != nil {
				t.Fatal(e)
			}
			schedule := realm.TaskSchedule{Name: "future-proof", Template: realm.UnitTemplate{Source: "app", Command: []string{"/bin/sh"}}, DueAt: time.Now().Add(time.Hour), MaxRuns: 1, MaxAttempts: 2, RetrySeconds: 1}
			if _, e = clients[current].Orchestration(http.MethodPost, "/v1/realm/task-schedules", schedule); e != nil {
				t.Fatal(e)
			}
			stop(current)
			current = leader(current)
			raw, e = clients[current].Orchestration(http.MethodPost, "/v1/realm/secret-rotation", rotation)
			if e != nil {
				t.Fatal("rotation receipt lost after leader crash", e)
			}
			var replay realm.SecretRotation
			json.Unmarshal(raw, &replay)
			if replay != marker {
				t.Fatal("rotation replayed after crash")
			}
			deadline := time.Now().Add(35 * time.Second)
			for {
				raw, e = clients[current].Orchestration(http.MethodGet, "/v1/realm/alerts", nil)
				var status observe.CollectionStatus
				if e == nil && json.Unmarshal(raw, &status) == nil && status.Collected == 2 && status.Expected == 3 {
					break
				}
				// Collection can legitimately report 503 for a dead original host;
				// verify persisted bounded evidence directly in this native fixture.
				central, e := observe.NewCentral(roots[current])
				if e == nil {
					status, e = central.Status()
					if e == nil && status.Collected == 2 && status.Expected == 3 {
						break
					}
				}
				if time.Now().After(deadline) {
					t.Fatal("central collection hid failed original controller", e)
				}
				time.Sleep(100 * time.Millisecond)
			}
			for i := range roots {
				stop(i)
			}
			schemaOne := os.Getenv("TITANUS_SCHEMA_ONE_BINARY")
			if schemaOne == "" {
				t.Fatal("actual schema-one legacy binary required")
			}
			for i := range roots {
				binaries[i] = schemaOne
				start(i)
				proc := processes[i]
				if e := proc.Wait(); e == nil {
					t.Fatal("schema-one binary admitted schema two")
				}
				processes[i] = nil
				logdata, e := os.ReadFile(logs[i])
				if e != nil || !strings.Contains(string(logdata), "unsupported schema rollback floor") {
					t.Fatal("schema-one binary refused for an unrelated reason", e, string(logdata))
				}
				binaries[i] = binary
				start(i)
			}
			current = leader(-1)
			state, e = clients[current].RealmState()
			if e != nil || state.SchemaVersion != 2 || state.TaskSchedules[schedule.Name].Name != schedule.Name || len(state.SecretRotations) != 1 {
				t.Fatal("M6 state lost after full process restart", e)
			}
			keys, e := secrets.Load(filepath.Join(roots[current], "private-keys.json"))
			if e != nil {
				t.Fatal(e)
			}
			plain, e := keys.Decrypt(state.Secrets["rotation-proof"][0])
			if e != nil || string(plain) != "NATIVE_ROTATED_VALUE" {
				t.Fatal("rotated ciphertext not functional", e)
			}
			clear(plain)
			t.Log("TITANUS_NATIVE_M6_SCHEMA_ROTATION_COLLECTION_RECOVERY_OK", version.Info().Arch)
		}
		t.Log("TITANUS_NATIVE_MIXED_VERSION_SCHEMA_TRANSITION_OK", version.Info().Arch, "old="+os.Getenv("TITANUS_SCHEMA_OLD_REVISION"), "new="+version.Info().Revision)
		return
	}
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
