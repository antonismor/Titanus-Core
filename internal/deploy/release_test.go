package deploy

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/remote"
	"github.com/antonismor/Titanus-Core/internal/version"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/model"
)

func TestPreparedRollbackIsOfflineAndPinned(t *testing.T) {
	plan := model.RealmPlan{Version: "titanus-plan/v2", RealmName: "LAB", FabricCIDR: "10.210.0.0/16", ServiceCIDR: "10.220.0.0/16", Nodes: []model.NodeSpec{{Name: "node", ManagementIP: "192.0.2.1", SSHUser: "root", SSHPort: 22, Capabilities: []model.Capability{model.CapabilityControl}}}, Installation: &model.Installation{Operation: "rollback", ReleaseVersion: "0.4.0-rc.4", Revision: strings.Repeat("a", 40), APIPort: 9443, RaftPort: 9444, NodePrefix: 24, VXLANID: 4242}}
	out := filepath.Join(t.TempDir(), "prepared")
	prepared, e := PrepareRelease(plan, out)
	if e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(prepared.Nodes[0].Script)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(data), "--expect-revision '"+strings.Repeat("a", 40)+"'") || strings.Contains(string(data), "ssh") {
		t.Fatal("offline pinned rollback missing")
	}
	if _, e = PrepareRelease(plan, out); e == nil {
		t.Fatal("private preparation overwritten")
	}
}
func TestConfigurationInventoryPinsEverySecretByte(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.tar")
	cfg := map[string][]byte{"pki/node.key": []byte("private fixture"), "daemon.env": []byte("NODE=fixture")}
	if e := writeConfigTar(file, cfg); e != nil {
		t.Fatal(e)
	}
	f, e := os.Open(file)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	files := map[string][]byte{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if h.Mode != 0600 {
			t.Fatal("config permissions")
		}
		files[h.Name], e = io.ReadAll(tr)
		if e != nil {
			t.Fatal(e)
		}
	}
	var hashes map[string]string
	if e = json.Unmarshal(files["config.json"], &hashes); e != nil {
		t.Fatal(e)
	}
	for name, digest := range hashes {
		sum := sha256.Sum256(files[name])
		if digest != hex.EncodeToString(sum[:]) {
			t.Fatal("unbound config")
		}
	}
}

type releaseSSHFixture struct {
	calls    []string
	copies   int
	failNode string
	profile  string
}

func (f *releaseSSHFixture) Run(n model.NodeSpec, command string) (remote.Result, error) {
	f.calls = append(f.calls, n.Name+":"+command)
	if command == "uname -m" {
		return remote.Result{Stdout: "x86_64"}, nil
	}
	if strings.Contains(command, "check_output") {
		return remote.Result{Stdout: fmt.Sprintf(`{"build":{"os":"linux","arch":"amd64","state_profile":%q},"realm":"LAB","node":%q}`, f.profile, n.Name)}, nil
	}
	if strings.Contains(command, "mktemp -d") {
		return remote.Result{Stdout: "/tmp/titanus-release-fixture"}, nil
	}
	if strings.Contains(command, "bash '") && n.Name == f.failNode {
		return remote.Result{}, fmt.Errorf("injected failure")
	}
	return remote.Result{}, nil
}
func (f *releaseSSHFixture) CopyFile(n model.NodeSpec, local, path string) error {
	f.copies++
	return nil
}
func TestApplyChecksWholePlanBeforeMutationAndRecordsPartialCompletion(t *testing.T) {
	plan := model.RealmPlan{Version: "titanus-plan/v2", RealmName: "LAB", FabricCIDR: "10.210.0.0/16", ServiceCIDR: "10.220.0.0/16", Installation: &model.Installation{Operation: "rollback", ReleaseVersion: "0.4.0-rc.4", Revision: strings.Repeat("a", 40), APIPort: 9443, RaftPort: 9444, NodePrefix: 24, VXLANID: 4242}, Nodes: []model.NodeSpec{
		{Name: "worker", ManagementIP: "192.0.2.2", SSHUser: "root", SSHPort: 22, Capabilities: []model.Capability{model.CapabilityExecution}},
		{Name: "control", ManagementIP: "192.0.2.1", SSHUser: "root", SSHPort: 22, Capabilities: []model.Capability{model.CapabilityControl}},
	}}
	prepared, e := PrepareRelease(plan, filepath.Join(t.TempDir(), "prepared"))
	if e != nil {
		t.Fatal(e)
	}
	if prepared.Nodes[0].Node != "control" {
		t.Fatal("bootstrap controller not first")
	}
	ssh := &releaseSSHFixture{profile: version.StateProfile, failNode: "worker"}
	d := &RealmDeployer{SSH: ssh, ReleasePreflight: func(model.RealmPlan) error { return nil }}
	modified := plan
	modified.RealmName = "OTHER"
	if _, e = d.ApplyPrepared(modified, prepared); e == nil || len(ssh.calls) != 0 {
		t.Fatal("changed plan contacted hosts")
	}
	bad := prepared
	bad.Nodes = append([]PreparedNode(nil), prepared.Nodes...)
	bad.Nodes[1].Node = "wrong"
	if _, e = d.ApplyPrepared(plan, bad); e == nil || len(ssh.calls) != 0 {
		t.Fatal("invalid later node contacted hosts")
	}
	ssh.profile = "titanus-state/v1"
	if _, e = d.ApplyPrepared(plan, prepared); e == nil || ssh.copies != 0 {
		t.Fatal("incompatible installed profile mutated hosts")
	}
	ssh.profile = version.StateProfile
	ssh.calls = nil
	results, e := d.ApplyPrepared(plan, prepared)
	if e == nil || len(results) != 1 || results[0].Node != "control" {
		t.Fatalf("lost partial completion: %+v %v", results, e)
	}
	data, e := os.ReadFile(prepared.ResultsPath)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(data), "control") || strings.Contains(string(data), "worker") {
		t.Fatal("recorded uncompleted node")
	}
}
func TestCustomAPIPortIsAdvertised(t *testing.T) {
	env := agentEnvironment(model.RealmPlan{}, model.NodeSpec{Name: "worker", ManagementIP: "192.0.2.1"}, model.NodeSpec{ManagementIP: "192.0.2.2"}, RealmDeployOptions{ClusterPort: 10443})
	if !strings.Contains(env, "TITANUS_NODE_ADDRESS=192.0.2.1:10443\n") {
		t.Fatal("custom node API port lost")
	}
}
