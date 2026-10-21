// Package releasecache prepares the official, verified installer archives locally.
// It never contacts a Realm node, extracts a bundle or starts provisioning.
package releasecache

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/release"
)

const DefaultDir = "/var/lib/titanus/installer-web/releases"
const officialURL = "https://github.com/antonismor/Titanus-Core/releases/download"

type Fetcher struct {
	Dir     string
	Client  *http.Client
	baseURL string // Test servers only; production always uses the official repository.
}

func Directory(version, revision string) string {
	return filepath.Join(DefaultDir, version+"-"+revision)
}

func validIdentity(version, revision string) bool {
	return regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[a-z0-9.-]+)?$`).MatchString(version) && regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(revision)
}

// Lookup verifies the cache again, including internal checksums and ELF inventory.
func Lookup(dir, version, revision, arch string) (model.Archive, error) {
	var a model.Archive
	if !validIdentity(version, revision) || (arch != "amd64" && arch != "arm64") {
		return a, fmt.Errorf("exact release identity and native architecture required")
	}
	a.Path = filepath.Join(dir, fmt.Sprintf("titanus-%s-linux-%s.tar.gz", version, arch))
	for _, p := range []string{a.Path, a.Path + ".sha256"} {
		st, err := os.Lstat(p)
		if err != nil {
			return model.Archive{}, err
		}
		if !st.Mode().IsRegular() {
			return model.Archive{}, fmt.Errorf("release cache requires regular files")
		}
	}
	b, err := os.ReadFile(a.Path + ".sha256")
	if err != nil {
		return model.Archive{}, err
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 || fields[1] != filepath.Base(a.Path) {
		return model.Archive{}, fmt.Errorf("invalid published checksum record")
	}
	a.SHA256 = fields[0]
	if _, err = release.Verify(a.Path, a.SHA256, version, revision, arch); err != nil {
		return model.Archive{}, err
	}
	return a, nil
}

func (f Fetcher) Ensure(ctx context.Context, version, revision string) (map[string]model.Archive, error) {
	if !validIdentity(version, revision) {
		return nil, fmt.Errorf("exact published version and tested revision required")
	}
	if f.Dir == "" {
		f.Dir = Directory(version, revision)
	}
	if err := os.MkdirAll(f.Dir, 0700); err != nil {
		return nil, err
	}
	st, err := os.Lstat(f.Dir)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0022 != 0 {
		return nil, fmt.Errorf("private, non-symlinked release cache directory required")
	}
	fd, err := syscall.Open(filepath.Join(f.Dir, ".download.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), "release-cache-lock")
	defer lock.Close()
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("unsafe release cache lock")
	}
	// Nonblocking acquisition lets cancellation interrupt a concurrent download wait.
	for {
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	if f.Client == nil {
		f.Client = &http.Client{Timeout: 5 * time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if len(via) > 8 || r.URL.Scheme != "https" {
				return fmt.Errorf("unsafe release redirect")
			}
			return nil
		}}
	}
	if f.baseURL == "" {
		f.baseURL = officialURL
	}
	archives := map[string]model.Archive{}
	for _, arch := range []string{"amd64", "arm64"} {
		a, err := Lookup(f.Dir, version, revision, arch)
		if err == nil {
			archives[arch] = a
			continue
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("%s cached release failed verification: %w", arch, err)
		}
		name := fmt.Sprintf("titanus-%s-linux-%s.tar.gz", version, arch)
		sum, err := f.get(ctx, f.baseURL+"/v"+version+"/"+name+".sha256", 4096)
		if err != nil {
			return nil, err
		}
		fields := strings.Fields(string(sum))
		if len(fields) != 2 || fields[1] != name || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(fields[0]) {
			return nil, fmt.Errorf("invalid published %s checksum", arch)
		}
		tmp, err := os.CreateTemp(f.Dir, ".archive-")
		if err != nil {
			return nil, err
		}
		err = f.download(ctx, f.baseURL+"/v"+version+"/"+name, tmp)
		closeErr := tmp.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			_, err = release.Verify(tmp.Name(), fields[0], version, revision, arch)
		}
		if err != nil {
			os.Remove(tmp.Name())
			return nil, fmt.Errorf("%s download verification failed: %w", arch, err)
		}
		path := filepath.Join(f.Dir, name)
		if err = os.Rename(tmp.Name(), path); err != nil {
			os.Remove(tmp.Name())
			return nil, err
		}
		sumfile, err := os.CreateTemp(f.Dir, ".checksum-")
		if err != nil {
			return nil, err
		}
		_, err = sumfile.WriteString(fields[0] + "  " + name + "\n")
		if err == nil {
			err = sumfile.Sync()
		}
		closeErr = sumfile.Close()
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Rename(sumfile.Name(), path+".sha256")
		}
		if err != nil {
			os.Remove(sumfile.Name())
			return nil, err
		}
		archives[arch] = model.Archive{Path: path, SHA256: fields[0]}
	}
	return archives, nil
}

func (f Fetcher) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := f.Client.Do(r)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("official release download returned HTTP %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("release response exceeds limit")
	}
	return b, err
}

func (f Fetcher) download(ctx context.Context, url string, out *os.File) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := f.Client.Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("official release download returned HTTP %d", res.StatusCode)
	}
	n, err := io.Copy(out, io.LimitReader(res.Body, (256<<20)+1))
	if err != nil {
		return err
	}
	if n > 256<<20 {
		return fmt.Errorf("release archive exceeds limit")
	}
	return out.Sync()
}
