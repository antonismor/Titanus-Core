package webinstaller

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/fencing"
	"github.com/antonismor/Titanus-Core/internal/model"
)

func TestApplyAutoQueryCephUsesCommonManagementNetworkAndUnusedDevices(t *testing.T) {
	plan := model.RealmPlan{
		Ceph:         model.CephSpec{Enabled: true, Replication: 3, RequestedTB: 10, EnableRBD: true, EnableCephFS: true},
		Installation: &model.Installation{Profile: model.ProfileNativeHA, Operation: "install"},
		Nodes: []model.NodeSpec{
			{Name: "ceph01", ManagementIP: "10.11.101.16", Capabilities: []model.Capability{model.CapabilityStorage}},
			{Name: "ceph02", ManagementIP: "10.11.101.17", Capabilities: []model.Capability{model.CapabilityStorage}},
			{Name: "ceph03", ManagementIP: "10.11.101.18", Capabilities: []model.Capability{model.CapabilityStorage}},
		},
	}
	probes := map[string]nodeProbe{}
	for index, node := range plan.Nodes {
		probes[node.Name] = nodeProbe{
			Node:  node.Name,
			CIDRs: []string{node.ManagementIP + "/24"},
			Devices: []probeDevice{
				{Path: "/dev/disk/by-id/test-data-" + string(rune('a'+index)), Size: 3_000_000_000_000},
			},
		}
	}
	findings, warnings := applyAutoQuery(&plan, probes, discoveryProfile{})
	if plan.Ceph.PublicCIDR != "10.11.101.0/24" || plan.Ceph.ClusterCIDR != "10.11.101.0/24" {
		t.Fatalf("unexpected Ceph networks: public=%q cluster=%q", plan.Ceph.PublicCIDR, plan.Ceph.ClusterCIDR)
	}
	for _, node := range plan.Nodes {
		if len(node.CephDevices) != 1 || !strings.HasPrefix(node.CephDevices[0], "/dev/disk/by-id/") {
			t.Fatalf("node %s devices were not auto-filled safely: %#v", node.Name, node.CephDevices)
		}
	}
	if len(findings) < 5 {
		t.Fatalf("expected detailed findings, got %#v", findings)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", warnings)
	}
}

func TestAutoQueryPreservesConflictingSelectionsAndReportsExistingState(t *testing.T) {
	plan := model.RealmPlan{
		Ceph:         model.CephSpec{Enabled: true, PublicCIDR: "10.230.0.0/24"},
		Installation: &model.Installation{Profile: model.ProfileNativeHA},
		Nodes:        []model.NodeSpec{{Name: "storage", ManagementIP: "192.0.2.1", Capabilities: []model.Capability{model.CapabilityStorage}, CephDevices: []string{"/dev/disk/by-id/selected"}}},
	}
	_, warnings := applyAutoQuery(&plan, map[string]nodeProbe{"storage": {Existing: true, CIDRs: []string{"192.0.2.1/24"}, Devices: []probeDevice{{Path: "/dev/disk/by-id/other", Size: 3e12}}}}, discoveryProfile{})
	if plan.Nodes[0].CephDevices[0] != "/dev/disk/by-id/selected" || len(warnings) < 3 {
		t.Fatalf("conflicts were silently overwritten or hidden: %v %v", plan.Nodes, warnings)
	}
}

func TestAutoQueryHTTPGuardsWithoutRemoteAccess(t *testing.T) {
	for _, scenario := range []string{"unauthenticated", "running", "quiescing", "missing-password"} {
		t.Run(scenario, func(t *testing.T) {
			s := testServer(t)
			s.running = scenario == "running"
			s.quiescing = scenario == "quiescing"
			body, _ := json.Marshal(autoQueryRequest{Plan: validPlan()})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/autofill", bytes.NewReader(body))
			if scenario != "unauthenticated" {
				req.Header.Set("X-Titanus-Installer-Token", s.accessToken)
			}
			res := httptest.NewRecorder()
			s.Handler().ServeHTTP(res, req)
			want := http.StatusConflict
			if scenario == "unauthenticated" {
				want = http.StatusUnauthorized
			}
			if scenario == "missing-password" {
				want = http.StatusBadRequest
			}
			if res.Code != want {
				t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
			}
		})
	}
}

func TestAutoQueryRejectsManifestOnlyArchiveAndPreservesPin(t *testing.T) {
	file := filepath.Join(t.TempDir(), "manifest-only.tar.gz")
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tr := tar.NewWriter(gz)
	manifest := []byte(`{"version":"0.4.0-rc.17","revision":"` + strings.Repeat("a", 40) + `","os":"linux","arch":"amd64"}`)
	if err := tr.WriteHeader(&tar.Header{Name: "titanus-0.4.0-rc.17-linux-amd64/manifest.json", Mode: 0600, Size: int64(len(manifest))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Write(manifest); err != nil {
		t.Fatal(err)
	}
	tr.Close()
	gz.Close()
	if err := os.WriteFile(file, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := verifiedArchive(file, "", "0.4.0-rc.17", strings.Repeat("a", 40), "amd64"); err == nil {
		t.Fatal("manifest-only archive accepted")
	}
	plan := validPlan()
	plan.Installation.Archives["amd64"] = model.Archive{Path: file, SHA256: strings.Repeat("f", 64)}
	_, warnings := autoFillArchives(&plan, discoveryProfile{})
	if plan.Installation.Archives["amd64"].SHA256 != strings.Repeat("f", 64) || len(warnings) == 0 {
		t.Fatal("invalid checksum was silently replaced")
	}
}

func TestReadOnlyProbeExcludesOSAndUsedDevices(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	fixture := `import subprocess,json,os,glob
os.geteuid=lambda:0
glob.glob=lambda _:[]
os.path.exists=lambda _:False
def output(args,**kw):
    if args[0]=="ip": return json.dumps([{"addr_info":[{"family":"inet","scope":"global","local":"192.0.2.1","prefixlen":24}]}])
    return json.dumps({"blockdevices":[{"type":"disk","path":"/dev/os","size":100,"children":[{"fstype":"ext4","mountpoints":["/"]}]},{"type":"disk","path":"/dev/data","size":3000000000000},{"type":"disk","path":"/dev/old-ceph","size":3000000000000}]})
subprocess.check_output=output
def run(args,**kw):
    return subprocess.CompletedProcess(args,0 if args[0]=="wipefs" else 3,stdout="ceph_bluestore" if args[-1]=="/dev/old-ceph" else "")
subprocess.run=run
`
	raw, err := exec.Command("python3", "-c", fixture+readOnlyProbePython).Output()
	if err != nil {
		t.Fatal(err)
	}
	var probe nodeProbe
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Devices) != 1 || probe.Devices[0].Path != "/dev/data" {
		t.Fatalf("unsafe device selection: %s", raw)
	}
}

func TestApplyAutoQueryRefreshesWorkerUUIDWithoutInventingSecrets(t *testing.T) {
	plan := model.RealmPlan{
		Installation: &model.Installation{
			Profile:   model.ProfileNativeHA,
			Operation: "install",
			PowerFencing: map[string]fencing.Target{
				"wrk01": {
					Provider:       "proxmox",
					Address:        "https://192.0.2.1:8006/api2/json/nodes/pve/qemu/2003",
					User:           "titanus@pve!fence",
					CredentialFile: "/root/not-present-token",
					CAFile:         "/root/not-present-ca",
				},
			},
		},
		Nodes: []model.NodeSpec{{Name: "wrk01", ManagementIP: "192.0.2.13", Capabilities: []model.Capability{model.CapabilityExecution}}},
	}
	probes := map[string]nodeProbe{"wrk01": {Node: "wrk01", UUID: "5F93D9AF-5A8E-4EE0-AC4D-CE63EB7B9204"}}
	_, warnings := applyAutoQuery(&plan, probes, discoveryProfile{})
	target := plan.Installation.PowerFencing["wrk01"]
	if target.UUID != "5f93d9af-5a8e-4ee0-ac4d-ce63eb7b9204" {
		t.Fatalf("worker UUID was not refreshed: %q", target.UUID)
	}
	if target.CredentialFile != "/root/not-present-token" || target.CAFile != "/root/not-present-ca" {
		t.Fatal("auto-query must preserve private file references and never invent secret contents")
	}
	if len(warnings) == 0 {
		t.Fatal("missing private files must be reported instead of silently accepted")
	}
}
