package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/realm"
)

type catalogRealizer interface {
	Inspect(string) (disk.Spec, error)
	Adopt(disk.Catalog) error
}

// Mirror immutable committed remote identities using verified metadata adoption.
// Followers also run this loop. No mount, create, format or deletion is performed.
func syncDiskCatalogs(catalogs map[string]disk.Catalog, manager catalogRealizer) error {
	var first error
	for name, catalog := range catalogs {
		if catalog.Spec.Provider == disk.ProviderLocal {
			continue
		}
		spec, err := manager.Inspect(name)
		if err == nil && spec.ManagedID == catalog.ID() {
			spec.ManagedID = ""
			if spec == catalog.Spec {
				continue
			}
		}
		if err != nil && !os.IsNotExist(err) {
			if first == nil {
				first = fmt.Errorf("Disk %s: %w", name, err)
			}
			continue
		}
		if err := manager.Adopt(catalog); err != nil && first == nil {
			first = fmt.Errorf("Disk %s: %w", name, err)
		}
	}
	return first
}

func maintainDiskCatalogs(ctx context.Context, store *realm.Store, manager catalogRealizer) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if err := syncDiskCatalogs(store.Snapshot().Disks, manager); err != nil {
			log.Printf("controller Disk catalog synchronization: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
