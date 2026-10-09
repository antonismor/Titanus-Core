package release

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/version"
)

func fixture(t *testing.T, mutation string) (string, string) {
	t.Helper()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	binary, e := os.ReadFile(exe)
	if e != nil {
		t.Fatal(e)
	}
	meta := version.Build{Version: "0.4.0-rc.4", Revision: strings.Repeat("a", 40), OS: "linux", Arch: runtime.GOARCH, StateProfile: version.StateProfile}
	if mutation == "arch" {
		meta.Arch = "other"
	}
	manifest, _ := json.Marshal(meta)
	files := map[string][]byte{"manifest.json": manifest}
	for _, n := range []string{"bin/titanus", "bin/titanusd", "bin/titanus-agent", "bin/titanus-init"} {
		files[n] = binary
	}
	for _, n := range []string{"systemd/titanusd.service", "systemd/titanus-agent.service", "scripts/install-release.py", "scripts/install-release.sh", "docs/RELEASE.md", "LICENSE"} {
		files[n] = []byte("fixture")
	}
	sums := ""
	names := []string{}
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sum := sha256.Sum256(files[n])
		sums += hex.EncodeToString(sum[:]) + "  " + n + "\n"
	}
	files["SHA256SUMS"] = []byte(sums)
	if mutation == "checksum" {
		files["LICENSE"] = []byte("changed")
	}
	if mutation == "extra" {
		files["unexpected"] = []byte("extra")
	}
	filename := filepath.Join(t.TempDir(), "fixture.tar.gz")
	f, e := os.Create(filename)
	if e != nil {
		t.Fatal(e)
	}
	gz, e := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if e != nil {
		t.Fatal(e)
	}
	tw := tar.NewWriter(gz)
	root := fmt.Sprintf("titanus-%s-linux-%s", meta.Version, runtime.GOARCH)
	for name, data := range files {
		hdr := &tar.Header{Name: root + "/" + name, Mode: 0644, Size: int64(len(data))}
		if mutation == "escape" && name == "LICENSE" {
			hdr.Name = "../outside"
		}
		if mutation == "symlink" && name == "LICENSE" {
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = "/etc/passwd"
			hdr.Size = 0
			data = nil
		}
		if e = tw.WriteHeader(hdr); e != nil {
			t.Fatal(e)
		}
		tw.Write(data)
		if mutation == "duplicate" && name == "LICENSE" {
			tw.WriteHeader(hdr)
			tw.Write(data)
		}
	}
	tw.Close()
	gz.Close()
	f.Close()
	data, _ := os.ReadFile(filename)
	sum := sha256.Sum256(data)
	return filename, hex.EncodeToString(sum[:])
}
func TestArchiveInventoryAndArchitecture(t *testing.T) {
	for _, mutation := range []string{"", "checksum", "extra", "escape", "symlink", "duplicate", "arch"} {
		t.Run(mutation, func(t *testing.T) {
			file, sum := fixture(t, mutation)
			_, e := Verify(file, sum, "0.4.0-rc.4", strings.Repeat("a", 40), runtime.GOARCH)
			if (e == nil) != (mutation == "") {
				t.Fatalf("mutation=%q: %v", mutation, e)
			}
			if _, e = Verify(file, strings.Repeat("0", 64), "0.4.0-rc.4", strings.Repeat("a", 40), runtime.GOARCH); e == nil {
				t.Fatal("untrusted archive accepted")
			}
		})
	}
}
