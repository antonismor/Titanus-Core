package backup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/antonismor/Titanus-Core/internal/consensus"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/version"
)

// ClusterBinding is inside each authenticated host manifest. A host archive
// with this binding cannot pass the standalone restore path.
type ClusterBinding struct {
	ID            string                  `json:"id"`
	RealmSHA256   string                  `json:"realm_sha256"`
	Revision      uint64                  `json:"revision"`
	Peers         []consensus.Peer        `json:"peers"`
	Catalogs      map[string]disk.Catalog `json:"catalogs"`
	StorageSHA256 string                  `json:"storage_sha256"`
}

func (b ClusterBinding) validate() error {
	if !hashPattern.MatchString(b.ID) || !hashPattern.MatchString(b.RealmSHA256) || b.Revision == 0 {
		return fmt.Errorf("invalid cluster binding")
	}
	if b.StorageSHA256 != "" && !hashPattern.MatchString(b.StorageSHA256) {
		return fmt.Errorf("invalid storage binding")
	}
	c := consensus.Config{ID: b.PeersID(), Realm: "recovery", Peers: b.Peers}
	if e := c.Validate(); e != nil {
		return e
	}
	if len(b.Catalogs) > 128 {
		return fmt.Errorf("recovery Disk limit")
	}
	for name, c := range b.Catalogs {
		if e := c.Validate(); e != nil {
			return e
		}
		if name != c.Spec.Name || c.Spec.Provider == disk.ProviderLocal {
			return fmt.Errorf("invalid external catalog")
		}
	}
	return nil
}
func (b ClusterBinding) PeersID() string {
	if len(b.Peers) == 0 {
		return ""
	}
	return b.Peers[0].ID
}
func stateDigest(s realm.State) string {
	data, _ := json.Marshal(s)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
func (b ClusterBinding) checkState(s realm.State) error {
	if stateDigest(s) != b.RealmSHA256 || s.Revision != b.Revision {
		return fmt.Errorf("mixed/stale Realm checkpoint")
	}
	catalogs := map[string]disk.Catalog{}
	for n, c := range s.Disks {
		if c.Spec.Provider != disk.ProviderLocal {
			catalogs[n] = c
		}
	}
	if !reflect.DeepEqual(catalogs, b.Catalogs) {
		return fmt.Errorf("incomplete storage catalogs")
	}
	return nil
}

type ClusterIntent struct {
	Format  string         `json:"format"`
	Realm   string         `json:"realm"`
	Binding ClusterBinding `json:"binding"`
	State   realm.State    `json:"state"`
	Hosts   []Plan         `json:"hosts"`
	Build   version.Build  `json:"build"`
}
type ClusterHost struct {
	Plan     Plan   `json:"plan"`
	Archive  string `json:"archive"`
	BackupID string `json:"backup_id,omitempty"`
	SHA256   string `json:"sha256,omitempty"`
}
type ClusterSetPlan struct {
	Intent  string        `json:"intent"`
	Storage string        `json:"storage"`
	Hosts   []ClusterHost `json:"hosts"`
}
type ClusterSet struct {
	Format        string        `json:"format"`
	Intent        ClusterIntent `json:"intent"`
	Storage       string        `json:"storage"`
	StorageSHA256 string        `json:"storage_sha256"`
	Hosts         []ClusterHost `json:"hosts"`
}

func readSigned(path, keyPath string, value any) error {
	k, e := loadKey(keyPath)
	if e != nil {
		return e
	}
	defer clear(k)
	f, e := openRegular(path)
	if e != nil {
		return e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || st.Size() > MaxManifest {
		return fmt.Errorf("signed recovery document exceeds limit")
	}
	data, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return authenticate(k, data, value)
}
func writeSigned(path, keyPath string, value any) error {
	k, e := loadKey(keyPath)
	if e != nil {
		return e
	}
	defer clear(k)
	data, e := sign(k, value)
	if e != nil {
		return e
	}
	if len(data) > MaxManifest {
		return fmt.Errorf("recovery document limit")
	}
	return createPrivate(path, data)
}
func loadDocument(path string, v any) error {
	f, e := openRegular(path)
	if e != nil {
		return e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || st.Size() > MaxManifest {
		return fmt.Errorf("document limit")
	}
	data, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	return strictJSON(data, v)
}
func (i ClusterIntent) validate() error {
	if i.Format != "titanus-cluster-intent/v1" || i.Realm != i.State.Name || i.Build.Version != version.Version || i.Build.StateProfile != version.StateProfile {
		return fmt.Errorf("unsupported cluster intent")
	}
	if e := i.Binding.validate(); e != nil {
		return e
	}
	if e := i.Binding.checkState(i.State); e != nil {
		return e
	}
	if len(i.Hosts) < 3 || len(i.Hosts) > 128 {
		return fmt.Errorf("bounded complete host set required")
	}
	seen := map[string]bool{}
	controllers := map[string]bool{}
	for _, p := range i.Hosts {
		if e := p.Validate(); e != nil {
			return e
		}
		if p.Realm != i.Realm || seen[p.Node] {
			return fmt.Errorf("duplicate/foreign host")
		}
		seen[p.Node] = true
		if p.Role == "controller" {
			controllers[p.Node] = true
		}
	}
	if len(controllers) != len(i.Binding.Peers) {
		return fmt.Errorf("all original voters required")
	}
	for _, p := range i.Binding.Peers {
		if !controllers[p.ID] {
			return fmt.Errorf("missing original voter")
		}
	}
	for id := range i.State.Nodes {
		if !seen[id] {
			return fmt.Errorf("missing Realm node %s", id)
		}
	}
	// Restoring in-flight executions may replay side effects. Quiesce first;
	// unknown Task executions stay an explicit operator decision, never replayed.
	for _, a := range i.State.Assignments {
		if a.State != realm.AssignmentStopped {
			return fmt.Errorf("stop every assignment before recovery-set backup")
		}
	}
	for _, a := range i.State.Tasks {
		if !a.Terminal() {
			return fmt.Errorf("Task execution is not quiescent")
		}
	}
	for _, c := range i.Binding.Catalogs {
		if len(c.Snapshots) != 0 {
			return fmt.Errorf("snapshot-history backup requires another profile")
		}
	}
	return nil
}

func CreateClusterIntent(controller Plan, hostsPath, keyPath, output string) (ClusterIntent, error) {
	var i ClusterIntent
	l, e := offline.Exclusive()
	if e != nil {
		return i, e
	}
	defer l.Close()
	if e = rejectRunningActors(); e != nil {
		return i, e
	}
	if controller.Role != "controller" {
		return i, fmt.Errorf("offline controller required")
	}
	state, e := consensus.ReadOfflineState(controller.StateRoot)
	if e != nil {
		return i, e
	}
	cfg, e := consensus.LoadConfig(filepath.Join(controller.ConfigRoot, "ha.json"))
	if e != nil {
		return i, e
	}
	var hosts []Plan
	if e = loadDocument(hostsPath, &hosts); e != nil {
		return i, e
	}
	token := make([]byte, 32)
	if _, e = rand.Read(token); e != nil {
		return i, e
	}
	catalogs := map[string]disk.Catalog{}
	for n, c := range state.Disks {
		if c.Spec.Provider != disk.ProviderLocal {
			catalogs[n] = c
		}
	}
	i = ClusterIntent{"titanus-cluster-intent/v1", state.Name, ClusterBinding{hex.EncodeToString(token), stateDigest(state), state.Revision, cfg.Peers, catalogs, ""}, state, hosts, version.Info()}
	if e = i.validate(); e != nil {
		return i, e
	}
	if _, e = validateHostBound(controller, controller.roots(), &i.Binding); e != nil {
		return i, e
	}
	found := false
	for _, p := range hosts {
		if p == controller {
			found = true
		}
	}
	if !found {
		return i, fmt.Errorf("source controller absent from planned hosts")
	}
	return i, writeSigned(output, keyPath, i)
}

func CreateClusterHost(p Plan, archive, keyPath, intentPath, storagePath string) (Manifest, error) {
	l, e := offline.Exclusive()
	if e != nil {
		return Manifest{}, e
	}
	defer l.Close()
	if e = rejectRunningActors(); e != nil {
		return Manifest{}, e
	}
	var i ClusterIntent
	if e = readSigned(intentPath, keyPath, &i); e != nil {
		return Manifest{}, e
	}
	if e = i.validate(); e != nil {
		return Manifest{}, e
	}
	found := false
	for _, h := range i.Hosts {
		if p == h {
			found = true
		}
	}
	if !found {
		return Manifest{}, fmt.Errorf("host absent from signed intent")
	}
	data, e := VerifyStorage(storagePath, keyPath)
	if e != nil {
		return Manifest{}, e
	}
	if !reflect.DeepEqual(data.Binding, i.Binding) {
		return Manifest{}, fmt.Errorf("storage belongs to another recovery intent")
	}
	b := i.Binding
	b.StorageSHA256, e = digestFile(filepath.Join(storagePath, "manifest.json"))
	if e != nil {
		return Manifest{}, e
	}
	if p.Role == "controller" {
		cfg, e := consensus.LoadConfig(filepath.Join(p.ConfigRoot, "ha.json"))
		if e != nil {
			return Manifest{}, e
		}
		if !reflect.DeepEqual(cfg.Peers, b.Peers) {
			return Manifest{}, fmt.Errorf("voter membership differs from recovery set")
		}
	}
	return createBound(p, archive, keyPath, &b)
}

func SealClusterSet(planPath, keyPath, output string) (ClusterSet, error) {
	var set ClusterSet
	var p ClusterSetPlan
	if e := loadDocument(planPath, &p); e != nil {
		return set, e
	}
	if e := readSigned(p.Intent, keyPath, &set.Intent); e != nil {
		return set, e
	}
	if e := set.Intent.validate(); e != nil {
		return set, e
	}
	set.Format = "titanus-cluster-set/v1"
	set.Storage = p.Storage
	set.Hosts = p.Hosts
	var e error
	set.StorageSHA256, e = digestFile(filepath.Join(set.Storage, "manifest.json"))
	if e != nil {
		return set, e
	}
	if e = verifyClusterSet(set, keyPath); e != nil {
		return set, e
	}
	if e = set.bindHosts(keyPath); e != nil {
		return set, e
	}
	return set, writeSigned(output, keyPath, set)
}
func VerifyClusterSet(path, keyPath string) (ClusterSet, error) {
	var set ClusterSet
	if e := readSigned(path, keyPath, &set); e != nil {
		return set, e
	}
	return set, verifyClusterSet(set, keyPath)
}
func verifyClusterSet(set ClusterSet, keyPath string) error {
	if set.Format != "titanus-cluster-set/v1" {
		return fmt.Errorf("unsupported recovery set")
	}
	if e := set.Intent.validate(); e != nil {
		return e
	}
	data, e := VerifyStorage(set.Storage, keyPath)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(data.Binding, set.Intent.Binding) {
		return fmt.Errorf("mixed storage/Realm backup")
	}
	digest, e := digestFile(filepath.Join(set.Storage, "manifest.json"))
	if e != nil || digest != set.StorageSHA256 {
		return fmt.Errorf("changed storage manifest")
	}
	if len(set.Hosts) != len(set.Intent.Hosts) {
		return fmt.Errorf("incomplete host backup set")
	}
	expected := map[string]Plan{}
	for _, p := range set.Intent.Hosts {
		expected[p.Node] = p
	}
	seen := map[string]bool{}
	archives := map[string]bool{}
	b := set.Intent.Binding
	b.StorageSHA256 = digest
	for _, h := range set.Hosts {
		if expected[h.Plan.Node] != h.Plan || seen[h.Plan.Node] || archives[h.Archive] {
			return fmt.Errorf("duplicate/conflicting/missing recovery host")
		}
		seen[h.Plan.Node] = true
		archives[h.Archive] = true
		m, e := Verify(h.Archive, keyPath)
		if e != nil {
			return e
		}
		if m.Plan != h.Plan || !reflect.DeepEqual(m.Cluster, &b) || m.RealmSHA256 != b.RealmSHA256 || m.RealmRevision != b.Revision {
			return fmt.Errorf("mixed host checkpoint")
		}
		got, e := digestFile(h.Archive)
		if e != nil {
			return e
		}
		if h.BackupID != "" && h.BackupID != m.ID || h.SHA256 != "" && h.SHA256 != got {
			return fmt.Errorf("changed host archive")
		}
	}
	return nil
}

// Populate immutable archive identities before publication of a new set.
func (s *ClusterSet) bindHosts(keyPath string) error {
	for n := range s.Hosts {
		m, e := Verify(s.Hosts[n].Archive, keyPath)
		if e != nil {
			return e
		}
		s.Hosts[n].BackupID = m.ID
		s.Hosts[n].SHA256, e = digestFile(s.Hosts[n].Archive)
		if e != nil {
			return e
		}
	}
	return nil
}
func checkHostRestoreStartup(root string, binding *ClusterBinding) error {
	if binding == nil {
		return offline.CheckStartup(root)
	}
	for _, suffix := range []string{".recovery-pending", ".backup-pending"} {
		if _, e := os.Lstat(root + suffix); !os.IsNotExist(e) {
			return fmt.Errorf("unfinished host operation")
		}
	}
	data, e := os.ReadFile(root + ".cluster-recovery-pending")
	if e != nil || string(data) != binding.ID {
		return fmt.Errorf("matching cluster startup blocker required")
	}
	return nil
}
func RestoreClusterHost(setPath, keyPath, node, fence string) (Manifest, error) {
	l, e := offline.Exclusive()
	if e != nil {
		return Manifest{}, e
	}
	defer l.Close()
	if e = rejectRunningActors(); e != nil {
		return Manifest{}, e
	}
	set, e := VerifyClusterSet(setPath, keyPath)
	if e != nil {
		return Manifest{}, e
	}
	for _, h := range set.Hosts {
		if h.Plan.Node == node {
			if e = offline.CheckStartup(h.Plan.StateRoot); e != nil {
				return Manifest{}, e
			}
			if e = createPrivate(h.Plan.StateRoot+".cluster-recovery-pending", []byte(set.Intent.Binding.ID)); e != nil {
				return Manifest{}, e
			}
			b := set.Intent.Binding
			b.StorageSHA256 = set.StorageSHA256
			return restoreBound(h.Archive, keyPath, fence, h.Plan, nil, &b)
		}
	}
	return Manifest{}, fmt.Errorf("unknown recovery host")
}

type RecoveryProof struct {
	Format      string    `json:"format"`
	SetSHA256   string    `json:"set_sha256"`
	ID          string    `json:"id"`
	Node        string    `json:"node"`
	StateSHA256 string    `json:"state_sha256"`
	Storage     bool      `json:"storage"`
	CreatedAt   time.Time `json:"created_at"`
}
type RecoveryCompletion struct {
	Format    string          `json:"format"`
	SetSHA256 string          `json:"set_sha256"`
	ID        string          `json:"id"`
	Proofs    []RecoveryProof `json:"proofs"`
}

func ProveClusterHost(setPath, keyPath, node, output string) error {
	l, e := offline.Exclusive()
	if e != nil {
		return e
	}
	defer l.Close()
	if e = rejectRunningActors(); e != nil {
		return e
	}
	set, e := VerifyClusterSet(setPath, keyPath)
	if e != nil {
		return e
	}
	for _, h := range set.Hosts {
		if h.Plan.Node == node {
			b := set.Intent.Binding
			b.StorageSHA256 = set.StorageSHA256
			data, e := os.ReadFile(h.Plan.StateRoot + ".cluster-recovery-pending")
			if e != nil || string(data) != b.ID {
				return fmt.Errorf("cluster blocker missing")
			}
			var receipt struct {
				Format        string   `json:"format"`
				BackupID      string   `json:"backup_id"`
				ArchiveSHA256 string   `json:"archive_sha256"`
				Status        string   `json:"status"`
				RenamedRoots  []string `json:"renamed_roots"`
				Fence         Fence    `json:"fence"`
			}
			if e = loadDocument(h.Plan.StateRoot+".recovery-receipt.json", &receipt); e != nil {
				return e
			}
			if receipt.Status != "complete" || receipt.BackupID != h.BackupID || receipt.ArchiveSHA256 != h.SHA256 {
				return fmt.Errorf("host restore not complete")
			}
			s, e := validateHostBound(h.Plan, h.Plan.roots(), &b)
			if e != nil {
				return e
			}
			if e = b.checkState(s); e != nil {
				return e
			}
			m, e := Verify(h.Archive, keyPath)
			if e != nil {
				return e
			}
			if e = verifyInstalledEntries(m); e != nil {
				return e
			}
			sha, e := digestFile(setPath)
			if e != nil {
				return e
			}
			return writeSigned(output, keyPath, RecoveryProof{"titanus-recovery-proof/v1", sha, b.ID, node, stateDigest(s), false, time.Now().UTC()})
		}
	}
	return fmt.Errorf("unknown recovery host")
}
func verifyInstalledEntries(m Manifest) error {
	actual, e := inventory(m.Plan)
	if e != nil {
		return e
	}
	want := append([]Entry(nil), m.Entries...)
	for n := range actual {
		actual[n].MTimeNS = 0
	}
	for n := range want {
		want[n].MTimeNS = 0
	}
	if !reflect.DeepEqual(actual, want) {
		return fmt.Errorf("restored inventory changed/contains extra paths")
	}

	for _, v := range m.Entries {
		p := entrySource(m.Plan, v.Path)
		st, e := os.Lstat(p)
		if e != nil {
			return e
		}
		// Metadata timestamps can change while fsyncing directories; numeric owner,
		// permissions, type, link target and every file byte remain authoritative.
		h, e := entryInstalled(p, v)
		if e != nil {
			return e
		}
		_ = st
		if h != v.SHA256 && v.Type == "file" {
			return fmt.Errorf("restored byte mismatch")
		}
	}
	return nil
}
func CompleteClusterRecovery(setPath, keyPath, proofPaths, output string) error {
	set, e := VerifyClusterSet(setPath, keyPath)
	if e != nil {
		return e
	}
	var paths []string
	if e = loadDocument(proofPaths, &paths); e != nil {
		return e
	}
	sha, e := digestFile(setPath)
	if e != nil {
		return e
	}
	seen := map[string]bool{}
	proofs := []RecoveryProof{}
	storage := false
	if len(paths) != len(set.Hosts)+1 {
		return fmt.Errorf("every host plus storage proof required")
	}
	for _, p := range paths {
		var proof RecoveryProof
		if e = readSigned(p, keyPath, &proof); e != nil {
			return e
		}
		if proof.Format != "titanus-recovery-proof/v1" || proof.SetSHA256 != sha || proof.ID != set.Intent.Binding.ID || proof.StateSHA256 != set.Intent.Binding.RealmSHA256 || proof.CreatedAt.IsZero() {
			return fmt.Errorf("mixed recovery proof")
		}
		if proof.Storage {
			if storage || proof.Node != "storage" {
				return fmt.Errorf("duplicate storage proof")
			}
			storage = true
		} else {
			known := false
			for _, h := range set.Hosts {
				if h.Plan.Node == proof.Node {
					known = true
				}
			}
			if !known || seen[proof.Node] {
				return fmt.Errorf("unknown/duplicate host proof")
			}
			seen[proof.Node] = true
		}
		proofs = append(proofs, proof)
	}
	if !storage || len(seen) != len(set.Hosts) {
		return fmt.Errorf("incomplete recovered cluster")
	}
	sort.Slice(proofs, func(i, j int) bool { return proofs[i].Node < proofs[j].Node })
	return writeSigned(output, keyPath, RecoveryCompletion{"titanus-recovery-completion/v1", sha, set.Intent.Binding.ID, proofs})
}
func FinalizeClusterHost(setPath, keyPath, node, completionPath string) error {
	l, e := offline.Exclusive()
	if e != nil {
		return e
	}
	defer l.Close()
	if e = rejectRunningActors(); e != nil {
		return e
	}
	set, e := VerifyClusterSet(setPath, keyPath)
	if e != nil {
		return e
	}
	sha, e := digestFile(setPath)
	if e != nil {
		return e
	}
	var c RecoveryCompletion
	if e = readSigned(completionPath, keyPath, &c); e != nil {
		return e
	}
	if c.Format != "titanus-recovery-completion/v1" || c.SetSHA256 != sha || c.ID != set.Intent.Binding.ID || len(c.Proofs) != len(set.Hosts)+1 {
		return fmt.Errorf("complete matching cluster receipt required")
	}
	for _, h := range set.Hosts {
		if h.Plan.Node == node {
			b := set.Intent.Binding
			b.StorageSHA256 = set.StorageSHA256
			s, e := validateHostBound(h.Plan, h.Plan.roots(), &b)
			if e != nil {
				return e
			}
			if e = b.checkState(s); e != nil {
				return e
			}
			m, e := Verify(h.Archive, keyPath)
			if e != nil {
				return e
			}
			if e = verifyInstalledEntries(m); e != nil {
				return e
			}
			data, e := os.ReadFile(h.Plan.StateRoot + ".cluster-recovery-pending")
			if e != nil || string(data) != b.ID {
				return fmt.Errorf("wrong cluster recovery blocker")
			}
			if e = createPrivate(h.Plan.StateRoot+".cluster-recovery-complete.json", mustJSON(c)); e != nil {
				return e
			}
			if e = os.Remove(h.Plan.StateRoot + ".recovery-pending"); e != nil {
				return e
			}
			if e = os.Remove(h.Plan.StateRoot + ".cluster-recovery-pending"); e != nil {
				return e
			}
			return syncDir(filepath.Dir(h.Plan.StateRoot))
		}
	}
	return fmt.Errorf("unknown recovery host")
}
func mustJSON(v any) []byte { data, _ := json.Marshal(v); return data }
