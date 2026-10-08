package disk

import (
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
)

func TestOwnedDiskMaintenanceAndRestore(t *testing.T) {
	m := NewManager(t.TempDir())
	if _, err := m.Create(Spec{Name: "data", SizeBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	a, err := m.Acquire("data", "unit-a", "run-a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewManager(m.StateRoot).Acquire("data", "unit-b", "run-b"); err == nil {
		t.Fatal("simultaneous writer accepted")
	}
	if _, err = m.Snapshot("data", "backup"); err == nil {
		t.Fatal("snapshot of live writer accepted")
	}
	if err = m.Delete("data", false); err == nil {
		t.Fatal("deleted live data")
	}
	if err = m.Detach("data"); err == nil {
		t.Fatal("detached live writer")
	}
	if err = os.WriteFile(filepath.Join(a.Path, "proof"), []byte("before"), 0640); err != nil {
		t.Fatal(err)
	}
	if err = os.Link(filepath.Join(a.Path, "proof"), filepath.Join(a.Path, "hard")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink("../../outside", filepath.Join(a.Path, "link")); err != nil {
		t.Fatal(err)
	}
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Snapshot("data", "backup"); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Snapshot("data", "backup"); err == nil {
		t.Fatal("overwrote snapshot")
	}
	path, _ := m.Resolve("data")
	if err = os.WriteFile(filepath.Join(path, "proof"), []byte("after"), 0640); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Restore("data", "backup", "restored"); err != nil {
		t.Fatal(err)
	}
	b, err := m.Acquire("restored", "unit-b", "run-b")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	proof, err := os.ReadFile(filepath.Join(b.Path, "proof"))
	if err != nil || string(proof) != "before" {
		t.Fatal("snapshot not independent", err, string(proof))
	}
	link, _ := os.Readlink(filepath.Join(b.Path, "link"))
	if link != "../../outside" {
		t.Fatal("symlink changed")
	}
	x, _ := os.Stat(filepath.Join(b.Path, "proof"))
	y, _ := os.Stat(filepath.Join(b.Path, "hard"))
	if !os.SameFile(x, y) {
		t.Fatal("hardlinks lost")
	}
	if _, err = m.Restore("data", "backup", "restored"); err == nil {
		t.Fatal("overwrote restored Disk")
	}
	items, err := m.Snapshots("data")
	if err != nil || len(items) != 1 || items[0].Name != "backup" {
		t.Fatal(items, err)
	}
}

func TestAcquireIsSerializedAcrossManagers(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root)
	if _, err := m.Create(Spec{Name: "data", SizeBytes: 1024}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wins := make(chan *Attachment, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, e := NewManager(root).Acquire("data", "unit", "run")
			if e == nil {
				wins <- a
			}
		}()
	}
	wg.Wait()
	close(wins)
	count := 0
	for a := range wins {
		count++
		a.Close()
	}
	if count != 1 {
		t.Fatal("ownership split", count)
	}
}
func TestLockDescriptorDuplicationRetainsOwnership(t *testing.T) {
	m := NewManager(t.TempDir())
	m.Create(Spec{Name: "data", SizeBytes: 1024})
	a, err := m.Acquire("data", "unit", "run")
	if err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Dup(int(a.File.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	if _, err = m.Acquire("data", "another", "run"); err == nil {
		t.Fatal("parent close released inherited lock")
	}
	syscall.Close(fd)
	b, err := m.Acquire("data", "another", "run")
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
}
func TestUnsafeDiskLayoutFailsClosed(t *testing.T) {
	m := NewManager(t.TempDir())
	m.Create(Spec{Name: "data", SizeBytes: 1024})
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(m.diskDir("data"), "control")); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire("data", "unit", "run"); err == nil {
		t.Fatal("symlink control accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "lock")); !os.IsNotExist(err) {
		t.Fatal("wrote outside Disk control")
	}
}
