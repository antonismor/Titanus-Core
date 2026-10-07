package unitruntime

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestMappingLedgerPersistsAndDoesNotReuse(t *testing.T) {
	root := t.TempDir()
	var wg sync.WaitGroup
	mappings := make(chan IDMapping, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, e := allocateMapping(root, filepath.Join("state", string(rune('a'+i))))
			if e != nil {
				t.Error(e)
				return
			}
			mappings <- v
		}(i)
	}
	wg.Wait()
	close(mappings)
	seen := map[int]bool{}
	for v := range mappings {
		if seen[v.Base] || v.Base == 0 || v.Size != 65536 {
			t.Fatalf("unsafe mapping %+v", v)
		}
		seen[v.Base] = true
	}
	v, e := allocateMapping(root, "state/a")
	if e != nil {
		t.Fatal(e)
	}
	restored, e := allocateMapping(root, "state/a")
	if e != nil || v != restored {
		t.Fatal("mapping changed")
	}
	// A different incarnation (even with the same Unit ID) has a new ledger key.
	next, e := allocateMapping(root, "state/a@new")
	if e != nil || seen[next.Base] {
		t.Fatal("reused retired ownership")
	}
}

func TestMappingLedgerFailsClosedOnCorruption(t *testing.T) {
	root := t.TempDir()
	for _, data := range []string{`broken`, `{"a":{"base":0,"size":65536}}`, `{"a":{"base":1048576,"size":65536},"b":{"base":1048576,"size":65536}}`} {
		if e := os.WriteFile(filepath.Join(root, "allocations.json"), []byte(data), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := allocateMapping(root, "new"); e == nil {
			t.Fatalf("accepted %s", data)
		}
	}
}

func TestDiskMountTargetRejectsSourceSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(root, "escape")); e != nil {
		t.Fatal(e)
	}
	if _, e := unitMountTarget(root, "/escape/disk"); e == nil {
		t.Fatal("symlink escape accepted")
	}
	if _, e := os.Stat(filepath.Join(outside, "disk")); !os.IsNotExist(e) {
		t.Fatal("modified host path")
	}
}

func TestMappingLedgerLossOrMismatchFailsClosed(t *testing.T) {
	root := t.TempDir()
	mapping, err := allocateMapping(root, "unit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := allocateMapping(root, "unit", IDMapping{Base: mapping.Base + 65536, Size: 65536}); err == nil {
		t.Fatal("changed Unit ownership accepted")
	}
	if err := os.Remove(filepath.Join(root, "allocations.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := allocateMapping(root, "unit", mapping); err == nil {
		t.Fatal("lost ledger silently recreated")
	}
}
