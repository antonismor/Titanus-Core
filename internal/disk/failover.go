package disk

import (
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// Writer binds the exact native client instance to a verified remote object.
// A diagnostic Owner, PID or scheduling deadline is never a Writer fence.
type Writer struct {
	Disk      string `json:"disk"`
	CatalogID string `json:"catalog_id"`
	Address   string `json:"address"`
	Session   uint64 `json:"session,omitempty"`
}

func validInstance(a string) bool {
	a = blocklistAddress(a)
	i := strings.LastIndex(a, "/")
	if i < 0 {
		return false
	}
	n, e := strconv.ParseUint(a[i+1:], 10, 64)
	if e != nil || n == 0 {
		return false
	}
	h, p, e := net.SplitHostPort(a[:i])
	if e != nil || net.ParseIP(h) == nil {
		return false
	}
	port, e := strconv.ParseUint(p, 10, 16)
	return e == nil && port <= 65535
}

func (m *Manager) writers(spec Spec) ([]Writer, error) {
	cfg, err := m.CephConfig()
	if err != nil {
		return nil, err
	}
	result := []Writer{}
	if spec.Provider == ProviderCephFS {
		if err = m.verifyCephFSFencing(cfg); err != nil {
			return nil, err
		}
		out, e := commandOutput("ceph", append(m.cephBaseArgs(cfg), "tell", "mds."+cfg.FSName+":0", "client", "ls", "--format", "json")...)
		if e != nil {
			return nil, e
		}
		var sessions []cephSession
		if e = json.Unmarshal([]byte(out), &sessions); e != nil || sessions == nil {
			return nil, fmt.Errorf("invalid session evidence: %v", e)
		}
		for _, s := range sessions {
			if s.Metadata.Root != spec.RemotePath {
				continue
			}
			prefix := "client." + strconv.FormatUint(s.ID, 10) + " "
			if !strings.HasPrefix(s.Inst, prefix) || s.ID == 0 {
				return nil, fmt.Errorf("invalid native session")
			}
			result = append(result, Writer{Disk: spec.Name, CatalogID: spec.ManagedID, Address: strings.TrimPrefix(s.Inst, prefix), Session: s.ID})
		}
	} else if spec.Provider == ProviderCephRBD {
		out, e := commandOutput("rbd", append(m.rbdBaseArgs(cfg), "status", cfg.Pool+"/"+spec.Name, "--format", "json")...)
		if e != nil {
			return nil, e
		}
		var status struct {
			Watchers []struct {
				Address string `json:"address"`
			} `json:"watchers"`
		}
		if e = json.Unmarshal([]byte(out), &status); e != nil || status.Watchers == nil {
			return nil, fmt.Errorf("invalid RBD watcher evidence: %v", e)
		}
		for _, w := range status.Watchers {
			result = append(result, Writer{Disk: spec.Name, CatalogID: spec.ManagedID, Address: w.Address})
		}
	} else {
		return nil, fmt.Errorf("local Disk has no remote writer identity")
	}
	for _, w := range result {
		if !validInstance(w.Address) {
			return nil, fmt.Errorf("writer is missing exact instance address")
		}
	}
	return result, nil
}

// captureWriter identifies a newly established native mount by the before/after
// instance set. A reused mount must retain its previously fsynced exact identity.
// Ambiguity fails before any filesystem is exported to a workload.
func (m *Manager) captureWriter(spec Spec, before []Writer, reused bool) error {
	after, err := m.writers(spec)
	if err != nil {
		return err
	}
	if reused {
		w, err := m.Writer(spec.Name)
		if err != nil {
			return err
		}
		for _, current := range after {
			if current == w {
				return nil
			}
		}
		return fmt.Errorf("persisted native writer identity is no longer active")
	}
	candidates := []Writer{}
	for _, w := range after {
		found := false
		for _, old := range before {
			if w == old {
				found = true
			}
		}
		if !found {
			candidates = append(candidates, w)
		}
	}
	if len(candidates) != 1 {
		return fmt.Errorf("cannot identify one new native client instance")
	}
	return writeJSON(m.writerPath(spec.Name), candidates[0], 0600)
}

func (m *Manager) Writer(name string) (Writer, error) {
	s, err := m.Inspect(name)
	if err != nil {
		return Writer{}, err
	}
	var w Writer
	if err = readJSON(m.writerPath(name), &w); err != nil {
		return w, err
	}
	if w.Disk != name || s.ManagedID == "" || w.CatalogID != s.ManagedID || !validInstance(w.Address) {
		return w, fmt.Errorf("unbound writer identity")
	}
	return w, nil
}

// Fence is idempotent for a committed exact Writer. It never discovers or
// guesses a new victim after a controller restart. Persist the intent before
// calling this method and gate each native mutation on current quorum leadership.
func (m *Manager) Fence(c Catalog, w Writer, check func() error) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !c.AutoFailover || c.Spec.Provider == ProviderLocal || w.Disk != c.Spec.Name || w.CatalogID != c.ID() || !validInstance(w.Address) || check == nil {
		return fmt.Errorf("fence requires bound remote writer and quorum gate")
	}
	if c.Spec.Provider == ProviderCephFS && w.Session == 0 {
		return fmt.Errorf("missing native session")
	}
	if err := check(); err != nil {
		return err
	}
	b, err := m.backend(c.Spec)
	if err != nil {
		return err
	}
	if b != c.Backend {
		return fmt.Errorf("native backend identity changed")
	}
	spec := c.Spec
	spec.ManagedID = c.ID()
	current, err := m.writers(spec)
	if err != nil {
		return err
	}
	for _, other := range current {
		if other != w {
			return fmt.Errorf("unrecorded native client prevents automatic fencing")
		}
	}
	cfg, err := m.CephConfig()
	if err != nil {
		return err
	}
	// Ceph utime_t stores unsigned 32-bit seconds. An unchecked 100-year
	// duration exceeds its range and depends on a build's saturation behavior.
	// Keep a bounded 10-year fence, renewed from committed intents every day.
	expiry, err := quarantineSeconds(time.Now())
	if err != nil {
		return err
	}
	if err = check(); err != nil {
		return err
	}
	if _, err = commandOutput("ceph", append(m.cephBaseArgs(cfg), "osd", "blocklist", "add", w.Address, expiry)...); err != nil {
		return err
	}
	if err = m.waitCephFSBlocklists(cfg, w.Address); err != nil {
		return err
	}
	if spec.Provider == ProviderCephFS && len(current) == 1 {
		if err = check(); err != nil {
			return err
		}
		if _, err = commandOutput("ceph", append(m.cephBaseArgs(cfg), "tell", "mds."+cfg.FSName+":0", "client", "evict", "id="+strconv.FormatUint(w.Session, 10))...); err != nil {
			return err
		}
		// Eviction may install the default shorter expiry; restore the durable
		// quarantine interval and wait for its application again.
		if err = check(); err != nil {
			return err
		}
		if _, err = commandOutput("ceph", append(m.cephBaseArgs(cfg), "osd", "blocklist", "add", w.Address, expiry)...); err != nil {
			return err
		}
	}
	if err = m.waitCephFSBlocklists(cfg, w.Address); err != nil {
		return err
	}
	return check()
}

func quarantineSeconds(now time.Time) (string, error) {
	const seconds = 315360000
	if now.Unix() < 0 || now.Unix() > int64(^uint32(0))-seconds {
		return "", fmt.Errorf("Ceph quarantine expiry exceeds native timestamp range")
	}
	return strconv.FormatInt(seconds, 10), nil
}

// RefreshFence reasserts only an immutable committed old instance. It does
// not evict or discover any current/new writer on the same remote object.
func (m *Manager) RefreshFence(c Catalog, w Writer, check func() error) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if !c.AutoFailover || c.Spec.Provider == ProviderLocal || ValidateWriter(c, w) != nil || check == nil {
		return fmt.Errorf("refresh requires committed bound writer")
	}
	if err := check(); err != nil {
		return err
	}
	backend, err := m.backend(c.Spec)
	if err != nil {
		return err
	}
	if backend != c.Backend {
		return fmt.Errorf("fenced object identity changed")
	}
	cfg, err := m.CephConfig()
	if err != nil {
		return err
	}
	expiry, err := quarantineSeconds(time.Now())
	if err != nil {
		return err
	}
	if err = check(); err != nil {
		return err
	}
	if _, err = commandOutput("ceph", append(m.cephBaseArgs(cfg), "osd", "blocklist", "add", w.Address, expiry)...); err != nil {
		return err
	}
	if err = m.waitCephFSBlocklists(cfg, w.Address); err != nil {
		return err
	}
	return check()
}

func ValidateWriter(c Catalog, w Writer) error {
	if w.Disk != c.Spec.Name || w.CatalogID != c.ID() || !validInstance(w.Address) || (c.Spec.Provider == ProviderCephFS && w.Session == 0) {
		return fmt.Errorf("writer must bind native instance to catalog")
	}
	return nil
}
