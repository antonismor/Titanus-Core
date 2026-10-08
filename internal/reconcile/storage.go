package reconcile

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"time"
)

type storageFenceRefresher interface {
	RefreshFence(disk.Catalog, disk.Writer, func() error) error
}

func (c *Controller) maintainStorageFences() error {
	refresher, ok := c.Storage.(storageFenceRefresher)
	if !ok {
		return nil
	}
	for id, intent := range c.Store.Snapshot().StorageFailovers {
		if !intent.Completed || time.Now().Before(intent.RefreshAfter) {
			continue
		}
		for name, catalog := range intent.Catalogs {
			if err := refresher.RefreshFence(catalog, intent.Assignment.StorageWriters[name], c.Store.CheckLeader); err != nil {
				return err
			}
		}
		if err := c.Store.StorageFenceRefreshed(id); err != nil {
			return err
		}
	}
	return nil
}

type StorageNodes interface {
	DiskCatalog(address, name string) (disk.Catalog, error)
	ReleaseDisk(address, name, unit, catalogID string) error
	EnsureDisk(address string, c disk.Catalog) error
	DiskWriter(address, name string) (disk.Writer, error)
}

type StorageFencer interface {
	Fence(disk.Catalog, disk.Writer, func() error) error
}

func (c *Controller) prepareStorage(state realm.State, a realm.Assignment, spec *unitruntime.Spec) error {
	managed := false
	for _, m := range spec.Mounts {
		catalog, ok := state.Disks[m.Disk]
		if !ok {
			continue
		}
		if catalog.Fleet != a.Fleet {
			return fmt.Errorf("catalog ownership mismatch")
		}
		if catalog.LocalNode != "" {
			if catalog.LocalNode != a.NodeID {
				return fmt.Errorf("local Disk placement mismatch")
			}
			nodes, ok := c.Nodes.(StorageNodes)
			if !ok {
				return fmt.Errorf("local catalog verification unavailable")
			}
			actual, err := nodes.DiskCatalog(state.Nodes[a.NodeID].Address, m.Disk)
			if err != nil {
				return err
			}
			if actual.ID() != catalog.ID() {
				return fmt.Errorf("local Disk realization differs from catalog")
			}
			continue
		}
		managed = true
		nodes, ok := c.Nodes.(StorageNodes)
		if !ok {
			return fmt.Errorf("node client cannot realize remote catalog")
		}
		if err := c.Store.CheckLeader(); err != nil {
			return err
		}
		if err := nodes.EnsureDisk(state.Nodes[a.NodeID].Address, catalog); err != nil {
			return err
		}
	}
	if managed {
		mapping, err := c.Store.UnitMapping(a.ID)
		if err != nil {
			return err
		}
		key, err := realm.StorageMappingIdentity(state, a)
		if err != nil {
			return err
		}
		spec.ClusterMappingKey = state.Name + "/" + key
		spec.ClusterMapping = mapping
	}
	return nil
}

func (c *Controller) recordStorage(state realm.State, a realm.Assignment) error {
	writers := map[string]disk.Writer{}
	fleet, err := state.Fleets[a.Fleet].ForGeneration(a.Generation)
	if err != nil {
		return err
	}
	for _, m := range fleet.Template.Mounts {
		catalog, ok := state.Disks[m.Disk]
		if !ok || catalog.LocalNode != "" {
			continue
		}
		nodes, ok := c.Nodes.(StorageNodes)
		if !ok {
			return fmt.Errorf("writer evidence unavailable")
		}
		w, err := nodes.DiskWriter(state.Nodes[a.NodeID].Address, m.Disk)
		if err != nil {
			return err
		}
		writers[m.Disk] = w
	}
	if len(writers) == 0 {
		return nil
	}
	return c.Store.RecordStorageWriters(c.Store.Snapshot().Assignments[a.ID], writers)
}

func (c *Controller) fenceStorage(a realm.Assignment) error {
	if c.Storage == nil {
		return fmt.Errorf("automatic storage fencer unavailable")
	}
	intent, err := c.Store.BeginStorageFailover(a)
	if err != nil {
		return err
	}
	if intent.Completed {
		return c.Store.CheckLeader()
	}
	for name, catalog := range intent.Catalogs {
		if err = c.Store.CheckLeader(); err != nil {
			return err
		}
		if err = c.Storage.Fence(catalog, intent.Assignment.StorageWriters[name], c.Store.CheckLeader); err != nil {
			return err
		}
	}
	if err = c.Store.CheckLeader(); err != nil {
		return err
	}
	return c.Store.CompleteStorageFailover(intent)
}

func (c *Controller) releaseStorage(state realm.State, a realm.Assignment) error {
	f, ok := state.Fleets[a.Fleet]
	if !ok {
		return nil
	}
	f, err := f.ForGeneration(a.Generation)
	if err != nil {
		return err
	}
	for _, m := range f.Template.Mounts {
		catalog, ok := state.Disks[m.Disk]
		if !ok || catalog.LocalNode != "" {
			continue
		}
		nodes, ok := c.Nodes.(StorageNodes)
		if !ok {
			return fmt.Errorf("storage release client unavailable")
		}
		if err = c.Store.CheckLeader(); err != nil {
			return err
		}
		if err = nodes.ReleaseDisk(state.Nodes[a.NodeID].Address, m.Disk, a.ID, catalog.ID()); err != nil {
			return err
		}
	}
	return nil
}
