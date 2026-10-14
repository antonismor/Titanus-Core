package backup

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"os"
	"path/filepath"
)

// StageClusterNode aligns only an offline non-voter's cache with a signed
// recovery intent. Consensus directories and voter state are never changed.
func StageClusterNode(p Plan, intentPath, keyPath, previousPath string) error {
	lock, err := offline.Exclusive()
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = rejectRunningActors(); err != nil {
		return err
	}
	if err = p.Validate(); err != nil {
		return err
	}
	if p.Role != "node" {
		return fmt.Errorf("checkpoint staging is only for non-voter nodes")
	}
	if _, err = os.Lstat(filepath.Join(p.StateRoot, "realm", "consensus")); !os.IsNotExist(err) {
		return fmt.Errorf("checkpoint staging refuses consensus-managed state")
	}
	var intent ClusterIntent
	if err = readSigned(intentPath, keyPath, &intent); err != nil {
		return err
	}
	if err = intent.validate(); err != nil {
		return err
	}
	found := false
	for _, planned := range intent.Hosts {
		if planned == p {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("node absent from signed recovery intent")
	}
	if !filepath.IsAbs(previousPath) || filepath.Clean(previousPath) != previousPath {
		return fmt.Errorf("new absolute previous-state output required")
	}
	for _, root := range p.roots() {
		if within(previousPath, root) {
			return fmt.Errorf("previous-state output must be outside host roots")
		}
	}
	if err = noSymlinkParents(previousPath); err != nil {
		return err
	}
	var previous realm.State
	if err = loadJSON(filepath.Join(p.StateRoot, "realm", "state.json"), &previous); err != nil {
		return err
	}
	if previous.Name != intent.Realm {
		return fmt.Errorf("local cache belongs to another Realm")
	}
	// Check all retained Sources, PKI, UID ledger, stopped Units, schema floor,
	// local bytes and bound catalogs before publishing the new cache.
	if _, err = validateHostBoundState(p, p.roots(), &intent.Binding, &intent.State); err != nil {
		return err
	}
	if err = writeSigned(previousPath, keyPath, previous); err != nil {
		return err
	}
	return durable.WriteJSON(filepath.Join(p.StateRoot, "realm", "state.json"), intent.State, 0600)
}
