// Package backup implements authenticated offline host backups. External Ceph
// data recovery and a coordinated all-controller recovery set remain separate.
package backup

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/antonismor/Titanus-Core/internal/version"
)

const (
	Format            = "titanus-backup/v1"
	MaxEntries        = 100000
	MaxManifest       = 32 << 20
	MaxBytes    int64 = 1 << 40
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Plan struct {
	Format     string `json:"format"`
	Realm      string `json:"realm"`
	Node       string `json:"node"`
	Role       string `json:"role"`
	StateRoot  string `json:"state_root"`
	ConfigRoot string `json:"config_root"`
	LedgerRoot string `json:"ledger_root"`
}

type Entry struct {
	Path    string `json:"path"`
	Type    string `json:"type"`
	Mode    int64  `json:"mode"`
	UID     int    `json:"uid"`
	GID     int    `json:"gid"`
	Size    int64  `json:"size"`
	MTimeNS int64  `json:"mtime_ns"`
	SHA256  string `json:"sha256,omitempty"`
	Link    string `json:"link,omitempty"`
}

type Manifest struct {
	Cluster       *ClusterBinding `json:"cluster,omitempty"`
	Format        string          `json:"format"`
	ID            string          `json:"id"`
	CreatedAt     time.Time       `json:"created_at"`
	Build         version.Build   `json:"build"`
	Plan          Plan            `json:"plan"`
	RealmRevision uint64          `json:"realm_revision"`
	RealmSHA256   string          `json:"realm_sha256"`
	Entries       []Entry         `json:"entries"`
}

type envelope struct {
	Data json.RawMessage `json:"data"`
	HMAC string          `json:"hmac_sha256"`
}

type FenceRecord struct {
	Realm                  string `json:"realm"`
	Node                   string `json:"node"`
	OldControllersExcluded bool   `json:"old_controllers_excluded"`
	OldWritersExcluded     bool   `json:"old_writers_excluded"`
	GatewaysWithdrawn      bool   `json:"gateways_withdrawn"`
	Evidence               string `json:"evidence"`
}

type Fence struct {
	Format        string      `json:"format"`
	BackupID      string      `json:"backup_id"`
	ArchiveSHA256 string      `json:"archive_sha256"`
	IssuedAt      time.Time   `json:"issued_at"`
	ExpiresAt     time.Time   `json:"expires_at"`
	Record        FenceRecord `json:"record"`
}

func (p Plan) roots() map[string]string {
	return map[string]string{"state": p.StateRoot, "configuration": p.ConfigRoot, "ledger": p.LedgerRoot}
}

func (p Plan) Validate() error {
	if p.Format != "titanus-backup-plan/v1" || !namePattern.MatchString(p.Realm) || !namePattern.MatchString(p.Node) || (p.Role != "controller" && p.Role != "node") {
		return fmt.Errorf("invalid backup plan identity/format/role")
	}
	for _, root := range p.roots() {
		if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
			return fmt.Errorf("backup roots must be clean absolute paths")
		}
		if e := noSymlinkParents(root); e != nil {
			return e
		}
	}
	roots := []string{p.StateRoot, p.ConfigRoot, p.LedgerRoot}
	for i, a := range roots {
		for j, b := range roots {
			if i != j && within(a, b) {
				return fmt.Errorf("backup roots overlap")
			}
		}
	}
	return nil
}

func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}

func noSymlinkParents(path string) error {
	for p := filepath.Clean(path); p != "/"; p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink in backup/restore path: %s", p)
		}
	}
	return nil
}

func strictJSON(data []byte, target any) error {
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if e := d.Decode(target); e != nil {
		return fmt.Errorf("invalid backup JSON: %w", e)
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return fmt.Errorf("trailing backup JSON")
	}
	return nil
}

func Keygen(path string) error {
	key := make([]byte, 32)
	if _, e := rand.Read(key); e != nil {
		return e
	}
	defer clear(key)
	return createPrivate(path, key)
}

func loadKey(path string) ([]byte, error) {
	f, e := openRegular(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || st.Size() != 32 || st.Mode().Perm() != 0600 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, fmt.Errorf("backup key must be an owned 0600 32-byte regular file")
	}
	return io.ReadAll(f)
}

func openRegular(path string) (*os.File, error) {
	if e := noSymlinkParents(path); e != nil {
		return nil, e
	}
	fd, e := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), path)
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("regular file required: %s", path)
	}
	return f, nil
}

func sign(key []byte, value any) ([]byte, error) {
	data, e := json.Marshal(value)
	if e != nil {
		return nil, e
	}
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return json.Marshal(envelope{data, hex.EncodeToString(h.Sum(nil))})
}

func authenticate(key, data []byte, target any) error {
	var v envelope
	if e := strictJSON(data, &v); e != nil {
		return e
	}
	mac, e := hex.DecodeString(v.HMAC)
	if e != nil {
		return fmt.Errorf("invalid backup authentication")
	}
	h := hmac.New(sha256.New, key)
	h.Write(v.Data)
	if !hmac.Equal(h.Sum(nil), mac) {
		return fmt.Errorf("backup authentication failed")
	}
	return strictJSON(v.Data, target)
}

func createPrivate(path string, data []byte) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("absolute output path required")
	}
	if e := noSymlinkParents(path); e != nil {
		return e
	}
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(data)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		os.Remove(path)
		return e
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

func digestFile(path string) (string, error) {
	f, e := openRegular(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func LoadPlan(path string) (Plan, error) {
	var p Plan
	data, e := os.ReadFile(path)
	if e != nil || len(data) > 64<<10 {
		return p, fmt.Errorf("backup plan unavailable/too large")
	}
	if e = strictJSON(data, &p); e != nil {
		return p, e
	}
	return p, p.Validate()
}
