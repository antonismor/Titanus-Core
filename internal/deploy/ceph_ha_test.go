package deploy

import (
	"encoding/json"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/model"
	"strings"
	"testing"
)

func haReportFixture() (map[string]any, model.RealmPlan) {
	p := model.RealmPlan{Installation: &model.Installation{Profile: model.ProfileNativeHA}, Ceph: model.CephSpec{Enabled: true, EnableRBD: true, Replication: 3}}
	for _, name := range []string{"a", "b", "c"} {
		p.Nodes = append(p.Nodes, model.NodeSpec{Name: name, Capabilities: []model.Capability{model.CapabilityStorage}})
	}
	report := map[string]any{"quorum": map[string]any{"quorum_names": []string{"a", "b", "c"}, "monmap": map[string]any{"mons": []map[string]any{{"name": "a"}, {"name": "b"}, {"name": "c"}}}}, "status": map[string]any{"health": map[string]any{"status": "HEALTH_OK"}, "pgmap": map[string]any{"num_pgs": 8, "pgs_by_state": []map[string]any{{"state_name": "active+clean", "count": 8}}}}, "pools": []map[string]any{{"pool_name": "titanus", "size": 3, "min_size": 2, "crush_rule": 7}}, "rule": map[string]any{"rule_id": 7, "type": 1, "steps": []map[string]any{{"op": "take", "item_name": "default"}, {"op": "chooseleaf_firstn", "type": "host", "num": 0}, {"op": "emit"}}}, "tree": map[string]any{"nodes": []map[string]any{{"id": 0, "type": "osd", "status": "up"}, {"id": 1, "type": "osd", "status": "up"}, {"id": 2, "type": "osd", "status": "up"}, {"id": -1, "name": "a", "type": "host", "children": []int{0}}, {"id": -2, "name": "b", "type": "host", "children": []int{1}}, {"id": -3, "name": "c", "type": "host", "children": []int{2}}}}}
	return report, p
}
func TestHACephEvidenceRejectsUnsafeQuorumPoolsAndDomains(t *testing.T) {
	report, p := haReportFixture()
	b, _ := json.Marshal(report)
	if e := ValidateHACephReport(b, p); e != nil {
		t.Fatal(e)
	}
	changes := []func(map[string]any){
		func(r map[string]any) { r["quorum"].(map[string]any)["quorum_names"] = []string{"a", "a", "c"} },
		func(r map[string]any) { r["pools"].([]map[string]any)[0]["min_size"] = 1 },
		func(r map[string]any) { r["pools"].([]map[string]any)[0]["size"] = 2 },
		func(r map[string]any) {
			r["rule"].(map[string]any)["steps"] = []map[string]any{{"op": "chooseleaf_firstn", "type": "osd"}}
		},
		func(r map[string]any) { r["tree"].(map[string]any)["nodes"].([]map[string]any)[2]["status"] = "down" },
		func(r map[string]any) {
			r["status"].(map[string]any)["health"] = map[string]any{"status": "HEALTH_WARN"}
		},
	}
	for i, change := range changes {
		r, p := haReportFixture()
		change(r)
		b, _ := json.Marshal(r)
		if ValidateHACephReport(b, p) == nil {
			t.Fatalf("unsafe Ceph evidence %d accepted", i)
		}
	}
	cfg := buildCephConfig(p, "fixture")
	if !strings.Contains(cfg, "osd_pool_default_min_size = 2") {
		t.Fatal("HA min_size missing")
	}
}

func TestHACephFSRequiresSeparateActiveAndStandbyMDS(t *testing.T) {
	_, p := haReportFixture()
	p.Ceph.EnableCephFS = true
	good := []byte(`{"filesystems":[{"mdsmap":{"fs_name":"titanusfs","info":{"gid1":{"name":"a","state":"up:active"}}}}],"standbys":[{"name":"b"}]}`)
	if e := validateHAMDS(good, p); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{`{}`, strings.Replace(string(good), `"name":"b"`, `"name":"a"`, 1), strings.Replace(string(good), `up:active`, `up:standby`, 1)} {
		if validateHAMDS([]byte(bad), p) == nil {
			t.Fatal("missing or same-host MDS standby accepted")
		}
	}
}

func TestHACephCatalogEvidenceBindsOriginalFSIDAndPool(t *testing.T) {
	r, p := haReportFixture()
	r["status"].(map[string]any)["fsid"] = "original-fsid"
	c := map[string]disk.Catalog{"data": {Spec: disk.Spec{Provider: disk.ProviderCephRBD}, Backend: disk.Backend{FSID: "original-fsid", Pool: "titanus"}}}
	b, _ := json.Marshal(r)
	if e := ValidateHACephCatalogReport(b, p, c); e != nil {
		t.Fatal(e)
	}
	old := c["data"]
	changed := old
	changed.Backend.FSID = "different-fsid"
	c["data"] = changed
	if ValidateHACephCatalogReport(b, p, c) == nil {
		t.Fatal("different Ceph cluster accepted")
	}
	changed = old
	changed.Backend.Pool = "unverified"
	c["data"] = changed
	if ValidateHACephCatalogReport(b, p, c) == nil {
		t.Fatal("unverified retained pool accepted")
	}
}

func TestHACephHostLocationUsesPlannedNodeIdentity(t *testing.T) {
	n := model.NodeSpec{Name: "planned-host", Capabilities: []model.Capability{model.CapabilityStorage}}
	if got := cephHostConfig("[global]\n", n, true); !strings.Contains(got, "crush_location = root=default host=planned-host") {
		t.Fatal("unrelated OS hostname could determine HA domain")
	}
	if cephHostConfig("legacy", n, false) != "legacy" {
		t.Fatal("legacy config changed")
	}
}

func TestHACephMonitorAddressFormatsBindPlannedIP(t *testing.T) {
	_, p := haReportFixture()
	p.Nodes[0].CephPublicIP = "192.0.2.1"
	for _, field := range []string{`"public_addr":"192.0.2.1:6789/0"`, `"addr":"v1:192.0.2.1:6789/0"`, `"public_addrs":{"addrvec":[{"addr":"192.0.2.1:3300","type":"v2"}]}`} {
		raw := `{"quorum_names":["a","b","c"],"monmap":{"mons":[{"name":"a",` + field + `},{"name":"b"},{"name":"c"}]}}`
		if e := validateHAQuorum([]byte(raw), p); e != nil {
			t.Fatal(e)
		}
		if validateHAQuorum([]byte(strings.ReplaceAll(raw, "192.0.2.1", "192.0.2.2")), p) == nil {
			t.Fatal("wrong monitor IP accepted")
		}
	}
}
