package deploy

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/consensus"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/version"
)

// Three real generated voter configurations share this runner's kernel. Only
// bind addresses, filesystem roots and Unix sockets are translated into private
// test locations; roles, CA/leaf keys, membership and network plan are retained.
func TestNativeGuidedInstallation(t *testing.T) {
	if os.Getenv("TITANUS_GUIDED_INSTALL_TEST") != "1" {
		t.Skip("native guided installation fixture required")
	}
	if os.Geteuid() != 0 {
		t.Fatal("disposable native runner root required")
	}
	dir := t.TempDir()
	revisionBytes, e := exec.Command("git", "rev-parse", "HEAD").Output()
	if e != nil {
		t.Fatal(e)
	}
	revision := strings.TrimSpace(string(revisionBytes))
	archives := map[string]model.Archive{}
	for _, arch := range []string{"amd64", "arm64"} {
		file := filepath.Join(os.Getenv("TITANUS_GUIDED_ARCHIVES"), fmt.Sprintf("titanus-%s-linux-%s.tar.gz", version.Version, arch))
		data, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		sum := sha256.Sum256(data)
		archives[arch] = model.Archive{Path: file, SHA256: hex.EncodeToString(sum[:])}
	}
	// Hold all API/Raft sockets until membership/configuration has been written.
	held := []net.Listener{}
	reserve := func(port int) (int, bool) {
		start := len(held)
		for i := 1; i <= 3; i++ {
			l, e := net.Listen("tcp", fmt.Sprintf("127.0.0.%d:%d", i+10, port))
			if e != nil {
				for _, l := range held[start:] {
					l.Close()
				}
				held = held[:start]
				return 0, false
			}
			held = append(held, l)
			port = l.Addr().(*net.TCPAddr).Port
		}
		return port, true
	}
	api, ok := reserve(0)
	if !ok {
		t.Fatal("reserve API")
	}
	raft, ok := reserve(0)
	if !ok {
		t.Fatal("reserve Raft")
	}
	t.Cleanup(func() {
		for _, l := range held {
			l.Close()
		}
	})
	plan := model.RealmPlan{Version: "titanus-plan/v2", RealmName: "GUIDED-LAB", FabricCIDR: "10.210.0.0/16", ServiceCIDR: "10.220.0.0/16", Installation: &model.Installation{Operation: "install", ReleaseVersion: version.Version, Revision: revision, Archives: archives, PKIDir: filepath.Join(dir, "authority"), APIPort: api, RaftPort: raft, NodePrefix: 24, VXLANID: 4242}}
	for i := 1; i <= 3; i++ {
		plan.Nodes = append(plan.Nodes, model.NodeSpec{Name: fmt.Sprintf("control%d", i), ManagementIP: fmt.Sprintf("127.0.0.%d", i+10), SSHUser: "root", SSHPort: 22, Capabilities: []model.Capability{model.CapabilityControl}})
	}
	output := filepath.Join(dir, "prepared")
	prepared, e := PrepareRelease(plan, output)
	if e != nil {
		t.Fatal(e)
	}
	bundleDir := filepath.Join(dir, "bundle")
	os.Mkdir(bundleDir, 0700)
	if data, e := exec.Command("tar", "-xzf", archives[runtime.GOARCH].Path, "-C", bundleDir).CombinedOutput(); e != nil {
		t.Fatalf("extract native fixture: %v %s", e, data)
	}
	bundle := filepath.Join(bundleDir, fmt.Sprintf("titanus-%s-linux-%s", version.Version, runtime.GOARCH))
	type installed struct {
		root, name, socket string
		env                []string
		log                *os.File
	}
	installations := []installed{}
	for index, item := range prepared.Nodes {
		root := filepath.Join(dir, item.Node)
		cfg := filepath.Join(root, "configuration")
		os.MkdirAll(cfg, 0700)
		f, e := os.Open(item.Config)
		if e != nil {
			t.Fatal(e)
		}
		tr := tar.NewReader(f)
		for {
			h, e := tr.Next()
			if e == io.EOF {
				break
			}
			if e != nil {
				t.Fatal(e)
			}
			file := filepath.Join(cfg, h.Name)
			os.MkdirAll(filepath.Dir(file), 0700)
			data, e := io.ReadAll(tr)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(file, data, 0600); e != nil {
				t.Fatal(e)
			}
		}
		f.Close()
		command := exec.Command("python3", filepath.Join(bundle, "scripts/install-release.py"), "--root", root, "--bundle", bundle, "--config", cfg, "--expect-version", version.Version, "--expect-revision", revision)
		if data, e := command.CombinedOutput(); e != nil {
			t.Fatalf("install %s: %v %s", item.Node, e, data)
		}
		state := filepath.Join(root, "var/lib/titanus")
		seed := exec.Command(filepath.Join(root, "usr/local/bin/titanus"), "realm", "seed", "--name", plan.RealmName, "--fabric-cidr", plan.FabricCIDR, "--service-cidr", plan.ServiceCIDR, "--node-prefix", "24", "--vxlan-id", "4242")
		seed.Env = append(os.Environ(), "TITANUS_STATE_ROOT="+state)
		if data, e := seed.CombinedOutput(); e != nil {
			t.Fatalf("seed: %v %s", e, data)
		}
		configData, e := os.ReadFile(filepath.Join(root, "etc/titanus/daemon.env"))
		if e != nil {
			t.Fatal(e)
		}
		env := []string{}
		socket := filepath.Join(dir, item.Node+".sock")
		for _, line := range strings.Split(strings.TrimSpace(string(configData)), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				t.Fatal("invalid environment")
			}
			if strings.HasPrefix(value, "/etc/titanus/") || value == "/var/lib/titanus" {
				value = filepath.Join(root, value)
			}
			if key == "TITANUS_CLUSTER_LISTEN" {
				value = fmt.Sprintf("%s:%d", plan.Nodes[index].ManagementIP, api)
			}
			env = append(env, key+"="+value)
		}
		env = append(env, "TITANUS_SOCKET="+socket)
		var membership consensus.Config
		configData, e = os.ReadFile(filepath.Join(root, "etc/titanus/ha.json"))
		if e != nil {
			t.Fatal(e)
		}
		if e = json.Unmarshal(configData, &membership); e != nil {
			t.Fatal(e)
		}
		if membership.Bootstrap != (index == 0) || len(membership.Peers) != 3 {
			t.Fatal("wrong generated quorum/bootstrap")
		}
		if info, e := os.Stat(filepath.Join(root, "etc/titanus/pki/ca.key")); e != nil || info.Mode().Perm() != 0600 {
			t.Fatal("missing private surviving signer")
		}
		log, e := os.Create(filepath.Join(dir, item.Node+".log"))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { log.Close() })
		installations = append(installations, installed{root: root, name: item.Node, socket: socket, env: env, log: log})
	}
	for _, l := range held {
		l.Close()
	}
	for _, node := range installations {
		cmd := exec.Command(filepath.Join(node.root, "usr/local/sbin/titanusd"))
		cmd.Env = append(os.Environ(), node.env...)
		cmd.Stdout = node.log
		cmd.Stderr = node.log
		if e = cmd.Start(); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	}
	tls, e := identity.TLSConfig(filepath.Join(output, "operator/ca.crt"), filepath.Join(output, "operator/admin.crt"), filepath.Join(output, "operator/admin.key"), false)
	if e != nil {
		t.Fatal(e)
	}
	transport := &http.Transport{TLSClientConfig: tls}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	// A two-voter quorum can answer while the third installed daemon is broken.
	// Require every generated controller to serve authenticated local consensus
	// status and agree on one leader before accepting the installation fixture.
	controllersReady := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		leaderAPI := ""
		leaders, followers := 0, 0
		ready := true
		for _, node := range plan.Nodes {
			response, e := client.Get(fmt.Sprintf("https://%s:%d/v1/realm/consensus", node.ManagementIP, api))
			if e != nil {
				ready = false
				break
			}
			var status map[string]string
			e = json.NewDecoder(response.Body).Decode(&status)
			response.Body.Close()
			if response.StatusCode != http.StatusOK || e != nil || status["id"] != node.Name || status["realm"] != plan.RealmName || status["leader_api"] == "" {
				ready = false
				break
			}
			if leaderAPI == "" {
				leaderAPI = status["leader_api"]
			}
			if status["leader_api"] != leaderAPI {
				ready = false
				break
			}
			switch status["state"] {
			case "Leader":
				if leaderAPI != fmt.Sprintf("https://%s:%d", node.ManagementIP, api) {
					ready = false
				}
				leaders++
			case "Follower":
				followers++
			default:
				ready = false
			}
		}
		if ready && leaders == 1 && followers == 2 {
			controllersReady = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !controllersReady {
		for _, n := range installations {
			data, _ := os.ReadFile(n.log.Name())
			t.Logf("%s: %s", n.name, data)
		}
		t.Fatal("all three generated controllers must serve mTLS consensus with one common leader and two followers")
	}
	t.Log("TITANUS_NATIVE_GUIDED_THREE_CONTROLLERS_READY")
	var recovered realm.State
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		for _, node := range plan.Nodes {
			response, e := client.Get(fmt.Sprintf("https://%s:%d/v1/realm/state", node.ManagementIP, api))
			if e != nil {
				continue
			}
			data, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode == 200 && json.Unmarshal(data, &recovered) == nil && recovered.Name == plan.RealmName && recovered.PKI.CA != "" {
				break
			}
		}
		if recovered.PKI.CA != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if recovered.PKI.CA == "" || recovered.Network.FabricCIDR != plan.FabricCIDR || recovered.Network.ServiceCIDR != plan.ServiceCIDR {
		for _, n := range installations {
			data, _ := os.ReadFile(n.log.Name())
			t.Logf("%s: %s", n.name, data)
		}
		t.Fatal("generated membership, TLS/admin identity and seeded network did not reconstruct functional quorum state")
	}
	t.Log("TITANUS_NATIVE_GUIDED_QUORUM_INSTALL_OK")
}
