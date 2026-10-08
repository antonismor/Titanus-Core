package disk

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Attachment's read-only, empty lock descriptor is held by namespace PID 1.
// Killing the daemon or host monitor cannot release a live Unit's ownership.
// Applications never inherit the descriptor. CephFS flock is mediated by MDS;
// RBD additionally requires a non-cooperative exclusive kernel mapping.
type Attachment struct {
	Path  string
	File  *os.File
	Owner Owner
}
type Owner struct {
	Unit       string    `json:"unit"`
	Run        string    `json:"run"`
	Boot       string    `json:"boot"`
	Token      string    `json:"token"`
	AcquiredAt time.Time `json:"acquired_at"`
}

func (a *Attachment) Close() error { return a.File.Close() }

func (m *Manager) Acquire(name, unit, run string) (*Attachment, error) {
	unlock, err := m.lock(name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	spec, err := m.Inspect(name)
	if err != nil {
		return nil, err
	}
	guard, err := m.guardSpec(spec)
	if err != nil {
		return nil, err
	}
	defer guard()
	return m.acquire(name, unit, run)
}
func (m *Manager) acquire(name, unit, run string) (*Attachment, error) {
	spec, err := m.Inspect(name)
	if err != nil {
		return nil, err
	}
	if unit == "" || run == "" {
		return nil, fmt.Errorf("attachment requires Unit and incarnation")
	}
	if spec.Provider != ProviderLocal && spec.LayoutVersion != 1 {
		return nil, fmt.Errorf("legacy remote Disk requires offline migration")
	}
	root, err := m.resolve(spec)
	if err != nil {
		return nil, err
	}
	control := filepath.Join(m.diskDir(name), "control")
	data := root
	if spec.Provider != ProviderLocal {
		if spec.LayoutVersion != 1 {
			return nil, fmt.Errorf("legacy remote Disk requires offline migration to data/control layout")
		}
		if spec.Provider == ProviderCephFS {
			control = filepath.Join(root, ".titanus-control")
		}
		data = filepath.Join(root, "data")
	}
	for _, p := range []string{control, data} {
		info, e := os.Lstat(p)
		if os.IsNotExist(e) {
			if e = os.Mkdir(p, 0750); e != nil {
				return nil, e
			}
		} else if e != nil {
			return nil, e
		} else if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("unsafe Disk layout")
		}
	}
	if err = os.Chmod(control, 0700); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(control, "lock")
	// Never replace this inode: renaming a flock file would split ownership.
	fd, err := syscall.Open(lockPath, syscall.O_RDONLY|syscall.O_CREAT|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "titanus-disk-lock")
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("Disk %s is owned by a live attachment: %w", name, err)
	}
	if spec.Provider == ProviderCephFS {
		cfg, e := m.CephConfig()
		if e == nil {
			e = m.waitCephFSBlocklists(cfg, "")
		}
		if e != nil {
			f.Close()
			return nil, e
		}
	}
	var token [24]byte
	if _, err = rand.Read(token[:]); err != nil {
		f.Close()
		return nil, err
	}
	boot, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	a := &Attachment{Path: data, File: f, Owner: Owner{Unit: unit, Run: run, Boot: string(boot), Token: hex.EncodeToString(token[:]), AcquiredAt: time.Now().UTC()}}
	if err = writeJSON(filepath.Join(control, "owner.json"), a.Owner, 0600); err != nil {
		f.Close()
		return nil, err
	}
	return a, nil
}
func (m *Manager) resolve(spec Spec) (string, error) {
	switch spec.Provider {
	case ProviderLocal:
		return m.dataPath(spec.Name), nil
	case ProviderCephRBD:
		return m.attachRBD(spec)
	case ProviderCephFS:
		return m.attachCephFS(spec)
	}
	return "", fmt.Errorf("unsupported provider %q", spec.Provider)
}

// The node-local lock also serializes create/delete/map with acquisition. The
// exported data lock supplies cross-manager/process/CephFS-client exclusion.
func (m *Manager) lock(name string) (func(), error) {
	if !diskName.MatchString(name) {
		return nil, fmt.Errorf("invalid Disk name %q", name)
	}
	dir := filepath.Join(m.storageDir(), "locks")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(filepath.Join(dir, name), syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "disk-operation-lock")
	if err = syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func (m *Manager) ProvisionOwnership(name string, uid, gid int) error {
	if uid < 0 || gid < 0 {
		return fmt.Errorf("invalid ownership")
	}
	unlock, err := m.lock(name)
	if err != nil {
		return err
	}
	defer unlock()
	spec, err := m.Inspect(name)
	if err != nil {
		return err
	}
	guard, err := m.guardSpec(spec)
	if err != nil {
		return err
	}
	defer guard()
	a, err := m.acquire(name, "maintenance", "ownership")
	if err != nil {
		return err
	}
	defer a.Close()
	// Only an empty data root can be provisioned automatically. Existing files
	// require explicit offline migration preserving each relative UID/GID.
	entries, err := os.ReadDir(a.Path)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("populated Disk requires explicit offline ownership migration")
	}
	if err = os.Chown(a.Path, uid, gid); err != nil {
		return err
	}
	return os.Chmod(a.Path, 0750)
}
