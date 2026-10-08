package source

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func (m *Manager) Identity(name string) (string, error) {
	if !sourceName.MatchString(name) {
		return "", fmt.Errorf("invalid Source name")
	}
	return manifestIdentity(filepath.Join(m.StateRoot, "sources", name, "rootfs"), name)
}

func manifestIdentity(root, name string) (string, error) {
	manifest, err := buildManifest(name, root)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(manifest.Files)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func syncTree(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		return err
	})
}
