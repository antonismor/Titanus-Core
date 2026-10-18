package deploy

import (
	"encoding/json"
	"github.com/antonismor/Titanus-Core/internal/fencing"
	"github.com/antonismor/Titanus-Core/internal/model"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeHAFencingPackagesPrivatePinnedCredentials(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned fencing files: covered by dedicated isolated root CI step")
	}
	dir := t.TempDir()
	secret := filepath.Join(dir, "credential")
	if e := os.WriteFile(secret, []byte("private-test-token"), 0600); e != nil {
		t.Fatal(e)
	}
	i := model.Installation{Profile: model.ProfileNativeHA, Operation: "install", PowerFencing: map[string]fencing.Target{"worker": {Provider: "ipmi", UUID: "00000000-0000-0000-0000-000000000004", Address: "192.0.2.50", User: "fencer", CredentialFile: secret}}}
	cfg, e := NativeHAFencingConfig(i)
	if e != nil {
		t.Fatal(e)
	}
	var targets map[string]fencing.Target
	if e = json.Unmarshal(cfg["power-fencing.json"], &targets); e != nil {
		t.Fatal(e)
	}
	if targets["worker"].CredentialFile != "/etc/titanus/fencing/worker.credential" || string(cfg["fencing/worker.credential"]) != "private-test-token" {
		t.Fatal("credential reference not bound to packaged bytes")
	}
	plan := model.RealmPlan{RealmName: "LAB", Installation: &i}
	node := model.NodeSpec{Name: "worker"}
	script := releaseApplyScript(plan, node, i, false, cfg)
	if strings.Contains(script, "private-test-token") || !strings.Contains(script, "--native-ha") {
		t.Fatal("script leaked secret or missed worker schema seed")
	}
	if e = os.Chmod(secret, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e = NativeHAFencingConfig(i); e == nil {
		t.Fatal("public credential packaged")
	}
	os.Chmod(secret, 0600)
	link := filepath.Join(dir, "symlink")
	if e = os.Symlink(secret, link); e != nil {
		t.Fatal(e)
	}
	target := i.PowerFencing["worker"]
	target.CredentialFile = link
	i.PowerFencing["worker"] = target
	if _, e = NativeHAFencingConfig(i); e == nil {
		t.Fatal("redirected secret packaged")
	}
}
