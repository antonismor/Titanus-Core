// Package haprofile defines the authenticated profile description and local readiness.
// It has no consensus storage or backup reader dependency.
package haprofile

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"net"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type HostPlan struct {
	Format     string `json:"format"`
	Realm      string `json:"realm"`
	Node       string `json:"node"`
	Role       string `json:"role"`
	StateRoot  string `json:"state_root"`
	ConfigRoot string `json:"config_root"`
	LedgerRoot string `json:"ledger_root"`
}

type Prepared struct {
	Format            string            `json:"format"`
	ID                string            `json:"id"`
	Plan              model.RealmPlan   `json:"plan"`
	BackupSetID       string            `json:"backup_set_id"`
	BackupRevision    uint64            `json:"backup_revision"`
	BackupStateSHA256 string            `json:"backup_state_sha256"`
	Hosts             []HostPlan        `json:"hosts"`
	FencingSHA256     map[string]string `json:"fencing_sha256"`
}

func Digest(v any) string { b, _ := json.Marshal(v); return fmt.Sprintf("%x", sha256.Sum256(b)) }
func (p Prepared) Validate() error {
	hash := regexp.MustCompile(`^[a-f0-9]{64}$`)
	if p.Format != "titanus-ha-transition/v1" || !hash.MatchString(p.BackupSetID) || !hash.MatchString(p.BackupStateSHA256) || p.BackupRevision == 0 {
		return fmt.Errorf("invalid transition backup identity")
	}
	id := p.ID
	p.ID = ""
	if len(id) != 64 || Digest(p) != id {
		return fmt.Errorf("transition identity/content changed")
	}
	if p.Plan.Installation == nil || p.Plan.Installation.Profile != model.ProfileNativeHA {
		return fmt.Errorf("Native HA target required")
	}
	if len(p.Hosts) != len(p.Plan.Nodes) {
		return fmt.Errorf("complete host inventory required")
	}
	seen := map[string]bool{}
	targets := map[string]bool{}
	for _, n := range p.Plan.Nodes {
		targets[n.Name] = true
	}
	for _, h := range p.Hosts {
		if seen[h.Node] || !targets[h.Node] || h.Realm != p.Plan.RealmName || h.Format != "titanus-backup-plan/v1" || (h.Role != "controller" && h.Role != "node") {
			return fmt.Errorf("invalid host identity")
		}
		seen[h.Node] = true
		for _, r := range []string{h.ConfigRoot, h.StateRoot, h.LedgerRoot} {
			if !filepath.IsAbs(r) || filepath.Clean(r) != r || r == "/" {
				return fmt.Errorf("unsafe host root")
			}
		}
	}
	if len(p.FencingSHA256) == 0 {
		return fmt.Errorf("private fencing inventory required")
	}
	for name, h := range p.FencingSHA256 {
		if name == "." || filepath.IsAbs(name) || filepath.Clean(name) != name || strings.Contains(name, "..") || !hash.MatchString(h) {
			return fmt.Errorf("unsafe fencing inventory")
		}
	}
	return p.Plan.Validate()
}
func ValidateTarget(state realm.State, p model.RealmPlan) error {
	if e := p.Validate(); e != nil {
		return e
	}
	if p.Installation == nil || p.Installation.Profile != model.ProfileNativeHA || p.Installation.Operation != "install" || p.Ceph.Provision {
		return fmt.Errorf("transition target must describe Native HA without provisioning")
	}
	if state.Name != p.RealmName || len(state.Nodes) != len(p.Nodes) || state.Network.FabricCIDR != p.FabricCIDR || state.Network.ServiceCIDR != p.ServiceCIDR || state.Network.NodePrefix != p.Installation.NodePrefix || state.Network.VXLANID != p.Installation.VXLANID {
		return fmt.Errorf("transition preserves Realm, host inventory and Fabric identity")
	}
	if state.NativeHA {
		return fmt.Errorf("Realm is already Native HA")
	}
	for _, n := range p.Nodes {
		old, ok := state.Nodes[n.Name]
		if !ok || old.PowerQuarantined || old.StorageQuarantined || old.State == realm.NodeDisabled || old.State == realm.NodeDraining {
			return fmt.Errorf("host %s missing, quarantined or administratively disabled", n.Name)
		}
		expected := net.JoinHostPort(n.ManagementIP, strconv.Itoa(p.Installation.APIPort))
		if strings.TrimPrefix(old.Address, "https://") != expected {
			return fmt.Errorf("host %s address differs from existing identity", n.Name)
		}
		oldControl, newControl := false, false
		for _, c := range old.Capabilities {
			oldControl = oldControl || c == model.CapabilityControl
		}
		for _, c := range n.Capabilities {
			newControl = newControl || c == model.CapabilityControl
		}
		if oldControl != newControl {
			return fmt.Errorf("controller membership cannot change during profile transition")
		}
		// Only remove EXECUTION from a CONTROL voter; other role changes require
		// a separate deployment. No storage or Gateway role is silently removed.
		before, after := map[model.Capability]bool{}, map[model.Capability]bool{}
		for _, c := range old.Capabilities {
			before[c] = true
		}
		for _, c := range n.Capabilities {
			after[c] = true
		}
		if oldControl {
			delete(before, model.CapabilityExecution)
		}
		if !reflect.DeepEqual(before, after) {
			return fmt.Errorf("host %s changes unrelated capabilities", n.Name)
		}
	}
	if len(state.Assignments) != 0 {
		return fmt.Errorf("quiesce every Fleet and acknowledge all Unit retirements before transition")
	}
	for _, f := range state.Fleets {
		if f.Instances != 0 {
			return fmt.Errorf("Fleet %s must be quiesced; preserve its desired settings in the maintenance journal", f.Name)
		}
	}
	for _, t := range state.Tasks {
		if !t.Terminal() {
			return fmt.Errorf("nonterminal or uncertain Task prevents transition")
		}
	}
	for _, f := range state.GatewayTransfers {
		if !f.Complete {
			return fmt.Errorf("pending Gateway transfer")
		}
	}
	for _, f := range state.StorageFailovers {
		if !f.Completed {
			return fmt.Errorf("pending storage fence")
		}
	}
	if len(state.Disks) > 0 && !p.NativeHAStorage() {
		return fmt.Errorf("existing remote Disks require an explicit verified Ceph HA target")
	}
	for _, c := range state.Disks {
		if c.Spec.Provider == disk.ProviderCephRBD && (!p.Ceph.EnableRBD || c.Backend.Pool != "titanus") {
			return fmt.Errorf("retained RBD pool requires explicit verified HA policy")
		}
		if c.Spec.Provider == disk.ProviderCephFS && (!p.Ceph.EnableCephFS || c.Backend.Filesystem != "titanusfs") {
			return fmt.Errorf("retained CephFS requires explicit verified HA policy")
		}
		if c.Spec.Provider == disk.ProviderLocal || !c.AutoFailover {
			return fmt.Errorf("all existing Disks need verified remote automatic-failover catalogs; local data is preserved and migration is refused")
		}
	}
	return nil
}
func Capabilities(caps []model.Capability) string {
	a := []string{}
	for _, c := range caps {
		a = append(a, string(c))
	}
	sort.Strings(a)
	return strings.Join(a, ",")
}
