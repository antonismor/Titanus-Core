package disk

import "testing"

func TestParseMount(t *testing.T) {
	m, err := ParseMount("db:/var/lib/db:ro")
	if err != nil {
		t.Fatal(err)
	}
	if m.Disk != "db" || m.Target != "/var/lib/db" || !m.ReadOnly {
		t.Fatalf("unexpected mount %#v", m)
	}
}

func TestLocalDiskLifecycle(t *testing.T) {
	manager := NewManager(t.TempDir())
	spec, err := manager.Create(Spec{Name: "data", Provider: ProviderLocal, SizeBytes: 1024 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Initialized {
		t.Fatal("local Disk should be initialized")
	}
	path, err := manager.Resolve("data")
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		t.Fatal("empty local Disk path")
	}
	items, err := manager.List()
	if err != nil || len(items) != 1 {
		t.Fatalf("unexpected list: %#v err=%v", items, err)
	}
	if err := manager.Delete("data", false); err != nil {
		t.Fatal(err)
	}
}
