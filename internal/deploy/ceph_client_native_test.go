package deploy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNativeCephClientResumePreservesKeys(t *testing.T) {
	if os.Getenv("TITANUS_CEPHX_PROVISION_TEST") != "1" {
		t.Skip("requires isolated authenticated native Ceph")
	}
	dir := t.TempDir()
	data := os.Getenv("TITANUS_CEPHX_DATA_KEYRING")
	if data == "" {
		data = filepath.Join(dir, "data.keyring")
	}
	fence := os.Getenv("TITANUS_CEPHX_FENCE_KEYRING")
	if fence == "" {
		fence = filepath.Join(dir, "fence.keyring")
	}
	// Seed the old provisioning policy, then require a resumed run to converge
	// privileges without generating a replacement identity.
	if err := exec.Command("ceph", "auth", "get-or-create", "client.titanus", "mon", "allow r", "mgr", "allow rw", "osd", "allow rwx", "mds", "allow rw", "-o", data).Run(); err != nil {
		t.Fatal("seed old client", err)
	}
	old, e := exec.Command("ceph", "auth", "get-key", "client.titanus").Output()
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if out, e := exec.Command("bash", "-ec", cephClientIdentityCommand(data, fence)).CombinedOutput(); e != nil {
			t.Fatalf("resume %d: %v: %s", i, e, out)
		}
		current, e := exec.Command("ceph", "auth", "get-key", "client.titanus").Output()
		if e != nil {
			t.Fatal(e)
		}
		if string(current) != string(old) {
			t.Fatal("resume changed existing data identity")
		}
		out, e := exec.Command("ceph", "auth", "get", "client.titanus", "--format", "json").Output()
		if e != nil {
			t.Fatal(e)
		}
		var entries []struct {
			Entity string            `json:"entity"`
			Caps   map[string]string `json:"caps"`
		}
		if e = json.Unmarshal(out, &entries); e != nil {
			t.Fatal(e)
		}
		if len(entries) != 1 || entries[0].Caps["mon"] != "allow r, profile rbd" {
			t.Fatal("resumed client lacks native RBD lock-takeover authority")
		}
	}
}
