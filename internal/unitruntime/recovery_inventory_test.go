package unitruntime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecoveryInventoryRejectsUninspectableAndRedirectedUnits(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StateRoot = t.TempDir()
	m := NewManager(cfg)
	if e := m.EmptyRecoveryInventory(); e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(cfg.StateRoot, "units")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	if e := m.EmptyRecoveryInventory(); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "corrupt"), []byte("incomplete writer"), 0600); e != nil {
		t.Fatal(e)
	}
	items, e := m.List()
	if e != nil || len(items) != 0 {
		t.Fatal("diagnostic List fixture", e)
	}
	if e = m.EmptyRecoveryInventory(); e == nil {
		t.Fatal("corrupt Unit was hidden by diagnostic List")
	}
	cfg.StateRoot = t.TempDir()
	m = NewManager(cfg)
	if e = os.Symlink(t.TempDir(), filepath.Join(cfg.StateRoot, "units")); e != nil {
		t.Fatal(e)
	}
	if e = m.EmptyRecoveryInventory(); e == nil {
		t.Fatal("redirected Unit inventory admitted")
	}
}
