package disk

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFenceIdentityAndQuorumFailClosed(t *testing.T) {
	c := Catalog{Spec: Spec{Name: "data", Provider: ProviderCephFS, SizeBytes: 1 << 20, Initialized: true, LayoutVersion: 1, RemotePath: "/volumes/data/uuid"}, Backend: Backend{FSID: "fsid", Filesystem: "cephfs", Object: "/volumes/data/uuid"}, Fleet: "db", AutoFailover: true}
	w := Writer{Disk: "data", CatalogID: c.ID(), Address: "v1:192.0.2.1:0/99", Session: 9}
	m := NewManager(t.TempDir())
	denied := errors.New("lost quorum")
	if e := m.Fence(c, w, func() error { return denied }); !errors.Is(e, denied) {
		t.Fatal("fence acted without quorum", e)
	}
	for _, address := range []string{"192.0.2.1:0", "v1:192.0.2.1:0/0", "client.9", "192.0.2.1:70000/9", "v1:host:0/9"} {
		if validInstance(address) {
			t.Fatal("ambiguous fence target", address)
		}
	}
	for _, address := range []string{"v1:192.0.2.1:0/99", "v2:[2001:db8::1]:3300/123"} {
		if !validInstance(address) {
			t.Fatal("valid instance rejected", address)
		}
	}
	wrong := w
	wrong.CatalogID = "another-object"
	if e := m.Fence(c, wrong, func() error { return nil }); e == nil {
		t.Fatal("fence accepted unrelated object")
	}
}
func TestCatalogCannotAdoptLocalDataOrFabricateSnapshots(t *testing.T) {
	m := NewManager(t.TempDir())
	s, e := m.Create(Spec{Name: "local", SizeBytes: 1 << 20})
	if e != nil {
		t.Fatal(e)
	}
	c := Catalog{Spec: s, Fleet: "db", LocalNode: "one"}
	if e = m.Adopt(c); e == nil {
		t.Fatal("local Disk metadata imported as data migration")
	}
	c.Snapshots = []Snapshot{{Name: "s", Disk: "wrong", Provider: ProviderLocal}}
	if e = c.Validate(); e == nil {
		t.Fatal("invalid snapshot inventory")
	}
	if e = os.WriteFile(filepath.Join(m.diskDir("local"), "writer.json"), []byte(`{"disk":"local","address":"v1:192.0.2.1:0/99"}`), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = m.Writer("local"); e == nil {
		t.Fatal("diagnostic file impersonated native writer")
	}
}
