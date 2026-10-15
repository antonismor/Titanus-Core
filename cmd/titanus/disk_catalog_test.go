package main

import (
	"github.com/antonismor/Titanus-Core/internal/disk"
	"testing"
)

func TestDiskListIncludesPartiallyRealizedRealmCatalog(t *testing.T) {
	a := disk.Spec{Name: "a", Provider: disk.ProviderCephFS, SizeBytes: 1024, RemotePath: "/volumes/a/original"}
	b := disk.Spec{Name: "b", Provider: disk.ProviderCephFS, SizeBytes: 2048, RemotePath: "/volumes/b/original"}
	local := a
	local.ManagedID = "managed"
	got, missing, e := mergeDiskViews([]disk.Spec{local}, map[string]disk.Catalog{"a": {Spec: a}, "b": {Spec: b}})
	if e != nil || !missing || len(got) != 2 || got[0].Name != "a" || got[1].Name != "b" {
		t.Fatal(got, missing, e)
	}
	local.RemotePath = "/volumes/a/conflict"
	if _, _, e := mergeDiskViews([]disk.Spec{local}, map[string]disk.Catalog{"a": {Spec: a}}); e == nil {
		t.Fatal("identity conflict hidden")
	}
}
