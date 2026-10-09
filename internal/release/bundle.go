// Package release verifies downloaded native archives before any host mutation.
package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/antonismor/Titanus-Core/internal/version"
)

// Verify binds the full archive to an independently supplied SHA256, inventories
// every member, checks the internal checksums and ELF architecture without
// executing a foreign architecture binary or extracting an untrusted path.
func Verify(filename, digest, wantVersion, revision, arch string) (version.Build, error) {
	var meta version.Build
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
		return meta, fmt.Errorf("pinned archive SHA256 required")
	}
	f, err := os.Open(filename)
	if err != nil {
		return meta, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return meta, err
	}
	if hex.EncodeToString(h.Sum(nil)) != digest {
		return meta, fmt.Errorf("archive SHA256 mismatch")
	}
	if _, err = f.Seek(0, 0); err != nil {
		return meta, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		return meta, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string][]byte{}
	prefix := ""
	total := int64(0)
	seen := map[string]bool{}
	for {
		hdr, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return meta, e
		}
		name := strings.TrimSuffix(hdr.Name, "/")
		if path.Clean(name) != name || path.IsAbs(name) || strings.HasPrefix(name, "../") || seen[name] {
			return meta, fmt.Errorf("unsafe or duplicate archive member")
		}
		seen[name] = true
		root, sub, found := strings.Cut(name, "/")
		if prefix == "" {
			prefix = root
		}
		if root != prefix {
			return meta, fmt.Errorf("multiple archive roots")
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if !found || hdr.Typeflag != tar.TypeReg || hdr.Size < 0 || hdr.Size > 256<<20 {
			return meta, fmt.Errorf("unsupported archive member")
		}
		total += hdr.Size
		if total > 1<<30 {
			return meta, fmt.Errorf("archive too large")
		}
		data, e := io.ReadAll(io.LimitReader(tr, hdr.Size+1))
		if e != nil || int64(len(data)) != hdr.Size {
			return meta, fmt.Errorf("incomplete archive member")
		}
		files[sub] = data
	}
	if _, err = io.Copy(io.Discard, gz); err != nil {
		return meta, fmt.Errorf("invalid gzip trailer: %w", err)
	}
	expected := map[string]bool{}
	for _, n := range []string{"bin/titanus", "bin/titanusd", "bin/titanus-agent", "bin/titanus-init", "systemd/titanusd.service", "systemd/titanus-agent.service", "scripts/install-release.py", "scripts/install-release.sh", "docs/RELEASE.md", "LICENSE", "manifest.json"} {
		expected[n] = true
	}
	lines := strings.Split(strings.TrimSpace(string(files["SHA256SUMS"])), "\n")
	seen = map[string]bool{}
	for _, line := range lines {
		hash, name, ok := strings.Cut(line, "  ")
		data, exists := files[name]
		actual := sha256.Sum256(data)
		if !ok || !exists || !expected[name] || seen[name] || hash != hex.EncodeToString(actual[:]) {
			return meta, fmt.Errorf("invalid bundle checksum inventory")
		}
		seen[name] = true
	}
	if len(seen) != len(expected) || len(files) != len(expected)+1 {
		return meta, fmt.Errorf("incomplete or extra bundle files")
	}
	if err = json.Unmarshal(files["manifest.json"], &meta); err != nil {
		return meta, err
	}
	if meta.Version != wantVersion || meta.Revision != revision || meta.OS != "linux" || meta.Arch != arch || meta.StateProfile != version.StateProfile {
		return meta, fmt.Errorf("bundle manifest differs from selected version/revision/architecture/profile")
	}
	if prefix != fmt.Sprintf("titanus-%s-linux-%s", wantVersion, arch) {
		return meta, fmt.Errorf("invalid archive root")
	}
	machine := elf.EM_X86_64
	if arch == "arm64" {
		machine = elf.EM_AARCH64
	} else if arch != "amd64" {
		return meta, fmt.Errorf("unsupported architecture")
	}
	for _, name := range []string{"titanus", "titanusd", "titanus-agent", "titanus-init"} {
		binary, e := elf.NewFile(bytes.NewReader(files["bin/"+name]))
		if e != nil {
			return meta, e
		}
		if binary.Machine != machine || binary.Class != elf.ELFCLASS64 {
			return meta, fmt.Errorf("ELF architecture mismatch")
		}
		binary.Close()
	}
	return meta, nil
}
