package offline

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMutualExclusionAndCrashMarkers(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lock")
	a, e := acquire(p, false)
	if e != nil {
		t.Fatal(e)
	}
	b, e := acquire(p, false)
	if e != nil {
		t.Fatal(e)
	}
	if x, e := acquire(p, true); e == nil {
		x.Close()
		t.Fatal("maintenance admitted live reader")
	}
	a.Close()
	b.Close()
	x, e := acquire(p, true)
	if e != nil {
		t.Fatal(e)
	}
	if y, e := acquire(p, false); e == nil {
		y.Close()
		t.Fatal("startup admitted during maintenance")
	}
	x.Close()
	root := filepath.Join(t.TempDir(), "state")
	if e := CheckStartup(root); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(root+".recovery-pending", nil, 0600); e != nil {
		t.Fatal(e)
	}
	if e := CheckStartup(root); e == nil {
		t.Fatal("partial restore allowed startup")
	}
	link := filepath.Join(t.TempDir(), "link")
	if e := os.Symlink(p, link); e != nil {
		t.Fatal(e)
	}
	if y, e := acquire(link, true); e == nil {
		y.Close()
		t.Fatal("followed lock symlink")
	}
}
