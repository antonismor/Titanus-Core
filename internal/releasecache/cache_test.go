package releasecache

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/version"
)

func archiveFixture(t *testing.T, arch, revision string) []byte {
	t.Helper()
	elf := make([]byte, 64)
	copy(elf, []byte{0x7f, 'E', 'L', 'F', 2, 1, 1})
	binary.LittleEndian.PutUint16(elf[16:], 2)
	machine := uint16(62)
	if arch == "arm64" {
		machine = 183
	}
	binary.LittleEndian.PutUint16(elf[18:], machine)
	binary.LittleEndian.PutUint32(elf[20:], 1)
	binary.LittleEndian.PutUint16(elf[52:], 64)
	meta, _ := json.Marshal(version.Build{Version: "0.4.0-rc.18", Revision: revision, OS: "linux", Arch: arch, StateProfile: version.StateProfile})
	files := map[string][]byte{"manifest.json": meta}
	for _, n := range []string{"bin/titanus", "bin/titanusd", "bin/titanus-agent", "bin/titanus-init"} {
		files[n] = elf
	}
	for _, n := range []string{"systemd/titanusd.service", "systemd/titanus-agent.service", "scripts/install-release.py", "scripts/install-release.sh", "docs/RELEASE.md", "LICENSE"} {
		files[n] = []byte("fixture")
	}
	sums := ""
	for n, b := range files {
		h := sha256.Sum256(b)
		sums += fmt.Sprintf("%x  %s\n", h, n)
	}
	files["SHA256SUMS"] = []byte(sums)
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for n, b := range files {
		if err := tw.WriteHeader(&tar.Header{Name: "titanus-0.4.0-rc.18-linux-" + arch + "/" + n, Mode: 0644, Size: int64(len(b))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(b)
	}
	tw.Close()
	gz.Close()
	return out.Bytes()
}

func TestDownloadsBothNativeArchivesAndReusesVerifiedCacheOffline(t *testing.T) {
	revision := strings.Repeat("a", 40)
	assets := map[string][]byte{}
	for _, arch := range []string{"amd64", "arm64"} {
		name := "titanus-0.4.0-rc.18-linux-" + arch + ".tar.gz"
		b := archiveFixture(t, arch, revision)
		assets[name] = b
		h := sha256.Sum256(b)
		assets[name+".sha256"] = []byte(fmt.Sprintf("%x  %s\n", h, name))
	}
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		b, ok := assets[filepath.Base(r.URL.Path)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	f := Fetcher{Dir: t.TempDir(), Client: server.Client(), baseURL: server.URL}
	a, err := f.Ensure(context.Background(), "0.4.0-rc.18", revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 2 || requests != 4 {
		t.Fatal(a, requests)
	}
	server.Close()
	b, err := f.Ensure(context.Background(), "0.4.0-rc.18", revision)
	if err != nil || len(b) != 2 {
		t.Fatal("cache required a network repair", err)
	}
	for _, arch := range []string{"amd64", "arm64"} {
		if a[arch] != b[arch] {
			t.Fatal("cache identity changed")
		}
	}
	os.WriteFile(a["amd64"].Path, []byte("corrupted"), 0600)
	if _, err = f.Ensure(context.Background(), "0.4.0-rc.18", revision); err == nil {
		t.Fatal("corrupt cache silently trusted")
	}
}

func TestDownloadRejectsChecksumRevisionAndIdentityFailures(t *testing.T) {
	for _, failure := range []string{"digest", "revision", "http", "traversal"} {
		t.Run(failure, func(t *testing.T) {
			revision := strings.Repeat("a", 40)
			bundle := archiveFixture(t, "amd64", revision)
			h := sha256.Sum256(bundle)
			digest := hex.EncodeToString(h[:])
			if failure == "digest" {
				digest = strings.Repeat("0", 64)
			}
			requests := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if failure == "http" {
					w.WriteHeader(503)
					return
				}
				if strings.HasSuffix(r.URL.Path, ".sha256") {
					fmt.Fprintln(w, digest+"  titanus-0.4.0-rc.18-linux-amd64.tar.gz")
				} else {
					w.Write(bundle)
				}
			}))
			defer server.Close()
			f := Fetcher{Dir: t.TempDir(), Client: server.Client(), baseURL: server.URL}
			target := "0.4.0-rc.18"
			if failure == "revision" {
				revision = strings.Repeat("b", 40)
			}
			if failure == "traversal" {
				target = "../../outside"
			}
			if _, err := f.Ensure(context.Background(), target, revision); err == nil {
				t.Fatal("invalid release was accepted")
			}
			if failure == "traversal" && requests != 0 {
				t.Fatal("invalid identity reached network")
			}
			files, _ := filepath.Glob(filepath.Join(f.Dir, "*.tar.gz"))
			if len(files) != 0 {
				t.Fatal("failed verification published an archive", files)
			}
		})
	}
}
