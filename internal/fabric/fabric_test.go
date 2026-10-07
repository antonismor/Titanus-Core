package fabric

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReserveUniqueAddresses(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root)
	if err := os.MkdirAll(filepath.Join(root, "fabric"), 0750); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Bridge: "titanus0", CIDR: "10.240.0.0/24", Gateway: "10.240.0.1/24"}
	if err := writeJSON(manager.configPath(), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(manager.statePath(), state{Allocations: map[string]Allocation{}}, 0600); err != nil {
		t.Fatal(err)
	}

	a, err := manager.Reserve("unit-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := manager.Reserve("unit-b")
	if err != nil {
		t.Fatal(err)
	}
	if a.Address == b.Address {
		t.Fatalf("duplicate allocation %s", a.Address)
	}
	if a.Address != "10.240.0.10" || b.Address != "10.240.0.11" {
		t.Fatalf("unexpected allocations: %s %s", a.Address, b.Address)
	}
}

func TestParsePort(t *testing.T) {
	p, err := ParsePort("8080:80/tcp")
	if err != nil {
		t.Fatal(err)
	}
	if p.HostPort != 8080 || p.ContainerPort != 80 || p.Protocol != "tcp" {
		t.Fatalf("unexpected port: %#v", p)
	}
}
