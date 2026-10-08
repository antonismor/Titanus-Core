package disk

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Snapshot struct {
	Name      string    `json:"name"`
	Disk      string    `json:"disk"`
	Provider  Provider  `json:"provider"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// Snapshot requires exclusive offline maintenance ownership. No live writer,
// including a Unit whose daemon crashed, can race the data copy.
func (m *Manager) Snapshot(name, snap string) (Snapshot, error) {
	if !diskName.MatchString(snap) {
		return Snapshot{}, fmt.Errorf("invalid snapshot name")
	}
	unlock, err := m.lock(name)
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	a, err := m.acquire(name, "maintenance", "snapshot")
	if err != nil {
		return Snapshot{}, err
	}
	defer a.Close()
	spec, err := m.Inspect(name)
	if err != nil {
		return Snapshot{}, err
	}
	result := Snapshot{Name: snap, Disk: name, Provider: spec.Provider, CreatedAt: time.Now().UTC()}
	switch spec.Provider {
	case ProviderLocal:
		dir := filepath.Join(m.diskDir(name), "snapshots", snap)
		if _, err = os.Lstat(dir); !os.IsNotExist(err) {
			return Snapshot{}, fmt.Errorf("snapshot already exists or cannot be inspected")
		}
		if err = os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
			return Snapshot{}, err
		}
		tmp, err := os.MkdirTemp(filepath.Dir(dir), ".snapshot-")
		if err != nil {
			return Snapshot{}, err
		}
		defer os.RemoveAll(tmp)
		if err = copyTree(a.Path, filepath.Join(tmp, "data")); err != nil {
			return Snapshot{}, err
		}
		if err = writeJSON(filepath.Join(tmp, "snapshot.json"), result, 0600); err != nil {
			return Snapshot{}, err
		}
		if err = os.Rename(tmp, dir); err != nil {
			return Snapshot{}, err
		}
		if err = syncDirectory(filepath.Dir(dir)); err != nil {
			return Snapshot{}, err
		}
	case ProviderCephRBD:
		// Flush and unmount the filesystem before recording the native RBD snapshot.
		// The exclusive mapping remains present; other kernel writers cannot attach.
		if err = m.unmountOnly(name); err != nil {
			return Snapshot{}, err
		}
		cfg, e := m.CephConfig()
		if e != nil {
			return Snapshot{}, e
		}
		if _, err = commandOutput("rbd", append(m.rbdBaseArgs(cfg), "snap", "create", cfg.Pool+"/"+name+"@"+snap)...); err != nil {
			return Snapshot{}, fmt.Errorf("RBD snapshot: %w", err)
		}
	case ProviderCephFS:
		cfg, e := m.CephConfig()
		if e != nil {
			return Snapshot{}, e
		}
		if _, err = commandOutput("ceph", append(m.cephBaseArgs(cfg), "fs", "subvolume", "snapshot", "create", cfg.FSName, name, snap)...); err != nil {
			return Snapshot{}, fmt.Errorf("CephFS snapshot: %w", err)
		}
	}
	return result, nil
}

// Restore publishes a new, independent Disk; it never overwrites the source.
// A failed or pending clone is not published as a usable Disk.
func (m *Manager) Restore(name, snap, target string) (Spec, error) {
	if !diskName.MatchString(snap) || !diskName.MatchString(target) || target == name {
		return Spec{}, fmt.Errorf("invalid restore names")
	}
	// Canonical ordering avoids deadlocks between reciprocal restores.
	names := []string{name, target}
	sort.Strings(names)
	first, err := m.lock(names[0])
	if err != nil {
		return Spec{}, err
	}
	defer first()
	second, err := m.lock(names[1])
	if err != nil {
		return Spec{}, err
	}
	defer second()
	spec, err := m.Inspect(name)
	if err != nil {
		return Spec{}, err
	}
	if _, err = os.Lstat(m.diskDir(target)); !os.IsNotExist(err) {
		return Spec{}, fmt.Errorf("restore target already exists or cannot be inspected")
	}
	if err = os.Mkdir(m.diskDir(target), 0750); err != nil {
		return Spec{}, err
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(m.diskDir(target))
		}
	}()
	restored := spec
	restored.Name = target
	restored.CreatedAt = time.Now().UTC()
	restored.LayoutVersion = 1
	switch spec.Provider {
	case ProviderLocal:
		var metadata Snapshot
		dir := filepath.Join(m.diskDir(name), "snapshots", snap)
		if err = readJSON(filepath.Join(dir, "snapshot.json"), &metadata); err != nil {
			return Spec{}, err
		}
		if metadata.Disk != name || metadata.Name != snap || metadata.Provider != spec.Provider {
			return Spec{}, fmt.Errorf("snapshot manifest mismatch")
		}
		if err = copyTree(filepath.Join(dir, "data"), m.dataPath(target)); err != nil {
			return Spec{}, err
		}
	case ProviderCephRBD:
		cfg, e := m.CephConfig()
		if e != nil {
			return Spec{}, e
		}
		if _, err = commandOutput("rbd", append(m.rbdBaseArgs(cfg), "deep", "cp", cfg.Pool+"/"+name+"@"+snap, cfg.Pool+"/"+target)...); err != nil {
			return Spec{}, fmt.Errorf("RBD restore: %w", err)
		}
	case ProviderCephFS:
		cfg, e := m.CephConfig()
		if e != nil {
			return Spec{}, e
		}
		if _, err = commandOutput("ceph", append(m.cephBaseArgs(cfg), "fs", "subvolume", "snapshot", "clone", cfg.FSName, name, snap, target)...); err != nil {
			return Spec{}, fmt.Errorf("CephFS clone: %w", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		for {
			out, e := commandOutput("ceph", append(m.cephBaseArgs(cfg), "fs", "clone", "status", cfg.FSName, target, "--format", "json")...)
			if e != nil {
				return Spec{}, fmt.Errorf("CephFS clone status: %w", e)
			}
			var status struct {
				Status struct {
					State string `json:"state"`
				} `json:"status"`
			}
			if e = json.Unmarshal([]byte(out), &status); e != nil {
				return Spec{}, e
			}
			if status.Status.State == "complete" {
				break
			}
			if status.Status.State != "pending" && status.Status.State != "in-progress" {
				return Spec{}, fmt.Errorf("CephFS clone not complete: %s", status.Status.State)
			}
			select {
			case <-ctx.Done():
				return Spec{}, ctx.Err()
			case <-time.After(time.Second):
			}
		}
		path, e := commandOutput("ceph", append(m.cephBaseArgs(cfg), "fs", "subvolume", "getpath", cfg.FSName, target)...)
		if e != nil {
			return Spec{}, e
		}
		restored.RemotePath = strings.TrimSpace(path)
	}
	if err = writeJSON(m.specPath(target), restored, 0600); err != nil {
		return Spec{}, err
	}
	success = true
	return restored, nil
}

func (m *Manager) Snapshots(name string) ([]Snapshot, error) {
	spec, err := m.Inspect(name)
	if err != nil {
		return nil, err
	}
	result := []Snapshot{}
	if spec.Provider == ProviderLocal {
		entries, err := os.ReadDir(filepath.Join(m.diskDir(name), "snapshots"))
		if os.IsNotExist(err) {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !diskName.MatchString(entry.Name()) || !entry.IsDir() {
				continue
			}
			var snapshot Snapshot
			if err = readJSON(filepath.Join(m.diskDir(name), "snapshots", entry.Name(), "snapshot.json"), &snapshot); err != nil {
				return nil, err
			}
			result = append(result, snapshot)
		}
	} else {
		cfg, err := m.CephConfig()
		if err != nil {
			return nil, err
		}
		var out string
		if spec.Provider == ProviderCephRBD {
			out, err = commandOutput("rbd", append(m.rbdBaseArgs(cfg), "snap", "ls", cfg.Pool+"/"+name, "--format", "json")...)
		} else {
			out, err = commandOutput("ceph", append(m.cephBaseArgs(cfg), "fs", "subvolume", "snapshot", "ls", cfg.FSName, name, "--format", "json")...)
		}
		if err != nil {
			return nil, err
		}
		var list []struct {
			Name string `json:"name"`
		}
		if err = json.Unmarshal([]byte(out), &list); err != nil {
			return nil, err
		}
		for _, item := range list {
			result = append(result, Snapshot{Name: item.Name, Disk: name, Provider: spec.Provider})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}
func copyTree(source, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// cp -a preserves ownership, xattrs, symlinks, hardlinks and sparse extents.
	out, err := exec.CommandContext(ctx, "cp", "-a", "--reflink=auto", "--", source, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("snapshot data copy: %w: %s", err, out)
	}
	return filepath.Walk(target, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() || info.IsDir() {
			f, e := os.Open(p)
			if e != nil {
				return e
			}
			e = f.Sync()
			f.Close()
			return e
		}
		return nil
	})
}
func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (m *Manager) unmountOnly(name string) error {
	target := m.mountPath(name)
	if mounted(target) {
		if _, err := commandOutput("umount", target); err != nil {
			return err
		}
	}
	return nil
}
