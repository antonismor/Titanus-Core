package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestSourceIdentityPreservesModesLinksAndRejectsConflict(t *testing.T) {
	input := t.TempDir()
	os.WriteFile(filepath.Join(input, "app"), []byte("content"), 0755)
	os.Symlink("app", filepath.Join(input, "run"))
	one := NewManager(t.TempDir())
	if err := one.ImportDirectory("app-v1", input); err != nil {
		t.Fatal(err)
	}
	digest, err := one.Identity("app-v1")
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if err = one.Export("app-v1", &data); err != nil {
		t.Fatal(err)
	}
	two := NewManager(t.TempDir())
	if _, err = two.ImportBundleExpected(bytes.NewReader(data.Bytes()), "wrong-name", digest); err == nil {
		t.Fatal("published wrong name")
	}
	if names, _ := two.List(); len(names) != 0 {
		t.Fatal("rejected import published Source")
	}
	if _, err = two.ImportBundleExpected(bytes.NewReader(data.Bytes()), "app-v1", "wrong-digest"); err == nil {
		t.Fatal("published wrong digest")
	}
	if _, err = two.ImportBundleExpected(bytes.NewReader(data.Bytes()), "app-v1", digest); err != nil {
		t.Fatal(err)
	}
	if actual, err := two.Identity("app-v1"); err != nil || actual != digest {
		t.Fatal("metadata identity lost", err)
	}
	os.Chmod(filepath.Join(two.StateRoot, "sources", "app-v1", "rootfs", "app"), 0644)
	if actual, _ := two.Identity("app-v1"); actual == digest {
		t.Fatal("executable bit omitted from identity")
	}
}
func TestBundleCannotFollowSymlinkOutOfStaging(t *testing.T) {
	outside := t.TempDir()
	var buffer bytes.Buffer
	gz := gzip.NewWriter(&buffer)
	tw := tar.NewWriter(gz)
	manifest := []byte(`{"format":"titanus-source/v1","name":"evil"}`)
	tw.WriteHeader(&tar.Header{Name: "titanus-source.json", Size: int64(len(manifest)), Mode: 0644})
	tw.Write(manifest)
	tw.WriteHeader(&tar.Header{Name: "rootfs/escape", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0777})
	tw.WriteHeader(&tar.Header{Name: "rootfs/escape/proof", Size: 3, Mode: 0644})
	tw.Write([]byte("bad"))
	tw.Close()
	gz.Close()
	if _, err := NewManager(t.TempDir()).ImportBundle(bytes.NewReader(buffer.Bytes())); err == nil {
		t.Fatal("symlink extraction escape accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "proof")); !os.IsNotExist(err) {
		t.Fatal("host path written", err)
	}
}
