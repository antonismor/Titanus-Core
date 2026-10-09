package backup

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/antonismor/Titanus-Core/internal/consensus"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
)

func loadJSON(path string, value any) error {
	f, e := openRegular(path)
	if e != nil {
		return e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || st.Size() > 2<<20 {
		return fmt.Errorf("state JSON exceeds limit")
	}
	data, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return strictJSON(data, value)
}

func validateHost(p Plan) (realm.State, error) { return validateHostAt(p, p.roots()) }

func validateHostAt(p Plan, roots map[string]string) (realm.State, error) {
	var state realm.State
	for name, root := range roots {
		st, e := os.Lstat(root)
		if e != nil || !st.IsDir() || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) || (name != "state" && st.Mode().Perm()&0077 != 0) || (name == "state" && st.Mode().Perm()&0027 != 0) {
			return state, fmt.Errorf("missing, foreign-owned or non-private %s root", name)
		}
	}
	if e := rejectMounts(roots); e != nil {
		return state, e
	}
	if e := loadJSON(filepath.Join(roots["state"], "realm/state.json"), &state); e != nil {
		return state, e
	}
	if _, e := os.Lstat(filepath.Join(roots["state"], "realm/consensus")); e == nil {
		if p.Role != "controller" {
			return state, fmt.Errorf("HA state requires controller identity")
		}
		var persisted consensus.Config
		if e := loadJSON(filepath.Join(roots["state"], "realm/consensus/membership.json"), &persisted); e != nil {
			return state, e
		}
		configured, e := consensus.LoadConfig(filepath.Join(roots["configuration"], "ha.json"))
		if e != nil {
			return state, e
		}
		configured.Bootstrap = false
		canonical := state
		canonical.Revision = 0
		canonical.UpdatedAt = time.Time{}
		encoded, _ := json.Marshal(canonical)
		seedDigest := fmt.Sprintf("%x", sha256.Sum256(encoded))
		if configured.SeedHash != "" && configured.SeedHash != seedDigest {
			return state, fmt.Errorf("HA seed identity mismatch")
		}
		configured.SeedHash = seedDigest
		if persisted.ID != p.Node || persisted.Realm != p.Realm || !reflect.DeepEqual(persisted, configured) {
			return state, fmt.Errorf("HA membership/config/host identity mismatch")
		}
		state, e = consensus.ReadOfflineState(roots["state"])
		if e != nil {
			return state, e
		}
	} else if !os.IsNotExist(e) {
		return state, e
	}
	if state.Name != p.Realm || state.Nodes == nil || state.Fleets == nil || state.Assignments == nil || state.Routes == nil || state.Policies == nil {
		return state, fmt.Errorf("Realm identity/state mismatch")
	}
	if e := validatePKI(p, roots["configuration"], state.PKI); e != nil {
		return state, e
	}
	ledger := map[string]unitruntime.IDMapping{}
	if e := loadJSON(filepath.Join(roots["ledger"], "allocations.json"), &ledger); e != nil && !os.IsNotExist(e) {
		return state, e
	}
	seen := map[int]bool{}
	for key, m := range ledger {
		if key == "" || m.Size != 65536 || m.Base < 1048576 || m.Base > 2147418112 || m.Base%65536 != 0 || seen[m.Base] {
			return state, fmt.Errorf("invalid UID/GID ledger")
		}
		seen[m.Base] = true
	}
	for _, c := range state.Disks {
		if e := c.Validate(); e != nil {
			return state, e
		}
		if c.Spec.Provider != disk.ProviderLocal {
			return state, fmt.Errorf("external Ceph data is not included; full recovery requires a coordinated external-storage backup")
		}
	}
	var keyring *secrets.Keyring
	if _, e := os.Lstat(filepath.Join(roots["configuration"], "secrets.json")); e == nil {
		var err error
		keyring, err = secrets.Load(filepath.Join(roots["configuration"], "secrets.json"))
		if err != nil {
			return state, err
		}
	} else if !os.IsNotExist(e) {
		return state, e
	}
	for name, records := range state.Secrets {
		for _, r := range records {
			if r.Realm != p.Realm || r.Name != name {
				return state, fmt.Errorf("secret identity mismatch")
			}
			plain, e := keyring.Decrypt(r)
			if e != nil {
				return state, e
			}
			clear(plain)
		}
	}
	sm := source.NewManager(roots["state"])
	for name, record := range state.Sources {
		digest, e := sm.Identity(name)
		if e != nil || record.Name != name || digest != record.Digest {
			return state, fmt.Errorf("missing or changed committed Source %s", name)
		}
	}
	checkTemplate := func(template realm.UnitTemplate) error {
		exists, e := sm.Exists(template.Source)
		if e != nil || !exists {
			return fmt.Errorf("retained Fleet/Task Source missing")
		}
		return nil
	}
	for _, fleet := range state.Fleets {
		if e := checkTemplate(fleet.Template); e != nil {
			return state, e
		}
		for _, revision := range fleet.History {
			if e := checkTemplate(revision.Template); e != nil {
				return state, e
			}
		}
	}
	for _, task := range state.Tasks {
		if e := checkTemplate(task.Template); e != nil {
			return state, e
		}
	}
	units, e := os.ReadDir(filepath.Join(roots["state"], "units"))
	if e != nil && !os.IsNotExist(e) {
		return state, e
	}
	for _, d := range units {
		var spec unitruntime.Spec
		var s unitruntime.State
		root := filepath.Join(roots["state"], "units", d.Name())
		if !d.IsDir() || !namePattern.MatchString(d.Name()) {
			return state, fmt.Errorf("invalid Unit inventory")
		}
		if e := loadJSON(filepath.Join(root, "spec.json"), &spec); e != nil {
			return state, e
		}
		if e := spec.Validate(); e != nil {
			return state, e
		}
		if e := loadJSON(filepath.Join(root, "state.json"), &s); e != nil {
			return state, e
		}
		if s.ID != d.Name() || spec.ID != s.ID || s.PID != 0 || s.DesiredRunning || s.Status == unitruntime.StatusActive || s.Status == unitruntime.StatusStarting {
			return state, fmt.Errorf("Unit %s must be stopped before backup/recovery", d.Name())
		}
		if s.Status != unitruntime.StatusCreated && s.Status != unitruntime.StatusStopped && s.Status != unitruntime.StatusFailed {
			return state, fmt.Errorf("unknown Unit execution state")
		}
		if s.UserMapping.Size != 0 {
			key := filepath.Join(p.StateRoot, "units", s.ID) + "@" + s.CreatedAt.Format("20060102T150405.000000000Z")
			if spec.ClusterMappingKey != "" {
				key = "cluster:" + spec.ClusterMappingKey
				if spec.ClusterMapping != s.UserMapping {
					return state, fmt.Errorf("Unit/cluster mapping mismatch")
				}
			}
			if ledger[key] != s.UserMapping {
				return state, fmt.Errorf("Unit mapping missing/conflicting in node ledger")
			}
		}
		for _, b := range spec.SecretBindings {
			plain, e := keyring.Decrypt(b.Record)
			if e != nil {
				return state, e
			}
			clear(plain)
		}
		if exists, e := sm.Exists(spec.Source); e != nil || !exists {
			return state, fmt.Errorf("Unit Source missing")
		}
	}
	disks, e := os.ReadDir(filepath.Join(roots["state"], "disks"))
	if e != nil && !os.IsNotExist(e) {
		return state, e
	}
	for _, d := range disks {
		var spec disk.Spec
		if !d.IsDir() || !namePattern.MatchString(d.Name()) {
			return state, fmt.Errorf("invalid Disk inventory")
		}
		if e := loadJSON(filepath.Join(roots["state"], "disks", d.Name(), "disk.json"), &spec); e != nil {
			return state, e
		}
		if spec.Name != d.Name() || spec.Provider != disk.ProviderLocal || !spec.Initialized || spec.LayoutVersion != 1 {
			return state, fmt.Errorf("only initialized local Disk data is supported by host-local backup")
		}
		if st, e := os.Lstat(filepath.Join(roots["state"], "disks", d.Name(), "data")); e != nil || !st.IsDir() {
			return state, fmt.Errorf("missing local Disk data")
		}
	}
	return state, nil
}

func validateMappedOwners(entries []Entry, ledgerRoot string) error {
	ledger := map[string]unitruntime.IDMapping{}
	if e := loadJSON(filepath.Join(ledgerRoot, "allocations.json"), &ledger); e != nil && !os.IsNotExist(e) {
		return e
	}
	for _, entry := range entries {
		for _, id := range []int{entry.UID, entry.GID} {
			if id < 1048576 || id > 2147483647 {
				continue
			}
			found := false
			for _, allocation := range ledger {
				if id >= allocation.Base && id < allocation.Base+allocation.Size {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("numeric owner has no preserved node mapping: %s", entry.Path)
			}
		}
	}
	return nil
}

func validatePKI(p Plan, config string, policy identity.Policy) error {
	pki := filepath.Join(config, "pki")
	caData, e := os.ReadFile(filepath.Join(pki, "ca.crt"))
	if e != nil {
		return e
	}
	block, _ := pem.Decode(caData)
	if block == nil {
		return fmt.Errorf("invalid CA PEM")
	}
	ca, e := x509.ParseCertificate(block.Bytes)
	if e != nil || !ca.IsCA || ca.Subject.CommonName != "Titanus Realm CA: "+p.Realm {
		return fmt.Errorf("CA Realm identity mismatch")
	}
	pair, e := tls.LoadX509KeyPair(filepath.Join(pki, "node.crt"), filepath.Join(pki, "node.key"))
	if e != nil {
		return fmt.Errorf("node certificate/private key mismatch")
	}
	leaf, e := x509.ParseCertificate(pair.Certificate[0])
	if e != nil {
		return e
	}
	principal, e := identity.CertificatePrincipal(leaf)
	want := identity.RoleNode
	if p.Role == "controller" {
		want = identity.RoleController
	}
	if e != nil || principal.ID != p.Node || principal.Role != want || leaf.CheckSignatureFrom(ca) != nil {
		return fmt.Errorf("certificate backup Node/role/CA mismatch")
	}
	if p.Role == "controller" {
		if _, e := tls.LoadX509KeyPair(filepath.Join(pki, "ca.crt"), filepath.Join(pki, "ca.key")); e != nil {
			return fmt.Errorf("controller signing material unavailable/mismatched")
		}
	}
	if policy.CA != "" && policy.CA != fmt.Sprintf("%x", sha256.Sum256(ca.Raw)) {
		return fmt.Errorf("committed CA differs from backup identity")
	}
	crlData, e := os.ReadFile(filepath.Join(pki, "ca.crl"))
	if e != nil {
		return e
	}
	block, _ = pem.Decode(crlData)
	if block == nil {
		return fmt.Errorf("invalid CRL PEM")
	}
	list, e := x509.ParseRevocationList(block.Bytes)
	if e != nil || list.CheckSignatureFrom(ca) != nil {
		return fmt.Errorf("invalid CA-signed CRL")
	}
	// Local CRL may lag committed policy; preserve both, validate both signatures.
	if len(policy.CRL) > 0 {
		block, _ = pem.Decode(policy.CRL)
		if block == nil {
			return fmt.Errorf("invalid committed CRL")
		}
		committed, e := x509.ParseRevocationList(block.Bytes)
		if e != nil || committed.CheckSignatureFrom(ca) != nil {
			return fmt.Errorf("committed CRL signature mismatch")
		}
	}
	return filepath.WalkDir(config, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		st, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if st.Mode()&os.ModeSymlink != 0 {
			target, e := filepath.EvalSymlinks(path)
			if e != nil || !within(target, config) {
				return fmt.Errorf("identity symlink leaves backed-up configuration")
			}
		}
		if st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) || (st.Mode().IsRegular() && st.Mode().Perm()&0077 != 0 && (strings.HasSuffix(path, ".key") || strings.HasSuffix(path, "secrets.json") || strings.HasSuffix(path, ".env"))) {
			return fmt.Errorf("identity/configuration is not privately owned: %s (uid=%d, mode=%o)", path, st.Sys().(*syscall.Stat_t).Uid, st.Mode().Perm())
		}
		return nil
	})
}

func rejectMounts(roots map[string]string) error {
	data, e := os.ReadFile("/proc/self/mountinfo")
	if e != nil {
		return fmt.Errorf("mount status unavailable")
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		raw := fields[4]
		// mountinfo escapes whitespace/backslashes with octal byte sequences.
		for _, esc := range []string{"040", "011", "012", "134"} {
			n, _ := strconv.ParseInt(esc, 8, 8)
			raw = strings.ReplaceAll(raw, "\\"+esc, string(byte(n)))
		}
		for _, root := range roots {
			if within(raw, root) {
				return fmt.Errorf("offline backup/recovery refuses mounted root/data: %s", raw)
			}
		}
	}
	return nil
}

// Older binaries do not participate in the shared lock. Refuse any remaining
// named Titanus actor, including a surviving namespace init, before maintenance.
func rejectRunningActors() error {
	processes, e := os.ReadDir("/proc")
	if e != nil {
		return fmt.Errorf("process inventory unavailable")
	}
	for _, process := range processes {
		pid, e := strconv.Atoi(process.Name())
		if e != nil || pid == os.Getpid() {
			continue
		}
		name, e := os.ReadFile(filepath.Join("/proc", process.Name(), "comm"))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return fmt.Errorf("process exclusion cannot be established")
		}
		switch strings.TrimSpace(string(name)) {
		case "titanus", "titanusd", "titanus-agent", "titanus-init":
			return fmt.Errorf("Titanus process %d remains active; offline exclusion required", pid)
		}
	}
	return nil
}
