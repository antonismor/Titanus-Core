package disk

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"time"
)

// ExportOfflineData holds both the native lifecycle guard and the live-attachment
// exclusion across the whole export. RBD diffs cover every extent, including
// zero ranges. CephFS preserves numeric ownership and metadata with cp -a.
// This profile deliberately refuses snapshot histories rather than omit them.
func (m *Manager) ExportOfflineData(c Catalog, destination string) (digest string, err error) {
	if e := c.Validate(); e != nil {
		return "", e
	}
	if c.Spec.Provider == ProviderLocal {
		return "", fmt.Errorf("Ceph catalog required")
	}
	unlock, e := m.lock(c.Spec.Name)
	if e != nil {
		return "", e
	}
	defer unlock()
	if e = m.VerifyCatalog(c); e != nil {
		return "", e
	}
	if len(c.Snapshots) != 0 {
		return "", fmt.Errorf("offline data profile requires no retained snapshots; snapshot-history export is unsupported")
	}
	s, e := m.Inspect(c.Spec.Name)
	if e != nil {
		return "", e
	}
	s.ManagedID = ""
	if !reflect.DeepEqual(s, c.Spec) {
		return "", fmt.Errorf("local and committed Disk specifications differ")
	}
	guard, e := m.guardSpec(s)
	if e != nil {
		return "", e
	}
	defer guard()
	a, e := m.acquireOffline(s.Name, "offline-export")
	if e != nil {
		return "", e
	}
	defer func() {
		if a.File != nil {
			err = errors.Join(err, a.Close())
		}
		err = errors.Join(err, m.detach(s.Name))
	}()
	if s.Provider == ProviderCephFS {
		return "", copyTree(a.Path, destination)
	}
	if e = a.Close(); e != nil {
		return "", e
	}
	a.File = nil
	if e = m.detach(s.Name); e != nil {
		return "", e
	}
	cfg, e := m.CephConfig()
	if e != nil {
		return "", e
	}
	_, e = commandOutput("rbd", append(m.rbdBaseArgs(cfg), "export-diff", "--whole-object", cfg.Pool+"/"+s.Name, destination)...)
	if e != nil {
		return "", e
	}
	return m.rawImageDigest(cfg, s.Name)
}

// ImportOfflineData preserves the exact original backend object identity. It
// cannot provision an unrelated Ceph cluster or replace a reused object name.
// The caller authenticates the entire recovery set and fencing BEFORE invoking.
func (m *Manager) ImportOfflineData(c Catalog, source string) (err error) {
	if e := c.Validate(); e != nil {
		return e
	}
	if c.Spec.Provider == ProviderLocal {
		return fmt.Errorf("Ceph catalog required")
	}
	unlock, e := m.lock(c.Spec.Name)
	if e != nil {
		return e
	}
	defer unlock()
	if e = m.VerifyCatalog(c); e != nil {
		return e
	}
	if len(c.Snapshots) != 0 {
		return fmt.Errorf("snapshot history cannot be overwritten")
	}
	s, e := m.Inspect(c.Spec.Name)
	if e != nil {
		return e
	}
	s.ManagedID = ""
	if !reflect.DeepEqual(s, c.Spec) {
		return fmt.Errorf("local and committed Disk specifications differ")
	}
	guard, e := m.guardSpec(s)
	if e != nil {
		return e
	}
	defer guard()
	a, e := m.acquireOffline(s.Name, "offline-import")
	if e != nil {
		return e
	}
	defer func() {
		if a.File != nil {
			err = errors.Join(err, a.Close())
		}
		err = errors.Join(err, m.detach(s.Name))
	}()
	if s.Provider == ProviderCephFS {
		entries, e := os.ReadDir(a.Path)
		if e != nil {
			return e
		}
		if len(entries) != 0 {
			return fmt.Errorf("CephFS recovery destination must be empty; no overwrite")
		}
		// Restore each child and the data-directory metadata without replacing its
		// inode while an attachment is held. No .titanus-control inode is imported.
		if e = copyTree(source+string(os.PathSeparator)+".", a.Path); e != nil {
			return e
		}
		return syncDirectory(a.Path)
	}
	if e = a.Close(); e != nil {
		return e
	}
	a.File = nil
	if e = m.detach(s.Name); e != nil {
		return e
	}
	cfg, e := m.CephConfig()
	if e != nil {
		return e
	}
	if _, e = commandOutput("rbd", append(m.rbdBaseArgs(cfg), "import-diff", filepath.Clean(source), cfg.Pool+"/"+s.Name)...); e != nil {
		return e
	}
	after, e := m.backend(s)
	if e != nil {
		return e
	}
	if after != c.Backend {
		return fmt.Errorf("Ceph identity changed during restore")
	}
	return nil
}

// WithOfflineData is bounded by native ownership, not a lease timeout.
func (m *Manager) WithOfflineData(c Catalog, check func(string) error) (err error) {
	unlock, e := m.lock(c.Spec.Name)
	if e != nil {
		return e
	}
	defer unlock()
	if e = m.VerifyCatalog(c); e != nil {
		return e
	}
	s, e := m.Inspect(c.Spec.Name)
	if e != nil {
		return e
	}
	s.ManagedID = ""
	if !reflect.DeepEqual(s, c.Spec) {
		return fmt.Errorf("Disk specification mismatch")
	}
	guard, e := m.guardSpec(s)
	if e != nil {
		return e
	}
	defer guard()
	a, e := m.acquireOffline(s.Name, "offline-verify")
	if e != nil {
		return e
	}
	defer func() {
		if a.File != nil {
			err = errors.Join(err, a.Close())
		}
		err = errors.Join(err, m.detach(s.Name))
	}()
	return check(a.Path)
}

func (m *Manager) RawImageDigest(c Catalog) (digest string, err error) {
	if c.Spec.Provider != ProviderCephRBD {
		return "", fmt.Errorf("RBD required")
	}
	unlock, e := m.lock(c.Spec.Name)
	if e != nil {
		return "", e
	}
	defer unlock()
	if e = m.VerifyCatalog(c); e != nil {
		return "", e
	}
	guard, e := m.radosGuard(c.Spec.Name)
	if e != nil {
		return "", e
	}
	defer guard()
	cfg, e := m.CephConfig()
	if e != nil {
		return "", e
	}
	// Verify native exclusion without mounting ext4: mounting would modify its
	// superblock and invalidate a byte-exact restored image verification.
	device, e := commandOutput("rbd", append(m.rbdBaseArgs(cfg), "device", "map", cfg.Pool+"/"+c.Spec.Name, "--exclusive", "--options", "noshare,lock_on_read,lock_timeout=5,mount_timeout=15")...)
	if e != nil {
		return "", e
	}
	defer func() {
		_, cleanup := commandOutput("rbd", append(m.rbdBaseArgs(cfg), "device", "unmap", device)...)
		err = errors.Join(err, cleanup)
	}()
	f, e := os.Open(device)
	if e != nil {
		return "", e
	}
	buffer := make([]byte, 4096)
	_, e = f.Read(buffer)
	f.Close()
	if e != nil {
		return "", e
	}
	return m.rawImageDigest(cfg, c.Spec.Name)
}
func (m *Manager) rawImageDigest(cfg CephConfig, name string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	args := append(m.rbdBaseArgs(cfg), "export", "--export-format", "1", cfg.Pool+"/"+name, "-")
	cmd := exec.CommandContext(ctx, "rbd", args...)
	h := sha256.New()
	cmd.Stdout = h
	if e := cmd.Run(); e != nil {
		return "", fmt.Errorf("raw RBD integrity read: %w", e)
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
