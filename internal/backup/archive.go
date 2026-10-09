package backup

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/version"
)

func inventory(p Plan) ([]Entry, error) {
	entries := []Entry{}
	var total int64
	for prefix, root := range p.roots() {
		e := filepath.WalkDir(root, func(file string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			st, e := os.Lstat(file)
			if e != nil {
				return e
			}
			if st.Mode().IsRegular() && st.Sys().(*syscall.Stat_t).Nlink != 1 {
				return fmt.Errorf("hard-linked data requires a different backup profile: %s", file)
			}
			if n, e := unix.Llistxattr(file, nil); e != nil || n != 0 {
				return fmt.Errorf("extended attributes require a different backup profile: %s", file)
			}
			rel, e := filepath.Rel(root, file)
			if e != nil {
				return e
			}
			name := prefix
			if rel != "." {
				name += "/" + filepath.ToSlash(rel)
			}
			h, e := tar.FileInfoHeader(st, "")
			if e != nil {
				return e
			}
			v := Entry{Path: name, Mode: h.Mode, UID: h.Uid, GID: h.Gid, MTimeNS: st.ModTime().UnixNano()}
			switch {
			case st.IsDir():
				v.Type = "directory"
			case st.Mode().IsRegular():
				v.Type = "file"
				v.Size = st.Size()
				total += v.Size
				if v.Size < 0 || total > MaxBytes {
					return fmt.Errorf("backup size limit exceeded")
				}
				v.SHA256, e = digestFile(file)
				if e != nil {
					return e
				}
			case st.Mode()&os.ModeSymlink != 0:
				if strings.HasPrefix(name, "state/realm/") || strings.HasPrefix(name, "state/storage/") || strings.HasSuffix(name, ".json") {
					return fmt.Errorf("state metadata cannot be a symlink")
				}
				v.Type = "symlink"
				v.Link, e = os.Readlink(file)
				if e != nil {
					return e
				}
				if prefix == "ledger" || (prefix == "configuration" && !safeConfigLink(name, v.Link)) {
					return fmt.Errorf("configuration symlink escapes its backed-up root")
				}
			default:
				return fmt.Errorf("backup refuses special file %s; offline migration required", file)
			}
			entries = append(entries, v)
			if len(entries) > MaxEntries {
				return fmt.Errorf("backup inventory limit exceeded")
			}
			return nil
		})
		if e != nil {
			return nil, e
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func (m Manifest) validate() error {
	if m.Format != Format || !hashPattern.MatchString(m.ID) || !hashPattern.MatchString(m.RealmSHA256) || m.CreatedAt.IsZero() || m.Build.OS != "linux" || m.Build.StateProfile != version.StateProfile || (m.Build.Arch != "amd64" && m.Build.Arch != "arm64") {
		return fmt.Errorf("unsupported backup format/profile/identity")
	}
	if e := m.Plan.Validate(); e != nil {
		return e
	}
	if len(m.Entries) < 3 || len(m.Entries) > MaxEntries {
		return fmt.Errorf("invalid backup inventory size")
	}
	seen := map[string]string{}
	var total int64
	for _, v := range m.Entries {
		root := strings.Split(v.Path, "/")[0]
		if _, ok := m.Plan.roots()[root]; !ok || path.Clean(v.Path) != v.Path || path.IsAbs(v.Path) || strings.Contains(v.Path, "\\") || strings.ContainsRune(v.Path, 0) || seen[v.Path] != "" || v.UID < 0 || v.GID < 0 || v.Mode < 0 || v.Mode > 07777 || v.Size < 0 {
			return fmt.Errorf("invalid inventory entry")
		}
		if v.Path != root && seen[path.Dir(v.Path)] != "directory" {
			return fmt.Errorf("unordered/unsafe inventory parent")
		}
		switch v.Type {
		case "file":
			if !hashPattern.MatchString(v.SHA256) || v.Link != "" {
				return fmt.Errorf("invalid file inventory")
			}
			total += v.Size
			if total > MaxBytes {
				return fmt.Errorf("backup size limit exceeded")
			}
		case "directory":
			if v.Size != 0 || v.SHA256 != "" || v.Link != "" {
				return fmt.Errorf("invalid directory inventory")
			}
		case "symlink":
			if root == "ledger" || (root == "configuration" && !safeConfigLink(v.Path, v.Link)) || v.Size != 0 || v.SHA256 != "" || v.Link == "" || strings.ContainsRune(v.Link, 0) {
				return fmt.Errorf("invalid link inventory")
			}
		default:
			return fmt.Errorf("unsupported inventory type")
		}
		seen[v.Path] = v.Type
	}
	for root := range m.Plan.roots() {
		if seen[root] != "directory" {
			return fmt.Errorf("missing inventory root")
		}
	}
	return nil
}

func safeConfigLink(name, link string) bool {
	if path.IsAbs(link) || strings.Contains(link, "\\") || strings.ContainsRune(link, 0) {
		return false
	}
	resolved := path.Clean(path.Join(path.Dir(name), link))
	return strings.HasPrefix(resolved, "configuration/")
}

func Create(p Plan, archive, keyPath string) (Manifest, error) {
	lock, e := offline.Exclusive()
	if e != nil {
		return Manifest{}, e
	}
	defer lock.Close()
	if e := rejectRunningActors(); e != nil {
		return Manifest{}, e
	}
	return create(p, archive, keyPath)
}

func create(p Plan, archive, keyPath string) (result Manifest, err error) {
	if err = p.Validate(); err != nil {
		return
	}
	for _, file := range []string{archive, keyPath} {
		if !filepath.IsAbs(file) || filepath.Clean(file) != file {
			return result, fmt.Errorf("absolute archive/key path required")
		}
		for _, root := range p.roots() {
			if within(file, root) {
				return result, fmt.Errorf("archive and authentication key must be outside backed-up roots")
			}
		}
	}
	if err = offline.CheckStartup(p.StateRoot); err != nil {
		return
	}
	key, e := loadKey(keyPath)
	if e != nil {
		return result, e
	}
	defer clear(key)
	locks, e := holdLocalLocks(p)
	if e != nil {
		return result, e
	}
	defer func() {
		for _, f := range locks {
			f.Close()
		}
	}()
	state, e := validateHost(p)
	if e != nil {
		return result, e
	}
	stateBytes, _ := json.Marshal(state)
	entries, e := inventory(p)
	if e != nil {
		return result, e
	}
	if e = validateMappedOwners(entries, p.LedgerRoot); e != nil {
		return result, e
	}
	id := make([]byte, 32)
	if _, e = rand.Read(id); e != nil {
		return result, e
	}
	result = Manifest{Format: Format, ID: hex.EncodeToString(id), CreatedAt: time.Now().UTC(), Build: version.Info(), Plan: p, RealmRevision: state.Revision, RealmSHA256: fmt.Sprintf("%x", sha256.Sum256(stateBytes)), Entries: entries}
	if err = result.validate(); err != nil {
		return
	}
	signed, e := sign(key, result)
	if e != nil || len(signed) > MaxManifest {
		return result, fmt.Errorf("backup manifest exceeds limit")
	}
	if err = createPrivate(p.StateRoot+".backup-pending", []byte(result.ID)); err != nil {
		return
	}
	defer func() { os.Remove(p.StateRoot + ".backup-pending"); syncDir(filepath.Dir(p.StateRoot)) }()
	if err = noSymlinkParents(archive); err != nil {
		return
	}
	if _, e = os.Lstat(archive); !os.IsNotExist(e) {
		return result, fmt.Errorf("backup output already exists")
	}
	tmp, e := os.CreateTemp(filepath.Dir(archive), ".titanus-backup-")
	if e != nil {
		return result, e
	}
	defer func() { tmp.Close(); os.Remove(tmp.Name()) }()
	if err = tmp.Chmod(0600); err != nil {
		return
	}
	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	if err = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(signed))}); err != nil {
		return
	}
	if _, err = tw.Write(signed); err != nil {
		return
	}
	for _, entry := range entries {
		h := header(entry)
		if err = tw.WriteHeader(h); err != nil {
			return
		}
		if entry.Type == "file" {
			f, e := openRegular(entrySource(p, entry.Path))
			if e != nil {
				return result, e
			}
			hash := sha256.New()
			n, e := io.Copy(io.MultiWriter(tw, hash), f)
			f.Close()
			if e != nil || n != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
				return result, fmt.Errorf("file changed during backup: %s", entry.Path)
			}
		}
	}
	if err = tw.Close(); err != nil {
		return
	}
	if err = gz.Close(); err != nil {
		return
	}
	after, e := inventory(p)
	if e != nil || !reflect.DeepEqual(entries, after) {
		return result, fmt.Errorf("host changed during offline backup")
	}
	if err = tmp.Sync(); err != nil {
		return
	}
	if err = tmp.Close(); err != nil {
		return
	}
	// Link publishes without replacing a concurrently created output.
	if err = os.Link(tmp.Name(), archive); err != nil {
		return
	}
	err = syncDir(filepath.Dir(archive))
	return
}

func entrySource(p Plan, name string) string {
	root, rel, _ := strings.Cut(name, "/")
	return filepath.Join(p.roots()[root], filepath.FromSlash(rel))
}

func header(v Entry) *tar.Header {
	h := &tar.Header{Name: v.Path, Mode: v.Mode, Uid: v.UID, Gid: v.GID, Size: v.Size, ModTime: time.Unix(0, v.MTimeNS), Format: tar.FormatPAX}
	switch v.Type {
	case "file":
		h.Typeflag = tar.TypeReg
	case "directory":
		h.Typeflag = tar.TypeDir
	case "symlink":
		h.Typeflag = tar.TypeSymlink
		h.Linkname = v.Link
	}
	return h
}

// readArchive authenticates every byte that can be restored. Extraction goes
// only to fresh private staging roots and never follows a payload symlink.
func readArchive(archive string, key []byte, staging map[string]string) (Manifest, error) {
	var m Manifest
	f, e := openRegular(archive)
	if e != nil {
		return m, e
	}
	defer f.Close()
	buffer := bufio.NewReader(f)
	gz, e := gzip.NewReader(buffer)
	if e != nil {
		return m, e
	}
	defer gz.Close()
	gz.Multistream(false)
	t := tar.NewReader(gz)
	h, e := t.Next()
	if e != nil || h.Name != "manifest.json" || h.Typeflag != tar.TypeReg || h.Size < 1 || h.Size > MaxManifest {
		return m, fmt.Errorf("missing bounded authenticated manifest")
	}
	data, e := io.ReadAll(t)
	if e != nil {
		return m, e
	}
	if e = authenticate(key, data, &m); e != nil {
		return m, e
	}
	if e = m.validate(); e != nil {
		return m, e
	}
	for _, v := range m.Entries {
		h, e = t.Next()
		if e != nil {
			return m, fmt.Errorf("incomplete backup payload")
		}
		want := header(v)
		if h.Name != want.Name || h.Typeflag != want.Typeflag || h.Mode != want.Mode || h.Uid != want.Uid || h.Gid != want.Gid || h.Size != want.Size || h.Linkname != want.Linkname || !h.ModTime.Equal(want.ModTime) {
			return m, fmt.Errorf("payload metadata disagrees with authenticated inventory: %s", v.Path)
		}
		var output *os.File
		if staging != nil {
			root, rel, _ := strings.Cut(v.Path, "/")
			dst := filepath.Join(staging[root], filepath.FromSlash(rel))
			if e = noSymlinkParents(filepath.Dir(dst)); e != nil {
				return m, e
			}
			switch v.Type {
			case "directory":
				if rel != "" {
					e = os.Mkdir(dst, 0700)
				}
			case "symlink":
				e = os.Symlink(v.Link, dst)
			case "file":
				output, e = os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			}
			if e != nil {
				return m, e
			}
		}
		if v.Type == "file" {
			hash := sha256.New()
			var writer io.Writer = hash
			if output != nil {
				writer = io.MultiWriter(output, hash)
			}
			n, e := io.Copy(writer, t)
			if output != nil {
				if e == nil {
					e = output.Sync()
				}
				ce := output.Close()
				if e == nil {
					e = ce
				}
			}
			if e != nil || n != v.Size || hex.EncodeToString(hash.Sum(nil)) != v.SHA256 {
				return m, fmt.Errorf("backup content checksum failed: %s", v.Path)
			}
		}
	}
	if _, e = t.Next(); e != io.EOF {
		return m, fmt.Errorf("unlisted/trailing backup member")
	}
	if extra, e := io.ReadAll(io.LimitReader(gz, 1)); e != nil || len(extra) != 0 {
		return m, fmt.Errorf("invalid gzip trailer/extra payload")
	}
	if _, e = buffer.Peek(1); e != io.EOF {
		return m, fmt.Errorf("trailing compressed data")
	}
	if staging != nil {
		// Apply directory metadata last so temporary parents stay private.
		for i := len(m.Entries) - 1; i >= 0; i-- {
			v := m.Entries[i]
			root, rel, _ := strings.Cut(v.Path, "/")
			dst := filepath.Join(staging[root], filepath.FromSlash(rel))
			if e = os.Lchown(dst, v.UID, v.GID); e != nil {
				return m, e
			}
			if v.Type != "symlink" {
				if e = os.Chmod(dst, fileMode(v.Mode)); e != nil {
					return m, e
				}
				if e = os.Chtimes(dst, time.Unix(0, v.MTimeNS), time.Unix(0, v.MTimeNS)); e != nil {
					return m, e
				}
			}
			if v.Type == "directory" {
				if e = syncDir(dst); e != nil {
					return m, e
				}
			}
		}
	}
	return m, nil
}

func fileMode(mode int64) os.FileMode {
	m := os.FileMode(mode & 0777)
	if mode&04000 != 0 {
		m |= os.ModeSetuid
	}
	if mode&02000 != 0 {
		m |= os.ModeSetgid
	}
	if mode&01000 != 0 {
		m |= os.ModeSticky
	}
	return m
}

func Verify(archive, keyPath string) (Manifest, error) {
	key, e := loadKey(keyPath)
	if e != nil {
		return Manifest{}, e
	}
	defer clear(key)
	return readArchive(archive, key, nil)
}

func lockFile(path string) (*os.File, error) {
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	if e = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("offline data is busy: %s", path)
	}
	return f, nil
}

func holdLocalLocks(p Plan) (files []*os.File, err error) {
	paths := []string{filepath.Join(p.StateRoot, "runtime.lock"), filepath.Join(p.LedgerRoot, "lock")}
	disks, e := os.ReadDir(filepath.Join(p.StateRoot, "disks"))
	if e != nil && !os.IsNotExist(e) {
		return nil, e
	}
	for _, d := range disks {
		paths = append(paths, filepath.Join(p.StateRoot, "disks", d.Name(), "control", "lock"))
	}
	defer func() {
		if err != nil {
			for _, f := range files {
				f.Close()
			}
		}
	}()
	for _, p := range paths {
		f, e := lockFile(p)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return files, e
		}
		files = append(files, f)
	}
	return files, nil
}
