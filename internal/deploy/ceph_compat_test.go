package deploy

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/model"
)

func TestCephMonitorAddressFallsBackToManagement(t *testing.T) {
	plan := model.RealmPlan{
		RealmName: "test-ceph",
		Nodes: []model.NodeSpec{
			{Name: "storage-a", ManagementIP: "10.11.101.15", Capabilities: []model.Capability{model.CapabilityStorage}},
			{Name: "storage-b", ManagementIP: "10.11.101.16", Capabilities: []model.Capability{model.CapabilityStorage}},
			{Name: "storage-c", ManagementIP: "10.11.101.17", Capabilities: []model.Capability{model.CapabilityStorage}},
		},
		Ceph: model.CephSpec{Enabled: true, PublicCIDR: "10.11.101.0/24", ClusterCIDR: "10.11.101.0/24", Replication: 3},
	}
	conf := buildCephConfig(plan, "4df38c4a-1c25-4017-90d7-b548ee8eac01")
	if !strings.Contains(conf, "mon_host = 10.11.101.15,10.11.101.16,10.11.101.17") {
		t.Fatalf("Ceph must not use empty monitor endpoints: %s", conf)
	}
	plan.Nodes[0].CephPublicIP = "10.11.101.115"
	conf = buildCephConfig(plan, "4df38c4a-1c25-4017-90d7-b548ee8eac01")
	if !strings.Contains(conf, "mon_host = 10.11.101.115,10.11.101.16,10.11.101.17") {
		t.Fatalf("explicit Ceph public IP should take precedence: %s", conf)
	}
}

func TestDebian13ReefManagerCompatibilityIsNarrowAndIdempotent(t *testing.T) {
	cmd := cephReefManagerCompatibilityCommand()
	for _, required := range []string{
		"VERSION_ID=",
		"ceph version 18\\.2\\.7",
		"3.13",
		"# Titanus Debian 13 Reef PyO3 compatibility",
		".titanus-original",
		"refusing unknown Debian 13 Ceph Reef mgr_util.py layout",
		"systemctl restart",
	} {
		if !strings.Contains(cmd, required) {
			t.Fatalf("compatibility command missing guard/action %q: %s", required, cmd)
		}
	}
	if strings.Contains(cmd, "apt-get install") || strings.Contains(cmd, "pip install") {
		t.Fatalf("compatibility shim must not replace distribution Python packages: %s", cmd)
	}
	shell := exec.Command("bash", "-n")
	shell.Stdin = strings.NewReader(cmd)
	if output, err := shell.CombinedOutput(); err != nil {
		t.Fatalf("compatibility command is not valid shell: %v: %s", err, output)
	}
}

func TestCephBootstrapMonmapCreationIsRerunnable(t *testing.T) {
	cmd := cephMonmapCreateCommand(" --add storage-a 10.11.101.15", "4df38c4a-1c25-4017-90d7-b548ee8eac01")
	if !strings.Contains(cmd, "monmaptool --create --clobber") {
		t.Fatalf("Ceph bootstrap monmap must be replaceable during idempotent deploy: %s", cmd)
	}
	if !strings.Contains(cmd, "--fsid") || !strings.Contains(cmd, "4df38c4a-1c25-4017-90d7-b548ee8eac01") {
		t.Fatalf("Ceph bootstrap monmap must preserve the cluster FSID: %s", cmd)
	}
}

func TestDebian13ReefManagerPolicyDisablesOnlyOptionalBrokenRestful(t *testing.T) {
	cmd := cephReefManagerPolicyCommand()
	for _, required := range []string{
		`VERSION_ID="?13"?`,
		`ceph version 18\.2\.7`,
		`enabled_modules`,
		`"restful"`,
		`ceph mgr module disable restful`,
	} {
		if !strings.Contains(cmd, required) {
			t.Fatalf("manager compatibility policy missing guard/action %q: %s", required, cmd)
		}
	}
	for _, forbidden := range []string{
		"module disable volumes",
		"module disable rbd_support",
		"module disable status",
	} {
		if strings.Contains(cmd, forbidden) {
			t.Fatalf("manager compatibility policy must not disable required module %q: %s", forbidden, cmd)
		}
	}
	shell := exec.Command("bash", "-n")
	shell.Stdin = strings.NewReader(cmd)
	if output, err := shell.CombinedOutput(); err != nil {
		t.Fatalf("manager compatibility policy is not valid shell: %v: %s", err, output)
	}
}
