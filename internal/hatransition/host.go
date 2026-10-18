package hatransition

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/antonismor/Titanus-Core/internal/backup"
	"github.com/antonismor/Titanus-Core/internal/consensus"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/fencing"
	"github.com/antonismor/Titanus-Core/internal/haprofile"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/offline"
)

type FileChange = haprofile.FileChange
type HostReceipt = haprofile.HostReceipt

var receiptPath = haprofile.ReceiptPath
var fileMatches = haprofile.FileMatches
var readReceipt = haprofile.ReadReceipt
var validReceipt = haprofile.ValidReceipt
var ReadReady = haprofile.ReadReady

func envValue(data []byte, key, value string) ([]byte, error) {
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	found := false
	for i, line := range lines {
		if strings.HasPrefix(line, key+"=") {
			if found {
				return nil, fmt.Errorf("duplicate environment key %s", key)
			}
			lines[i] = key + "=" + value
			found = true
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}
func Stage(preparedPath, setPath, keyPath, node string) error {
	l, e := offline.Exclusive()
	if e != nil {
		return e
	}
	defer l.Close()
	p, e := Load(preparedPath)
	if e != nil {
		return e
	}
	set, e := backup.VerifyClusterSet(setPath, keyPath)
	if e != nil {
		return e
	}
	if set.Intent.Binding.ID != p.BackupSetID || set.Intent.Binding.RealmSHA256 != p.BackupStateSHA256 || set.Intent.Binding.Revision != p.BackupRevision || !reflect.DeepEqual(hostPlans(set.Intent.Hosts), p.Hosts) {
		return fmt.Errorf("transition recovery set differs")
	}
	var host backup.Plan
	var target model.NodeSpec
	for _, h := range p.Hosts {
		if h.Node == node {
			host = backup.Plan(h)
		}
	}
	for _, n := range p.Plan.Nodes {
		if n.Name == node {
			target = n
		}
	}
	if host.Node == "" || target.Name == "" {
		return fmt.Errorf("node not in transition")
	}
	path, e := receiptPath(host.StateRoot, p.ID)
	if e != nil {
		return e
	}
	r, e := readReceipt(path)
	if os.IsNotExist(e) {
		// A controller's authenticated offline checkpoint must still match the
		// quiescent recovery set. Consensus logs/membership are never replaced.
		if host.Role == "controller" {
			state, e := consensus.ReadOfflineState(host.StateRoot)
			if e != nil {
				return e
			}
			if Digest(state) != p.BackupStateSHA256 {
				return fmt.Errorf("controller changed since recovery-set backup")
			}
		}
		// Match protected environments against the authenticated host archive,
		// including their exact bytes, before preserving them for rollback.
		hashes := map[string]string{}
		for _, h := range set.Hosts {
			if h.Plan.Node == node {
				m, e := backup.Verify(h.Archive, keyPath)
				if e != nil {
					return e
				}
				for _, entry := range m.Entries {
					hashes[entry.Path] = entry.SHA256
				}
			}
		}
		for _, name := range []string{"daemon.env", "agent.env"} {
			b, e := fencing.ReadPrivateFile(filepath.Join(host.ConfigRoot, name))
			if e != nil {
				return e
			}
			if hashes["configuration/"+name] != fmt.Sprintf("%x", sha256.Sum256(b)) {
				return fmt.Errorf("host configuration changed since backup: %s", name)
			}
		}
		r = HostReceipt{ID: p.ID, Node: node, Realm: p.Plan.RealmName, ConfigRoot: host.ConfigRoot, StateRoot: host.StateRoot, BackupSetID: p.BackupSetID, TargetCapabilities: Capabilities(target.Capabilities), Phase: "prepared", Files: map[string]FileChange{}}
		for _, name := range []string{"daemon.env", "agent.env"} {
			b, e := fencing.ReadPrivateFile(filepath.Join(host.ConfigRoot, name))
			if e != nil {
				return e
			}
			r.Files[name] = FileChange{Before: b, After: append([]byte(nil), b...), Existed: true}
		}
		agent := r.Files["agent.env"]
		agent.After, e = envValue(agent.Before, "TITANUS_CAPABILITIES", r.TargetCapabilities)
		if e != nil {
			return e
		}
		r.Files["agent.env"] = agent
		if host.Role == "controller" {
			cfg, e := deployConfig(preparedPath, p, host.ConfigRoot)
			if e != nil {
				return e
			}
			daemon := r.Files["daemon.env"]
			daemon.After, e = envValue(daemon.Before, "TITANUS_POWER_FENCING_CONFIG", filepath.Join(host.ConfigRoot, "native-ha", p.ID, "power-fencing.json"))
			if e != nil {
				return e
			}
			r.Files["daemon.env"] = daemon
			for name, b := range cfg {
				r.Files[filepath.Join("native-ha", p.ID, name)] = FileChange{After: b}
			}
		}
		if e = verifyParents(path); e != nil {
			return e
		}
		if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
			return e
		}
		if e = durable.WriteJSON(path, r, 0600); e != nil {
			return e
		}
	} else if e != nil {
		return e
	}
	if e = validReceipt(r); e != nil {
		return e
	}
	if r.ID != p.ID || r.Node != node || r.Phase == "rolled-back" {
		return fmt.Errorf("different or rolled-back transition")
	}
	// Validate every current destination before the first write. A retained
	// partial journal can resume only original or exact staged bytes.
	for name, c := range r.Files {
		file := filepath.Join(host.ConfigRoot, name)
		if e = verifyParents(file); e != nil {
			return e
		}
		if _, e = os.Lstat(file); os.IsNotExist(e) && !c.Existed {
			continue
		}
		if !fileMatches(file, c.After) && !(c.Existed && fileMatches(file, c.Before)) {
			return fmt.Errorf("configuration conflict: %s", name)
		}
	}
	for name, c := range r.Files {
		file := filepath.Join(host.ConfigRoot, name)
		if e = os.MkdirAll(filepath.Dir(file), 0700); e != nil {
			return e
		}
		if e = durable.WriteJSON(filepath.Join(filepath.Dir(path), "progress.json"), map[string]string{"next": name}, 0600); e != nil {
			return e
		}
		if e = writeBytes(file, c.After); e != nil {
			return e
		}
	}
	r.Phase = "staged"
	return durable.WriteJSON(path, r, 0600)
}
func writeBytes(path string, data []byte) error {
	// Open the parent without accepting redirected paths. Existing destination
	// symlinks are refused by the preflight above and durable publication below.
	for dir := filepath.Dir(path); dir != "/"; dir = filepath.Dir(dir) {
		st, e := os.Lstat(dir)
		if e != nil {
			return e
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("redirected configuration parent")
		}
	}
	tmp, e := os.CreateTemp(filepath.Dir(path), ".ha-config-")
	if e != nil {
		return e
	}
	name := tmp.Name()
	defer os.Remove(name)
	if e = tmp.Chmod(0600); e == nil {
		_, e = tmp.Write(data)
	}
	if e == nil {
		e = tmp.Sync()
	}
	closeErr := tmp.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	dir, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func deployConfig(preparedPath string, p Prepared, configRoot string) (map[string][]byte, error) {
	id := p.ID
	root := filepath.Join(filepath.Dir(preparedPath), "fencing")
	out := map[string][]byte{}
	e := filepath.WalkDir(root, func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		name, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		b, e := fencing.ReadPrivateFile(path)
		if e != nil {
			return e
		}
		out[name] = b
		return nil
	})
	if e != nil {
		return nil, e
	}
	if len(out) != len(p.FencingSHA256) {
		return nil, fmt.Errorf("prepared fencing inventory changed")
	}
	for name, data := range out {
		if fmt.Sprintf("%x", sha256.Sum256(data)) != p.FencingSHA256[name] {
			return nil, fmt.Errorf("prepared fencing bytes changed")
		}
	}
	// The prepared archive uses initial-install absolute paths; rebind only
	// credential paths to the immutable transition-specific private directory.
	var targets map[string]fencing.Target
	if e = json.Unmarshal(out["power-fencing.json"], &targets); e != nil {
		return nil, e
	}
	for name, t := range targets {
		for _, field := range []*string{&t.CredentialFile, &t.CAFile} {
			if *field != "" {
				*field = filepath.Join(configRoot, "native-ha", id, strings.TrimPrefix(*field, "/etc/titanus/"))
			}
		}
		targets[name] = t
	}
	out["power-fencing.json"], e = json.Marshal(targets)
	return out, e
}

// RollbackStaging restores preserved configuration bytes only. Activated HA
// cannot be downgraded by editing configuration; use the authenticated complete
// recovery set and independent controller/writer exclusion for that case.
func RollbackStaging(root, id, evidence string) error {
	if len(strings.TrimSpace(evidence)) < 64 {
		return fmt.Errorf("detailed independent pre-activation/quorum exclusion evidence required")
	}
	l, e := offline.Exclusive()
	if e != nil {
		return e
	}
	defer l.Close()
	path, e := receiptPath(root, id)
	if e != nil {
		return e
	}
	r, e := readReceipt(path)
	if e != nil {
		return e
	}
	if e = validReceipt(r); e != nil {
		return e
	}
	if r.StateRoot != root || r.ID != id {
		return fmt.Errorf("rollback host identity differs")
	}
	floor, floorErr := offline.ReadSchemaFloor(root)
	if floorErr != nil && !os.IsNotExist(floorErr) {
		return floorErr
	}
	if floorErr == nil && floor.Schema >= 4 {
		return fmt.Errorf("activation may have begun; retain staged configuration and use authenticated complete cluster recovery")
	}
	if _, e = os.Stat(filepath.Join(root, "realm/consensus/membership.json")); e == nil {
		state, e := consensus.ReadOfflineState(root)
		if e != nil {
			return e
		}
		if state.NativeHA {
			return fmt.Errorf("HA is committed; authenticated cluster recovery is required")
		}
	}
	for name, c := range r.Files {
		if !c.Existed {
			continue
		}
		file := filepath.Join(r.ConfigRoot, name)
		if !fileMatches(file, c.Before) && !fileMatches(file, c.After) {
			return fmt.Errorf("operator configuration conflict: %s", name)
		}
	}
	for name, c := range r.Files {
		if c.Existed {
			if e = writeBytes(filepath.Join(r.ConfigRoot, name), c.Before); e != nil {
				return e
			}
		}
	}
	// New private files remain unreferenced and retained for audit; no removal of
	// state, logs, credentials, membership, storage or data is performed.
	r.Phase = "rolled-back"
	return durable.WriteJSON(path, r, 0600)
}

func verifyParents(path string) error {
	absolute, e := filepath.Abs(path)
	if e != nil {
		return e
	}
	path = absolute
	for dir := filepath.Dir(path); dir != "/"; dir = filepath.Dir(dir) {
		st, e := os.Lstat(dir)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("redirected configuration parent")
		}
	}
	return nil
}
