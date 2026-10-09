package backup

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/version"
)

// AttestFence signs an operator's record of independent exclusion. It does not
// operate a power controller or assert that exclusion was measured by Titanus.
func AttestFence(archive, keyPath, recordPath, output string) (Fence, error) {
	var fence Fence
	key, e := loadKey(keyPath)
	if e != nil {
		return fence, e
	}
	defer clear(key)
	m, e := readArchive(archive, key, nil)
	if e != nil {
		return fence, e
	}
	data, e := os.ReadFile(recordPath)
	if e != nil || len(data) > 64<<10 {
		return fence, fmt.Errorf("bounded fence evidence required")
	}
	var record FenceRecord
	if e := strictJSON(data, &record); e != nil {
		return fence, e
	}
	if record.Realm != m.Plan.Realm || record.Node != m.Plan.Node || !record.OldControllersExcluded || !record.OldWritersExcluded || !record.GatewaysWithdrawn || len(record.Evidence) < 16 || len(record.Evidence) > 8192 {
		return fence, fmt.Errorf("complete Realm/Node-bound exclusion record required")
	}
	digest, e := digestFile(archive)
	if e != nil {
		return fence, e
	}
	now := time.Now().UTC()
	fence = Fence{"titanus-recovery-fence/v1", m.ID, digest, now, now.Add(15 * time.Minute), record}
	signed, e := sign(key, fence)
	if e != nil {
		return fence, e
	}
	return fence, createPrivate(output, signed)
}

func Restore(archive, keyPath, fencePath string, expected Plan) (Manifest, error) {
	lock, e := offline.Exclusive()
	if e != nil {
		return Manifest{}, e
	}
	defer lock.Close()
	if e := rejectRunningActors(); e != nil {
		return Manifest{}, e
	}
	return restore(archive, keyPath, fencePath, expected, nil)
}

// The hook is private and used only to inject a power-loss boundary in tests.
func restore(archive, keyPath, fencePath string, expected Plan, afterRename func(int) error) (Manifest, error) {
	return restoreBound(archive, keyPath, fencePath, expected, afterRename, nil)
}

func restoreBound(archive, keyPath, fencePath string, expected Plan, afterRename func(int) error, binding *ClusterBinding) (Manifest, error) {
	var m Manifest
	if e := expected.Validate(); e != nil {
		return m, e
	}
	key, e := loadKey(keyPath)
	if e != nil {
		return m, e
	}
	defer clear(key)
	// Verify the COMPLETE archive before creating a marker or touching targets.
	m, e = readArchive(archive, key, nil)
	if e != nil {
		return m, e
	}
	if !reflect.DeepEqual(m.Cluster, binding) {
		return m, fmt.Errorf("cluster backup requires complete authenticated recovery set")
	}
	if m.Plan != expected || m.Build.Version != version.Version || m.Build.StateProfile != version.StateProfile || m.Build.Arch != version.Info().Arch {
		return m, fmt.Errorf("restore requires exact original paths, logical Node/Realm and native profile/architecture")
	}
	data, e := os.ReadFile(fencePath)
	if e != nil || len(data) > 64<<10 {
		return m, fmt.Errorf("authenticated fence attestation required")
	}
	var fence Fence
	if e = authenticate(key, data, &fence); e != nil {
		return m, e
	}
	digest, e := digestFile(archive)
	if e != nil {
		return m, e
	}
	now := time.Now().UTC()
	if fence.Format != "titanus-recovery-fence/v1" || fence.BackupID != m.ID || fence.ArchiveSHA256 != digest || fence.Record.Node != expected.Node || fence.Record.Realm != expected.Realm || !fence.Record.OldControllersExcluded || !fence.Record.OldWritersExcluded || !fence.Record.GatewaysWithdrawn || len(fence.Record.Evidence) < 16 || fence.IssuedAt.After(now) || !fence.ExpiresAt.After(now) || fence.ExpiresAt.Sub(fence.IssuedAt) > 15*time.Minute {
		return m, fmt.Errorf("expired, mismatched or incomplete recovery fencing")
	}
	if e = checkHostRestoreStartup(expected.StateRoot, binding); e != nil {
		return m, e
	}
	if e = rejectMounts(expected.roots()); e != nil {
		return m, e
	}
	for _, file := range []string{archive, keyPath, fencePath} {
		for _, root := range expected.roots() {
			if within(file, root) {
				return m, fmt.Errorf("recovery inputs must remain outside restored roots")
			}
		}
	}
	for _, root := range expected.roots() {
		if _, e = os.Lstat(root); !os.IsNotExist(e) {
			return m, fmt.Errorf("restore cannot overwrite existing state/config/ledger: %s", root)
		}
		if st, e := os.Lstat(filepath.Dir(root)); e != nil || !st.IsDir() {
			return m, fmt.Errorf("existing recovery parent required")
		}
	}
	receipt := expected.StateRoot + ".recovery-receipt.json"
	if _, e = os.Lstat(receipt); !os.IsNotExist(e) {
		return m, fmt.Errorf("prior recovery receipt prevents silent replay")
	}
	if e = createPrivate(expected.StateRoot+".recovery-pending", data); e != nil {
		return m, e
	}
	// The fsynced marker survives all errors/crashes. No automatic rollback of
	// identities or restored data occurs, and startup remains blocked.
	stages := map[string]string{}
	defer func() {
		for _, stage := range stages {
			os.RemoveAll(stage)
		}
	}()
	for name, root := range expected.roots() {
		stage, e := os.MkdirTemp(filepath.Dir(root), ".titanus-recovery-")
		if e != nil {
			return m, e
		}
		stages[name] = stage
	}
	restoredManifest, e := readArchive(archive, key, stages)
	if e != nil {
		return m, e
	}
	if !reflect.DeepEqual(m, restoredManifest) {
		return m, fmt.Errorf("archive changed between verification and extraction")
	}
	if e = validateMappedOwners(m.Entries, stages["ledger"]); e != nil {
		return m, e
	}
	state, e := validateHostBound(expected, stages, binding)
	if e != nil {
		return m, e
	}
	stateBytes, _ := json.Marshal(state)
	if state.Revision != m.RealmRevision || fmt.Sprintf("%x", sha256.Sum256(stateBytes)) != m.RealmSHA256 {
		return m, fmt.Errorf("restored logical state disagrees with signed backup")
	}
	progress := map[string]any{"format": "titanus-recovery-receipt/v1", "backup_id": m.ID, "archive_sha256": digest, "status": "prepared", "renamed_roots": []string{}, "fence": fence}
	if e = durable.WriteJSON(receipt, progress, 0600); e != nil {
		return m, e
	}
	renamed := []string{}
	for i, name := range []string{"configuration", "ledger", "state"} {
		root := expected.roots()[name]
		// Linux renameat2 NOREPLACE prevents racing target creation/overwrite.
		if e = renameNoReplace(stages[name], root); e != nil {
			return m, e
		}
		delete(stages, name)
		if e = syncDir(filepath.Dir(root)); e != nil {
			return m, e
		}
		renamed = append(renamed, name)
		progress["renamed_roots"] = renamed
		if e = durable.WriteJSON(receipt, progress, 0600); e != nil {
			return m, e
		}
		if afterRename != nil {
			if e = afterRename(i); e != nil {
				return m, e
			}
		}
	}
	progress["status"] = "complete"
	if e = durable.WriteJSON(receipt, progress, 0600); e != nil {
		return m, e
	}
	if binding != nil {
		return m, syncDir(filepath.Dir(expected.StateRoot))
	}
	if e = os.Remove(expected.StateRoot + ".recovery-pending"); e != nil {
		return m, e
	}
	return m, syncDir(filepath.Dir(expected.StateRoot))
}
