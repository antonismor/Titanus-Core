package backup

import (
	"archive/tar"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/offline"
)

type StorageManifest struct {
	Format         string            `json:"format"`
	Binding        ClusterBinding    `json:"binding"`
	Entries        []Entry           `json:"entries"`
	RawImageSHA256 map[string]string `json:"raw_image_sha256"`
}

func storageRoots(root string, catalogs map[string]disk.Catalog) map[string]string {
	out := map[string]string{}
	for name := range catalogs {
		out[name] = filepath.Join(root, name)
	}
	return out
}
func verifyStorageEntries(root string, m StorageManifest) error {
	if m.Format != "titanus-ceph-data/v1" || m.Binding.StorageSHA256 != "" {
		return fmt.Errorf("unsupported Ceph backup profile")
	}
	if e := m.Binding.validate(); e != nil {
		return e
	}
	entries, e := inventoryRoots(storageRoots(root, m.Binding.Catalogs))
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(entries, m.Entries) {
		return fmt.Errorf("Ceph payload hash/owner/mode/inventory mismatch")
	}
	listed, e := os.ReadDir(root)
	if e != nil {
		return e
	}
	if len(listed) != len(m.Binding.Catalogs)+1 {
		return fmt.Errorf("unlisted Ceph bundle content")
	}
	for _, f := range listed {
		if f.Name() != "manifest.json" {
			if _, ok := m.Binding.Catalogs[f.Name()]; !ok {
				return fmt.Errorf("unlisted Ceph bundle path")
			}
		}
	}
	for name, c := range m.Binding.Catalogs {
		if len(c.Snapshots) != 0 {
			return fmt.Errorf("unsupported snapshot history")
		}
		if c.Spec.Provider == disk.ProviderCephRBD {
			if !hashPattern.MatchString(m.RawImageSHA256[name]) {
				return fmt.Errorf("missing full raw RBD integrity digest")
			}
		} else if m.RawImageSHA256[name] != "" {
			return fmt.Errorf("unexpected RBD hash for CephFS")
		}
	}
	return nil
}
func VerifyStorage(root, keyPath string) (StorageManifest, error) {
	var m StorageManifest
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return m, fmt.Errorf("absolute Ceph bundle directory required")
	}
	if e := noSymlinkParents(root); e != nil {
		return m, e
	}
	if e := readSigned(filepath.Join(root, "manifest.json"), keyPath, &m); e != nil {
		return m, e
	}
	return m, verifyStorageEntries(root, m)
}
func ExportStorage(stateRoot, intentPath, keyPath, output string) (StorageManifest, error) {
	var m StorageManifest
	l, e := offline.Exclusive()
	if e != nil {
		return m, e
	}
	defer l.Close()
	if e = rejectRunningActors(); e != nil {
		return m, e
	}
	var intent ClusterIntent
	if e = readSigned(intentPath, keyPath, &intent); e != nil {
		return m, e
	}
	if e = intent.validate(); e != nil {
		return m, e
	}
	if !filepath.IsAbs(output) || filepath.Clean(output) != output {
		return m, fmt.Errorf("absolute output required")
	}
	if e = noSymlinkParents(output); e != nil {
		return m, e
	}
	if _, e = os.Lstat(output); !os.IsNotExist(e) {
		return m, fmt.Errorf("Ceph bundle output exists")
	}
	for _, p := range intent.Hosts {
		for _, r := range p.roots() {
			if within(output, r) || within(keyPath, r) {
				return m, fmt.Errorf("Ceph output/authentication key must be outside host roots")
			}
		}
	}
	tmp, e := os.MkdirTemp(filepath.Dir(output), ".ceph-export-")
	if e != nil {
		return m, e
	}
	defer os.RemoveAll(tmp)
	manager := disk.NewManager(stateRoot)
	m = StorageManifest{"titanus-ceph-data/v1", intent.Binding, nil, map[string]string{}}
	for name, c := range m.Binding.Catalogs {
		var raw string
		if raw, e = manager.ExportOfflineData(c, filepath.Join(tmp, name)); e != nil {
			return m, e
		}
		if c.Spec.Provider == disk.ProviderCephRBD {
			m.RawImageSHA256[name] = raw
			if e != nil {
				return m, e
			}
		}
	}
	m.Entries, e = inventoryRoots(storageRoots(tmp, m.Binding.Catalogs))
	if e != nil {
		return m, e
	}
	if e = writeSigned(filepath.Join(tmp, "manifest.json"), keyPath, m); e != nil {
		return m, e
	}
	if e = verifyStorageEntries(tmp, m); e != nil {
		return m, e
	}
	if e = syncPayload(tmp); e != nil {
		return m, e
	}
	if e = renameNoReplace(tmp, output); e != nil {
		return m, e
	}
	return m, syncDir(filepath.Dir(output))
}

type StorageFence struct {
	Format    string      `json:"format"`
	SetSHA256 string      `json:"set_sha256"`
	ID        string      `json:"id"`
	Record    FenceRecord `json:"record"`
	IssuedAt  time.Time   `json:"issued_at"`
	ExpiresAt time.Time   `json:"expires_at"`
}

func AttestStorageFence(setPath, keyPath, recordPath, output string) error {
	set, e := VerifyClusterSet(setPath, keyPath)
	if e != nil {
		return e
	}
	var r FenceRecord
	if e = loadDocument(recordPath, &r); e != nil {
		return e
	}
	if r.Realm != set.Intent.Realm || r.Node != "storage" || !r.OldControllersExcluded || !r.OldWritersExcluded || !r.GatewaysWithdrawn || len(r.Evidence) < 16 || len(r.Evidence) > 8192 {
		return fmt.Errorf("complete set-bound external exclusion record required")
	}
	sha, e := digestFile(setPath)
	if e != nil {
		return e
	}
	now := time.Now().UTC()
	return writeSigned(output, keyPath, StorageFence{"titanus-storage-fence/v1", sha, set.Intent.Binding.ID, r, now, now.Add(2 * time.Hour)})
}
func ImportStorage(stateRoot, setPath, keyPath, fencePath, output string) error {
	l, e := offline.Exclusive()
	if e != nil {
		return e
	}
	defer l.Close()
	if e = rejectRunningActors(); e != nil {
		return e
	}
	// Authenticate the complete host + storage set before even creating a marker.
	set, e := VerifyClusterSet(setPath, keyPath)
	if e != nil {
		return e
	}
	sha, e := digestFile(setPath)
	if e != nil {
		return e
	}
	var fence StorageFence
	if e = readSigned(fencePath, keyPath, &fence); e != nil {
		return e
	}
	now := time.Now().UTC()
	if fence.Format != "titanus-storage-fence/v1" || fence.SetSHA256 != sha || fence.ID != set.Intent.Binding.ID || fence.Record.Realm != set.Intent.Realm || fence.Record.Node != "storage" || !fence.Record.OldControllersExcluded || !fence.Record.OldWritersExcluded || !fence.Record.GatewaysWithdrawn || len(fence.Record.Evidence) < 16 || fence.IssuedAt.After(now) || !fence.ExpiresAt.After(now) || fence.ExpiresAt.Sub(fence.IssuedAt) > 2*time.Hour {
		return fmt.Errorf("expired/incomplete storage exclusion")
	}
	m, e := VerifyStorage(set.Storage, keyPath)
	if e != nil {
		return e
	}
	manager := disk.NewManager(stateRoot)
	// Preflight every backend before modifying the first one.
	for _, c := range m.Binding.Catalogs {
		if e = manager.VerifyCatalog(c); e != nil {
			return e
		}
	}
	marker := stateRoot + ".backup-pending"
	receipt := stateRoot + ".ceph-recovery-receipt.json"
	if _, e = os.Lstat(receipt); !os.IsNotExist(e) {
		return fmt.Errorf("prior Ceph restore cannot silently replay")
	}
	if e = createPrivate(marker, []byte(sha)); e != nil {
		return e
	}
	progress := map[string]any{"format": "titanus-ceph-receipt/v1", "set_sha256": sha, "completed": []string{}, "status": "importing"}
	if e = durable.WriteJSON(receipt, progress, 0600); e != nil {
		return e
	}
	done := []string{}
	for name, c := range m.Binding.Catalogs {
		if !fence.ExpiresAt.After(time.Now()) {
			return fmt.Errorf("storage fence expired during import")
		}
		if e = manager.ImportOfflineData(c, filepath.Join(set.Storage, name)); e != nil {
			return e
		}
		if c.Spec.Provider == disk.ProviderCephRBD {
			h, e := manager.RawImageDigest(c)
			if e != nil {
				return e
			}
			if h != m.RawImageSHA256[name] {
				return fmt.Errorf("restored RBD logical bytes differ")
			}
		} else {
			if e = manager.WithOfflineData(c, func(path string) error {
				entries, e := inventoryRoots(map[string]string{name: path})
				if e != nil {
					return e
				}
				want := []Entry{}
				for _, v := range m.Entries {
					if strings.Split(v.Path, "/")[0] == name {
						want = append(want, v)
					}
				}
				if !reflect.DeepEqual(entries, want) {
					return fmt.Errorf("restored CephFS bytes/numeric ownership/metadata differ")
				}
				return nil
			}); e != nil {
				return e
			}
		}
		done = append(done, name)
		progress["completed"] = done
		if e = durable.WriteJSON(receipt, progress, 0600); e != nil {
			return e
		}
	}
	// Re-verify bundle immutability after all backend operations.
	if _, e = VerifyStorage(set.Storage, keyPath); e != nil {
		return e
	}
	if e = writeSigned(output, keyPath, RecoveryProof{"titanus-recovery-proof/v1", sha, set.Intent.Binding.ID, "storage", set.Intent.Binding.RealmSHA256, true, time.Now().UTC()}); e != nil {
		return e
	}
	progress["status"] = "complete"
	if e = durable.WriteJSON(receipt, progress, 0600); e != nil {
		return e
	}
	if e = os.Remove(marker); e != nil {
		return e
	}
	return syncDir(filepath.Dir(marker))
}
func entryInstalled(path string, v Entry) (string, error) {
	st, e := os.Lstat(path)
	if e != nil {
		return "", e
	}
	h, e := tar.FileInfoHeader(st, "")
	if e != nil {
		return "", e
	}
	if h.Uid != v.UID || h.Gid != v.GID || h.Mode != v.Mode {
		return "", fmt.Errorf("restored numeric permissions mismatch")
	}
	switch v.Type {
	case "file":
		if !st.Mode().IsRegular() || st.Size() != v.Size {
			return "", fmt.Errorf("restored type/size mismatch")
		}
		return digestFile(path)
	case "directory":
		if !st.IsDir() {
			return "", fmt.Errorf("restored directory mismatch")
		}
	case "symlink":
		if st.Mode()&os.ModeSymlink == 0 {
			return "", fmt.Errorf("restored link mismatch")
		}
		link, e := os.Readlink(path)
		if e != nil || link != v.Link {
			return "", fmt.Errorf("restored link target mismatch")
		}
	default:
		return "", fmt.Errorf("unknown installed entry")
	}
	return "", nil
}

func syncPayload(root string) error {
	return filepath.Walk(root, func(path string, st os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if st.Mode().IsRegular() || st.IsDir() {
			f, e := os.Open(path)
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
