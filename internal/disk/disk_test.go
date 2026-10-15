package disk

import (
	"strings"
	"testing"
)

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

func TestMountsRejectProtectedRuntimePaths(t *testing.T) {
	for _, target := range []string{"/dev", "/dev/disk", "/proc", "/sys", "/.titanus-oldroot", "/data/../proc"} {
		if err := ValidateMounts([]Mount{{Disk: "data", Target: target}}); err == nil {
			t.Fatalf("accepted %s", target)
		}
	}
}

func TestCephFenceIdentityIsSeparateFromDataIdentity(t *testing.T) {
	m := NewManager(t.TempDir())
	cfg := CephConfig{Cluster: "ceph", Client: "client.data", Conf: "/etc/ceph/ceph.conf", Keyring: "/data.keyring", FenceClient: "client.fence", FenceKeyring: "/fence.keyring"}
	data := strings.Join(m.cephBaseArgs(cfg), " ")
	fence := strings.Join(m.cephFenceArgs(cfg), " ")
	if !strings.Contains(data, "--id data") || !strings.Contains(data, "--keyring /data.keyring") || strings.Contains(data, "fence") {
		t.Fatalf("unexpected data identity args: %s", data)
	}
	if !strings.Contains(fence, "--id fence") || !strings.Contains(fence, "--keyring /fence.keyring") || strings.Contains(fence, "data.keyring") {
		t.Fatalf("unexpected fence identity args: %s", fence)
	}
}

func TestCephFenceIdentityDefaultsToDataIdentity(t *testing.T) {
	m := NewManager(t.TempDir())
	cfg := CephConfig{Cluster: "ceph", Client: "client.data", Conf: "/etc/ceph/ceph.conf", Keyring: "/data.keyring"}
	if err := m.ConfigureCeph(cfg); err != nil {
		t.Fatal(err)
	}
	stored, err := m.CephConfig()
	if err != nil {
		t.Fatal(err)
	}
	if stored.FenceClient != stored.Client || stored.FenceKeyring != stored.Keyring {
		t.Fatalf("legacy Ceph config did not preserve compatibility: %#v", stored)
	}
}
