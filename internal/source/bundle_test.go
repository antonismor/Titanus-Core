package source

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSourceBundleRoundTrip(t *testing.T) {
	sourceRoot := t.TempDir()
	input := filepath.Join(sourceRoot, "input")
	if err := os.MkdirAll(filepath.Join(input, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(input, "bin", "hello"), []byte("titanus\n"), 0755); err != nil {
		t.Fatal(err)
	}
	m1 := NewManager(filepath.Join(sourceRoot, "one"))
	if err := m1.ImportDirectory("demo", input); err != nil {
		t.Fatal(err)
	}
	var bundle bytes.Buffer
	if err := m1.Export("demo", &bundle); err != nil {
		t.Fatal(err)
	}

	m2 := NewManager(filepath.Join(sourceRoot, "two"))
	manifest, err := m2.ImportBundle(bytes.NewReader(bundle.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "demo" {
		t.Fatalf("unexpected manifest %#v", manifest)
	}
	data, err := os.ReadFile(filepath.Join(sourceRoot, "two", "sources", "demo", "rootfs", "bin", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "titanus\n" {
		t.Fatalf("unexpected file %q", data)
	}
}
