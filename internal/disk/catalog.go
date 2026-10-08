package disk

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// Backend excludes credentials and local configuration paths. Native object
// identity, rather than a reused Disk name, binds adoption and fencing.
type Backend struct {
	FSID       string `json:"fsid"`
	Pool       string `json:"pool,omitempty"`
	Filesystem string `json:"filesystem,omitempty"`
	Object     string `json:"object"`
}

type Catalog struct {
	Spec         Spec       `json:"spec"`
	Backend      Backend    `json:"backend"`
	Snapshots    []Snapshot `json:"snapshots"`
	LocalNode    string     `json:"local_node,omitempty"`
	Fleet        string     `json:"fleet"`
	AutoFailover bool       `json:"auto_failover"`
}

func (c Catalog) ID() string {
	// Snapshot inventory changes do not change the immutable Disk identity.
	b, _ := json.Marshal(struct {
		Spec    Spec
		Backend Backend
	}{c.Spec, c.Backend})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (c Catalog) Validate() error {
	if !diskName.MatchString(c.Spec.Name) || !diskName.MatchString(c.Fleet) || c.Spec.SizeBytes <= 0 || !c.Spec.Initialized || c.Spec.LayoutVersion != 1 {
		return fmt.Errorf("catalog requires an initialized version-1 Disk and Fleet")
	}
	if c.Spec.Provider == ProviderLocal {
		if !diskName.MatchString(c.LocalNode) || c.AutoFailover || c.Backend != (Backend{}) {
			return fmt.Errorf("local catalog must remain pinned and cannot auto-failover")
		}
	} else if c.Spec.Provider != ProviderCephRBD && c.Spec.Provider != ProviderCephFS {
		return fmt.Errorf("unsupported catalog provider")
	} else if c.LocalNode != "" || c.Backend.FSID == "" || c.Backend.Object == "" {
		return fmt.Errorf("remote catalog requires exact backend identity")
	}
	if c.Spec.Provider == ProviderCephRBD && (c.Backend.Pool == "" || c.Backend.Filesystem != "") {
		return fmt.Errorf("invalid RBD backend")
	}
	if c.Spec.Provider == ProviderCephFS && (c.Backend.Filesystem == "" || c.Backend.Pool != "" || c.Backend.Object != c.Spec.RemotePath || !strings.HasPrefix(c.Spec.RemotePath, "/volumes/")) {
		return fmt.Errorf("invalid CephFS backend")
	}
	seen := map[string]bool{}
	for _, s := range c.Snapshots {
		if !diskName.MatchString(s.Name) || seen[s.Name] || s.Disk != c.Spec.Name || s.Provider != c.Spec.Provider {
			return fmt.Errorf("invalid snapshot inventory")
		}
		seen[s.Name] = true
	}
	return nil
}

func (m *Manager) backend(spec Spec) (Backend, error) {
	if spec.Provider == ProviderLocal {
		return Backend{}, nil
	}
	cfg, err := m.CephConfig()
	if err != nil {
		return Backend{}, err
	}
	fsid, err := commandOutput("ceph", append(m.cephBaseArgs(cfg), "fsid")...)
	if err != nil {
		return Backend{}, err
	}
	b := Backend{FSID: fsid}
	if spec.Provider == ProviderCephRBD {
		out, e := commandOutput("rbd", append(m.rbdBaseArgs(cfg), "info", cfg.Pool+"/"+spec.Name, "--format", "json")...)
		if e != nil {
			return b, e
		}
		var image struct {
			ID       string   `json:"id"`
			Features []string `json:"features"`
		}
		if e = json.Unmarshal([]byte(out), &image); e != nil {
			return b, e
		}
		locked := false
		for _, f := range image.Features {
			if f == "exclusive-lock" {
				locked = true
			}
		}
		if image.ID == "" || !locked {
			return b, fmt.Errorf("catalog requires native exclusive-lock image identity")
		}
		b.Pool = cfg.Pool
		b.Object = image.ID
	} else {
		out, e := commandOutput("ceph", append(m.cephBaseArgs(cfg), "fs", "subvolume", "getpath", cfg.FSName, spec.Name)...)
		if e != nil {
			return b, e
		}
		if strings.TrimSpace(out) != spec.RemotePath {
			return b, fmt.Errorf("subvolume identity mismatch")
		}
		b.Filesystem = cfg.FSName
		b.Object = spec.RemotePath
	}
	return b, nil
}

func (m *Manager) Catalog(name string) (Catalog, error) {
	s, err := m.Inspect(name)
	if err != nil {
		return Catalog{}, err
	}
	// ManagedID is a local realization marker, never part of object identity.
	s.ManagedID = ""
	b, err := m.backend(s)
	if err != nil {
		return Catalog{}, err
	}
	snaps, err := m.Snapshots(name)
	return Catalog{Spec: s, Backend: b, Snapshots: snaps}, err
}

// Adopt publishes only verified metadata for an existing remote object. It
// never creates/formats data or replaces a conflicting local specification.
func (m *Manager) Adopt(c Catalog) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Spec.Provider == ProviderLocal {
		return fmt.Errorf("local data cannot be adopted remotely")
	}
	unlock, err := m.lock(c.Spec.Name)
	if err != nil {
		return err
	}
	defer unlock()
	b, err := m.backend(c.Spec)
	if err != nil {
		return err
	}
	if b != c.Backend {
		return fmt.Errorf("configured backend disagrees with catalog")
	}
	old, err := m.Inspect(c.Spec.Name)
	if err == nil {
		if old.ManagedID == "" && mounted(m.mountPath(c.Spec.Name)) {
			return fmt.Errorf("legacy mount must be detached before catalog adoption")
		}
		old.ManagedID = ""
		if !reflect.DeepEqual(old, c.Spec) {
			return fmt.Errorf("existing Disk metadata conflicts with catalog")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	s := c.Spec
	s.ManagedID = c.ID()
	return writeJSON(m.specPath(s.Name), s, 0600)
}

func (m *Manager) writerPath(name string) string {
	return filepath.Join(m.diskDir(name), "writer.json")
}

func (m *Manager) VerifyCatalog(c Catalog) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := m.backend(c.Spec)
	if err != nil {
		return err
	}
	if b != c.Backend {
		return fmt.Errorf("backend identity mismatch")
	}
	if c.Spec.Provider == ProviderLocal {
		return nil
	}
	cfg, err := m.CephConfig()
	if err != nil {
		return err
	}
	var out string
	if c.Spec.Provider == ProviderCephRBD {
		out, err = commandOutput("rbd", append(m.rbdBaseArgs(cfg), "snap", "ls", cfg.Pool+"/"+c.Spec.Name, "--format", "json")...)
	} else {
		out, err = commandOutput("ceph", append(m.cephBaseArgs(cfg), "fs", "subvolume", "snapshot", "ls", cfg.FSName, c.Spec.Name, "--format", "json")...)
	}
	if err != nil {
		return err
	}
	var snapshots []struct {
		Name string `json:"name"`
	}
	if err = json.Unmarshal([]byte(out), &snapshots); err != nil {
		return err
	}
	names := map[string]bool{}
	for _, s := range snapshots {
		names[s.Name] = true
	}
	if len(names) != len(c.Snapshots) {
		return fmt.Errorf("snapshot inventory differs from backend")
	}
	for _, s := range c.Snapshots {
		if !names[s.Name] {
			return fmt.Errorf("snapshot absent from backend")
		}
	}
	return nil
}
