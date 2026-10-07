package source

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
)

var sourceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type Manager struct {
	StateRoot string
}

func NewManager(stateRoot string) *Manager {
	if stateRoot == "" {
		stateRoot = "/var/lib/titanus"
	}
	return &Manager{StateRoot: stateRoot}
}

func (m *Manager) ImportDirectory(name, sourceDir string) error {
	if !sourceName.MatchString(name) {
		return fmt.Errorf("invalid Source name %q", name)
	}
	info, err := os.Stat(sourceDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", sourceDir)
	}

	dest := filepath.Join(m.StateRoot, "sources", name)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("Source %s already exists", name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	rootfs := filepath.Join(dest, "rootfs")
	if err := os.MkdirAll(rootfs, 0755); err != nil {
		return err
	}
	if err := copyTree(sourceDir, rootfs); err != nil {
		_ = os.RemoveAll(dest)
		return err
	}
	return nil
}

func (m *Manager) List() ([]string, error) {
	root := filepath.Join(m.StateRoot, "sources")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			if info, err := os.Stat(filepath.Join(root, entry.Name(), "rootfs")); err == nil && info.IsDir() {
				out = append(out, entry.Name())
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}

		switch {
		case info.IsDir():
			if err := os.MkdirAll(target, info.Mode().Perm()); err != nil {
				return err
			}
			return preserveOwner(target, info)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			if err := copyFile(path, target, info.Mode().Perm()); err != nil {
				return err
			}
			return preserveOwner(target, info)
		default:
			return fmt.Errorf("Source contains unsupported special file %s (%s)", path, info.Mode())
		}
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func preserveOwner(path string, info os.FileInfo) error {
	if os.Geteuid() != 0 {
		return nil
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return os.Chown(path, int(st.Uid), int(st.Gid))
}
