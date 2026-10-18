package deploy

import (
	"encoding/json"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/model"
	"net"
	"strings"
	"time"
)

func haPools(p model.RealmPlan) []string {
	pools := []string{}
	if p.Ceph.EnableRBD {
		pools = append(pools, "titanus")
	}
	if p.Ceph.EnableCephFS {
		pools = append(pools, "titanusfs_meta", "titanusfs_data")
	}
	return pools
}
func haCrushRuleCommand() string {
	return `$SUDO ceph osd crush rule ls --format json | python3 -c 'import json,sys;sys.exit(0 if "titanus-ha-host" in json.load(sys.stdin) else 1)' || $SUDO ceph osd crush rule create-replicated titanus-ha-host default host
$SUDO ceph osd crush rule dump titanus-ha-host --format json | python3 -c 'import json,sys; r=json.load(sys.stdin); sys.exit(0 if r.get("type")==1 and len(r.get("steps",[]))==3 and r["steps"][0].get("op")=="take" and r["steps"][0].get("item_name")=="default" and r["steps"][1].get("op") in ("chooseleaf_firstn","chooseleaf_indep") and r["steps"][1].get("type")=="host" and r["steps"][1].get("num")==0 and r["steps"][2].get("op")=="emit" else 1)'`
}
func validateHAQuorum(data []byte, p model.RealmPlan) error {
	var q struct {
		Quorum []string `json:"quorum_names"`
		Map    struct {
			Mons []struct {
				Name   string `json:"name"`
				Public string `json:"public_addr"`
				Addr   string `json:"addr"`
				Addrs  struct {
					Values []struct {
						Addr string `json:"addr"`
					} `json:"addrvec"`
				} `json:"public_addrs"`
			} `json:"mons"`
		} `json:"monmap"`
	}
	if e := json.Unmarshal(data, &q); e != nil {
		return e
	}
	expected := map[string]bool{}
	addresses := map[string]string{}
	for _, n := range p.CephMonitorNodes() {
		expected[n.Name] = true
		addresses[n.Name] = cephPublicIP(n)
	}
	if len(expected) != 3 || len(q.Quorum) != 3 || len(q.Map.Mons) != 3 {
		return fmt.Errorf("Native HA requires all three planned monitors in quorum")
	}
	seen := map[string]bool{}
	for _, name := range q.Quorum {
		if !expected[name] || seen[name] {
			return fmt.Errorf("unexpected/duplicate monitor quorum identity")
		}
		seen[name] = true
	}
	seen = map[string]bool{}
	for _, mon := range q.Map.Mons {
		if !expected[mon.Name] || seen[mon.Name] {
			return fmt.Errorf("monitor map differs from planned membership")
		}
		if ip := addresses[mon.Name]; ip != "" {
			candidates := []string{mon.Public, mon.Addr}
			for _, a := range mon.Addrs.Values {
				candidates = append(candidates, a.Addr)
			}
			found := false
			for _, address := range candidates {
				address = strings.TrimPrefix(strings.TrimPrefix(address, "v1:"), "v2:")
				address = strings.SplitN(address, "/", 2)[0]
				host, _, e := net.SplitHostPort(address)
				if e == nil && host == ip {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("monitor address differs from planned identity")
			}
		}
		seen[mon.Name] = true
	}
	return nil
}
func (d *CephDeployer) waitForHAQuorum(primary model.NodeSpec, p model.RealmPlan) error {
	for i := 0; i < 60; i++ {
		r, e := d.SSH.Run(primary, cephSudoPrefix+"; $SUDO timeout 10s ceph quorum_status --format json")
		if e == nil && validateHAQuorum([]byte(r.Stdout), p) == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("Native HA three-monitor quorum not confirmed")
}

// ValidateHACephReport is shared by provisioning and read-only transition probes.
// It requires clean data and explicit host failure domains, not merely ceph -s.
func ValidateHACephReport(data []byte, p model.RealmPlan) error {
	var report struct {
		Status struct {
			Health struct {
				Status string `json:"status"`
			} `json:"health"`
			PG struct {
				PGs    int `json:"num_pgs"`
				States []struct {
					State string `json:"state_name"`
					Count int    `json:"count"`
				} `json:"pgs_by_state"`
			} `json:"pgmap"`
		} `json:"status"`
		Quorum json.RawMessage `json:"quorum"`
		FS     json.RawMessage `json:"fs"`
		MGR    struct {
			Active   string `json:"active_name"`
			Standbys []struct {
				Name string `json:"name"`
			} `json:"standbys"`
		} `json:"mgr"`
		Pools []struct {
			Name string `json:"pool_name"`
			ID   int    `json:"pool"`
			Size int    `json:"size"`
			Min  int    `json:"min_size"`
			Rule int    `json:"crush_rule"`
		} `json:"pools"`
		Rule struct {
			ID    int `json:"rule_id"`
			Type  int `json:"type"`
			Steps []struct {
				Op       string `json:"op"`
				Type     string `json:"type"`
				ItemName string `json:"item_name"`
				Num      int    `json:"num"`
			} `json:"steps"`
		} `json:"rule"`
		Tree struct {
			Nodes []struct {
				ID       int    `json:"id"`
				Name     string `json:"name"`
				Type     string `json:"type"`
				Children []int  `json:"children"`
				Status   string `json:"status"`
			} `json:"nodes"`
		} `json:"tree"`
	}
	if e := json.Unmarshal(data, &report); e != nil {
		return e
	}
	if e := validateHAQuorum(report.Quorum, p); e != nil {
		return e
	}
	if report.Status.Health.Status != "HEALTH_OK" || report.Status.PG.PGs < 1 {
		return fmt.Errorf("Native HA storage must be HEALTH_OK with clean PGs")
	}
	clean := 0
	for _, pg := range report.Status.PG.States {
		if pg.State != "active+clean" {
			return fmt.Errorf("Native HA storage PGs are not all active+clean")
		}
		clean += pg.Count
	}
	if clean != report.Status.PG.PGs {
		return fmt.Errorf("incomplete PG evidence")
	}
	steps := report.Rule.Steps
	if report.Rule.Type != 1 || len(steps) != 3 || steps[0].Op != "take" || steps[0].ItemName != "default" || (steps[1].Op != "chooseleaf_firstn" && steps[1].Op != "chooseleaf_indep") || steps[1].Type != "host" || steps[1].Num != 0 || steps[2].Op != "emit" {
		return fmt.Errorf("Native HA requires the default replicated host-domain CRUSH rule")
	}
	plannedManagers := map[string]bool{}
	for _, n := range p.StorageNodes() {
		plannedManagers[n.Name] = true
	}
	managerReady := false
	for _, m := range report.MGR.Standbys {
		if plannedManagers[report.MGR.Active] && plannedManagers[m.Name] && report.MGR.Active != m.Name {
			managerReady = true
		}
	}
	if !managerReady {
		return fmt.Errorf("Native HA requires active and standby MGR on separate planned storage hosts")
	}
	if p.Ceph.EnableCephFS {
		poolIDs := map[string]int{}
		for _, pool := range report.Pools {
			poolIDs[pool.Name] = pool.ID
		}
		if e := validateHAMDS(report.FS, p, poolIDs); e != nil {
			return e
		}
	}
	pools := map[string]bool{}
	for _, pool := range report.Pools {
		if pool.Size == 3 && pool.Min == 2 && pool.Rule == report.Rule.ID {
			pools[pool.Name] = true
		}
	}
	for _, name := range haPools(p) {
		if !pools[name] {
			return fmt.Errorf("pool %s lacks size=3/min_size=2/host-rule policy", name)
		}
	}
	upOSD := map[int]bool{}
	for _, n := range report.Tree.Nodes {
		if n.Type == "osd" && n.Status == "up" {
			upOSD[n.ID] = true
		}
	}
	hosts := map[string]bool{}
	for _, n := range report.Tree.Nodes {
		if n.Type == "host" {
			for _, id := range n.Children {
				if upOSD[id] {
					hosts[n.Name] = true
				}
			}
		}
	}
	for _, n := range p.StorageNodes() {
		if !hosts[n.Name] {
			return fmt.Errorf("storage host %s has no up OSD in its own host failure domain", n.Name)
		}
	}
	return nil
}
func HACephProbeCommand() string {
	// Each subprocess is bounded; no cluster settings or data are modified.
	return `python3 -c 'import json,subprocess,os; base=json.loads(os.environ.get("TITANUS_CEPH_ARGS","[]")); run=lambda *a: json.loads(subprocess.check_output(["ceph",*base,*a,"--format","json"],timeout=15)); print(json.dumps({"status":run("status"),"quorum":run("quorum_status"),"pools":run("osd","pool","ls","detail"),"rule":run("osd","crush","rule","dump","titanus-ha-host"),"tree":run("osd","tree"),"fs":run("fs","dump"),"mgr":run("mgr","dump")}))'`
}
func (d *CephDeployer) verifyHACeph(primary model.NodeSpec, p model.RealmPlan) error {
	var last error
	for i := 0; i < 30; i++ {
		r, e := d.SSH.Run(primary, cephSudoPrefix+"; $SUDO "+HACephProbeCommand())
		if e == nil {
			e = ValidateHACephReport([]byte(r.Stdout), p)
		}
		if e == nil {
			return nil
		}
		last = e
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("Native HA storage did not become clean: %w", last)
}

func validateHAMDS(data []byte, p model.RealmPlan, poolIDs map[string]int) error {
	var fs struct {
		Filesystems []struct {
			ID     int `json:"id"`
			MDSMap struct {
				FSName       string `json:"fs_name"`
				MetadataPool int    `json:"metadata_pool"`
				MaxMDS       int    `json:"max_mds"`
				DataPools    []int  `json:"data_pools"`
				Info         map[string]struct {
					Name  string `json:"name"`
					State string `json:"state"`
				} `json:"info"`
			} `json:"mdsmap"`
		} `json:"filesystems"`
		Standbys []struct {
			Name    string `json:"name"`
			ForFS   *int   `json:"standby_for_fscid"`
			ForName string `json:"standby_for_name"`
			ForRank *int   `json:"standby_for_rank"`
		} `json:"standbys"`
	}
	if e := json.Unmarshal(data, &fs); e != nil {
		return e
	}
	planned := map[string]bool{}
	for _, n := range p.StorageNodes() {
		planned[n.Name] = true
	}
	meta, hasMeta := poolIDs["titanusfs_meta"]
	dataPool, hasData := poolIDs["titanusfs_data"]
	if !hasMeta || !hasData || meta == dataPool {
		return fmt.Errorf("CephFS HA pool identities missing")
	}
	active, standby := map[string]bool{}, map[string]bool{}
	targetFSID := -1
	for _, f := range fs.Filesystems {
		if f.MDSMap.FSName != "titanusfs" {
			continue
		}
		targetFSID = f.ID
		if f.MDSMap.MaxMDS != 1 {
			return fmt.Errorf("Native HA CephFS currently supports one active rank plus a standby")
		}
		if f.MDSMap.MetadataPool != meta || len(f.MDSMap.DataPools) != 1 || f.MDSMap.DataPools[0] != dataPool {
			return fmt.Errorf("CephFS does not use the verified HA pools")
		}
		for _, m := range f.MDSMap.Info {
			if planned[m.Name] && m.State == "up:active" {
				active[m.Name] = true
			}
			if planned[m.Name] && m.State == "up:standby-replay" {
				standby[m.Name] = true
			}
		}
	}
	for _, m := range fs.Standbys {
		if planned[m.Name] && (m.ForFS == nil || *m.ForFS < 0 || *m.ForFS == targetFSID) && (m.ForName == "" || active[m.ForName]) && (m.ForRank == nil || *m.ForRank < 0 || *m.ForRank == 0) {
			standby[m.Name] = true
		}
	}
	for a := range active {
		for b := range standby {
			if a != b {
				return nil
			}
		}
	}
	return fmt.Errorf("Native HA CephFS requires active and standby MDS on separate planned storage hosts")
}

// ValidateHACephCatalogReport binds the healthy cluster to every retained Disk.
func ValidateHACephCatalogReport(data []byte, p model.RealmPlan, catalogs map[string]disk.Catalog) error {
	if e := ValidateHACephReport(data, p); e != nil {
		return e
	}
	var r struct {
		Status struct {
			FSID string `json:"fsid"`
		} `json:"status"`
	}
	if e := json.Unmarshal(data, &r); e != nil {
		return e
	}
	for _, c := range catalogs {
		if c.Backend.FSID == "" || c.Backend.FSID != r.Status.FSID {
			return fmt.Errorf("Ceph report differs from retained Disk FSID")
		}
		if c.Spec.Provider == disk.ProviderCephRBD && (!p.Ceph.EnableRBD || c.Backend.Pool != "titanus") {
			return fmt.Errorf("retained RBD pool lacks verified HA policy")
		}
		if c.Spec.Provider == disk.ProviderCephFS && (!p.Ceph.EnableCephFS || c.Backend.Filesystem != "titanusfs") {
			return fmt.Errorf("retained CephFS lacks verified HA policy")
		}
	}
	return nil
}

func cephHostConfig(base string, n model.NodeSpec, nativeHA bool) string {
	if nativeHA {
		for _, c := range n.Capabilities {
			if c == model.CapabilityStorage {
				return base + "\n[osd]\ncrush_location = root=default host=" + n.Name + "\n"
			}
		}
	}
	return base
}
