package main

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"sort"
)

// Realm and local catalogs are different views. Include all committed disks,
// and fail explicitly on conflicting local native identities.
func mergeDiskViews(local []disk.Spec, catalogs map[string]disk.Catalog) ([]disk.Spec, bool, error) {
	items := map[string]disk.Spec{}
	for _, s := range local {
		items[s.Name] = s
	}
	missing := false
	for name, c := range catalogs {
		if old, ok := items[name]; ok {
			old.ManagedID = ""
			if old != c.Spec {
				return nil, false, fmt.Errorf("local Disk %s conflicts with Realm catalog", name)
			}
		} else {
			items[name] = c.Spec
			missing = true
		}
	}
	names := make([]string, 0, len(items))
	for name := range items {
		names = append(names, name)
	}
	sort.Strings(names)
	merged := make([]disk.Spec, 0, len(names))
	for _, name := range names {
		merged = append(merged, items[name])
	}
	return merged, missing, nil
}
