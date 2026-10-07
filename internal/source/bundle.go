package source

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Manifest struct {
	Format    string     `json:"format"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	Files     []FileHash `json:"files"`
}

type FileHash struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func (m *Manager) Export(name string, writer io.Writer) error {
	root := filepath.Join(m.StateRoot, "sources", name, "rootfs")
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		if err != nil {
			return err
		}
		return fmt.Errorf("Source %s rootfs is unavailable", name)
	}
	manifest, err := buildManifest(name, root)
	if err != nil {
		return err
	}

	gz := gzip.NewWriter(writer)
	tw := tar.NewWriter(gz)
	closeWith := func(prior error) error {
		if err := tw.Close(); prior == nil && err != nil {
			prior = err
		}
		if err := gz.Close(); prior == nil && err != nil {
			prior = err
		}
		return prior
	}

	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return closeWith(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: "titanus-source.json", Mode: 0644, Size: int64(len(manifestData)),
		ModTime: manifest.CreatedAt,
	}); err != nil {
		return closeWith(err)
	}
	if _, err := tw.Write(manifestData); err != nil {
		return closeWith(err)
	}

	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(filepath.Join("rootfs", rel))
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = name
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(tw, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			return closeErr
		}
		return nil
	})
	return closeWith(err)
}

func (m *Manager) ImportBundle(reader io.Reader) (Manifest, error) {
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return Manifest{}, fmt.Errorf("open Titanus Source bundle: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	tempRoot, err := os.MkdirTemp(filepath.Join(m.StateRoot, "sources"), ".titanus-import-")
	if err != nil {
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(filepath.Join(m.StateRoot, "sources"), 0750); mkErr != nil {
				return Manifest{}, mkErr
			}
			tempRoot, err = os.MkdirTemp(filepath.Join(m.StateRoot, "sources"), ".titanus-import-")
		}
		if err != nil {
			return Manifest{}, err
		}
	}
	defer os.RemoveAll(tempRoot)

	var manifest Manifest
	manifestSeen := false
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Manifest{}, err
		}
		clean := filepath.Clean(header.Name)
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return Manifest{}, fmt.Errorf("unsafe Source bundle path %q", header.Name)
		}
		if clean == "titanus-source.json" {
			data, err := io.ReadAll(io.LimitReader(tr, 4<<20))
			if err != nil {
				return Manifest{}, err
			}
			if err := json.Unmarshal(data, &manifest); err != nil {
				return Manifest{}, fmt.Errorf("invalid Source manifest: %w", err)
			}
			manifestSeen = true
			continue
		}
		if !strings.HasPrefix(filepath.ToSlash(clean), "rootfs/") {
			return Manifest{}, fmt.Errorf("unexpected Source bundle entry %q", header.Name)
		}
		target := filepath.Join(tempRoot, clean)
		if !strings.HasPrefix(target, tempRoot+string(os.PathSeparator)) {
			return Manifest{}, fmt.Errorf("unsafe Source target %q", target)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return Manifest{}, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
				return Manifest{}, err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(header.Mode))
			if err != nil {
				return Manifest{}, err
			}
			if _, err := io.CopyN(file, tr, header.Size); err != nil {
				_ = file.Close()
				return Manifest{}, err
			}
			if err := file.Close(); err != nil {
				return Manifest{}, err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(header.Linkname) {
				// Absolute links are valid inside a rootfs; preserve the link text.
			}
			if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
				return Manifest{}, err
			}
			if err := os.Symlink(header.Linkname, target); err != nil {
				return Manifest{}, err
			}
		default:
			return Manifest{}, fmt.Errorf("unsupported Source bundle entry type %d for %s", header.Typeflag, header.Name)
		}
	}
	if !manifestSeen || manifest.Format != "titanus-source/v1" || !sourceName.MatchString(manifest.Name) {
		return Manifest{}, fmt.Errorf("invalid or missing Titanus Source manifest")
	}
	root := filepath.Join(tempRoot, "rootfs")
	if err := verifyManifest(manifest, root); err != nil {
		return Manifest{}, err
	}
	final := filepath.Join(m.StateRoot, "sources", manifest.Name)
	if _, err := os.Stat(final); err == nil {
		return Manifest{}, fmt.Errorf("Source %s already exists", manifest.Name)
	}
	if err := os.Rename(tempRoot, final); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func buildManifest(name, root string) (Manifest, error) {
	manifest := Manifest{Format: "titanus-source/v1", Name: name, CreatedAt: time.Now().UTC()}
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		hash, err := hashFile(path)
		if err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, FileHash{Path: filepath.ToSlash(rel), Size: info.Size(), SHA256: hash})
		return nil
	})
	sort.Slice(manifest.Files, func(i, j int) bool { return manifest.Files[i].Path < manifest.Files[j].Path })
	return manifest, err
}

func verifyManifest(manifest Manifest, root string) error {
	for _, expected := range manifest.Files {
		path := filepath.Join(root, filepath.FromSlash(expected.Path))
		if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe manifest path %q", expected.Path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("manifest file %s: %w", expected.Path, err)
		}
		if info.Size() != expected.Size {
			return fmt.Errorf("Source integrity mismatch for %s: size", expected.Path)
		}
		hash, err := hashFile(path)
		if err != nil {
			return err
		}
		if hash != expected.SHA256 {
			return fmt.Errorf("Source integrity mismatch for %s: SHA-256", expected.Path)
		}
	}
	return nil
}

func hashFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
