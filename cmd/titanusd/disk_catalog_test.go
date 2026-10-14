package main

import (
	"errors"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"os"
	"testing"
)

type testCatalogRealizer struct {
	specs map[string]disk.Spec
	calls int
	fail  bool
}

func (m *testCatalogRealizer) Inspect(n string) (disk.Spec, error) {
	if s, ok := m.specs[n]; ok {
		return s, nil
	}
	return disk.Spec{}, os.ErrNotExist
}
func (m *testCatalogRealizer) Adopt(c disk.Catalog) error {
	m.calls++
	if m.fail {
		return errors.New("backend identity mismatch")
	}
	s := c.Spec
	s.ManagedID = c.ID()
	m.specs[s.Name] = s
	return nil
}
func TestControllerCatalogRecoveryAndConflict(t *testing.T) {
	c := disk.Catalog{Spec: disk.Spec{Name: "data", Provider: disk.ProviderCephFS, SizeBytes: 1024, Initialized: true, LayoutVersion: 1, RemotePath: "/volumes/data/original"}, Backend: disk.Backend{FSID: "original-fsid", Filesystem: "fs", Object: "/volumes/data/original"}, Fleet: "db"}
	catalogs := map[string]disk.Catalog{"data": c}
	m := &testCatalogRealizer{specs: map[string]disk.Spec{}}
	if e := syncDiskCatalogs(catalogs, m); e != nil || m.calls != 1 {
		t.Fatal(e, m.calls)
	}
	if e := syncDiskCatalogs(catalogs, m); e != nil || m.calls != 1 {
		t.Fatal("repeated synchronization rewrote matching metadata", e)
	}
	delete(m.specs, "data") // missing node-local metadata after recovery
	if e := syncDiskCatalogs(catalogs, m); e != nil || m.calls != 2 {
		t.Fatal(e, m.calls)
	}
	conflicting := c.Spec
	conflicting.RemotePath = "/volumes/data/other"
	m.specs["data"] = conflicting
	m.fail = true
	if e := syncDiskCatalogs(catalogs, m); e == nil {
		t.Fatal("conflict hidden")
	}
	if m.specs["data"].RemotePath != conflicting.RemotePath {
		t.Fatal("conflicting identity replaced")
	}
}
