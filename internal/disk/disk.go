package disk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Provider string

const (
	ProviderLocal   Provider = "local"
	ProviderCephRBD Provider = "ceph-rbd"
	ProviderCephFS  Provider = "cephfs"
)

var diskName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type Spec struct {
	ManagedID     string    `json:"managed_id,omitempty"`
	Name          string    `json:"name"`
	Provider      Provider  `json:"provider"`
	SizeBytes     int64     `json:"size_bytes"`
	CreatedAt     time.Time `json:"created_at"`
	Initialized   bool      `json:"initialized"`
	LayoutVersion int       `json:"layout_version"`
	RemotePath    string    `json:"remote_path,omitempty"`
}

type Mount struct {
	Disk     string `json:"disk"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

type CephConfig struct {
	Cluster string `json:"cluster"`
	Pool    string `json:"rbd_pool"`
	FSName  string `json:"cephfs_name"`
	Client  string `json:"client"`
	Conf    string `json:"conf"`
	Keyring string `json:"keyring,omitempty"`
}

type Manager struct {
	StateRoot string
}

func NewManager(stateRoot string) *Manager {
	if strings.TrimSpace(stateRoot) == "" {
		stateRoot = "/var/lib/titanus"
	}
	return &Manager{StateRoot: filepath.Clean(stateRoot)}
}

func (m *Manager) ConfigureCeph(cfg CephConfig) error {
	if cfg.Cluster == "" {
		cfg.Cluster = "ceph"
	}
	if cfg.Pool == "" {
		cfg.Pool = "titanus"
	}
	if cfg.FSName == "" {
		cfg.FSName = "cephfs"
	}
	if cfg.Client == "" {
		cfg.Client = "client.titanus"
	}
	if cfg.Conf == "" {
		cfg.Conf = "/etc/ceph/ceph.conf"
	}
	if err := os.MkdirAll(m.storageDir(), 0750); err != nil {
		return err
	}
	return writeJSON(m.cephConfigPath(), cfg, 0600)
}

func (m *Manager) CephConfig() (CephConfig, error) {
	var cfg CephConfig
	if err := readJSON(m.cephConfigPath(), &cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, fmt.Errorf("Ceph adapter is not configured")
		}
		return cfg, err
	}
	return cfg, nil
}

func (m *Manager) Create(spec Spec) (Spec, error) {
	unlock, err := m.lock(spec.Name)
	if err != nil {
		return Spec{}, err
	}
	defer unlock()
	return m.create(spec)
}
func (m *Manager) create(spec Spec) (Spec, error) {
	if spec.ManagedID != "" {
		return Spec{}, fmt.Errorf("new Disk cannot impersonate managed catalog")
	}
	spec.Name = strings.TrimSpace(spec.Name)
	if !diskName.MatchString(spec.Name) {
		return Spec{}, fmt.Errorf("invalid Disk name %q", spec.Name)
	}
	if spec.SizeBytes <= 0 {
		return Spec{}, fmt.Errorf("Disk size must be greater than zero")
	}
	if spec.Provider == "" {
		spec.Provider = ProviderLocal
	}
	switch spec.Provider {
	case ProviderLocal, ProviderCephRBD, ProviderCephFS:
	default:
		return Spec{}, fmt.Errorf("unsupported Disk provider %q", spec.Provider)
	}
	if _, err := os.Lstat(m.diskDir(spec.Name)); err == nil {
		return Spec{}, fmt.Errorf("Disk %s already exists", spec.Name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Spec{}, err
	}

	spec.CreatedAt = time.Now().UTC()
	spec.LayoutVersion = 1
	if err := os.MkdirAll(m.diskDir(spec.Name), 0750); err != nil {
		return Spec{}, err
	}

	switch spec.Provider {
	case ProviderLocal:
		if err := os.MkdirAll(m.dataPath(spec.Name), 0750); err != nil {
			_ = os.RemoveAll(m.diskDir(spec.Name))
			return Spec{}, err
		}
		spec.Initialized = true

	case ProviderCephRBD:
		cfg, err := m.CephConfig()
		if err != nil {
			_ = os.RemoveAll(m.diskDir(spec.Name))
			return Spec{}, err
		}
		if err := requireCommands("rbd", "mount", "umount", "mkfs.ext4", "blkid"); err != nil {
			_ = os.RemoveAll(m.diskDir(spec.Name))
			return Spec{}, err
		}
		sizeMiB := (spec.SizeBytes + 1024*1024 - 1) / (1024 * 1024)
		args := append(m.rbdBaseArgs(cfg), "create", cfg.Pool+"/"+spec.Name, "--size", strconv.FormatInt(sizeMiB, 10), "--image-feature", "layering,exclusive-lock")
		if out, err := commandOutput("rbd", args...); err != nil {
			_ = os.RemoveAll(m.diskDir(spec.Name))
			return Spec{}, fmt.Errorf("create Ceph RBD: %w: %s", err, out)
		}

	case ProviderCephFS:
		cfg, err := m.CephConfig()
		if err != nil {
			_ = os.RemoveAll(m.diskDir(spec.Name))
			return Spec{}, err
		}
		if err := requireCommands("ceph", "mount", "umount"); err != nil {
			_ = os.RemoveAll(m.diskDir(spec.Name))
			return Spec{}, err
		}
		args := append(m.cephBaseArgs(cfg), "fs", "subvolume", "create", cfg.FSName, spec.Name, "--size", strconv.FormatInt(spec.SizeBytes, 10))
		if out, err := commandOutput("ceph", args...); err != nil {
			_ = os.RemoveAll(m.diskDir(spec.Name))
			return Spec{}, fmt.Errorf("create CephFS subvolume: %w: %s", err, out)
		}
		pathArgs := append(m.cephBaseArgs(cfg), "fs", "subvolume", "getpath", cfg.FSName, spec.Name)
		out, err := commandOutput("ceph", pathArgs...)
		if err != nil {
			return Spec{}, fmt.Errorf("get CephFS subvolume path: %w: %s", err, out)
		}
		spec.RemotePath = strings.TrimSpace(out)
		spec.Initialized = true
	}

	if err := writeJSON(m.specPath(spec.Name), spec, 0600); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

// Resolve is for offline diagnostics of local data. Runtime callers must Acquire.
func (m *Manager) Resolve(name string) (string, error) {
	spec, err := m.Inspect(name)
	if err != nil {
		return "", err
	}
	if spec.Provider != ProviderLocal {
		return "", fmt.Errorf("remote Disks require an owned attachment")
	}
	return m.dataPath(name), nil
}

func (m *Manager) Inspect(name string) (Spec, error) {
	if !diskName.MatchString(name) {
		return Spec{}, fmt.Errorf("invalid Disk name %q", name)
	}
	var spec Spec
	if err := readJSON(m.specPath(name), &spec); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

func (m *Manager) List() ([]Spec, error) {
	entries, err := os.ReadDir(m.disksDir())
	if errors.Is(err, os.ErrNotExist) {
		return []Spec{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]Spec, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		spec, err := m.Inspect(entry.Name())
		if err == nil {
			out = append(out, spec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Manager) Delete(name string, destroyData bool) error {
	unlock, err := m.lock(name)
	if err != nil {
		return err
	}
	defer unlock()
	spec, err := m.Inspect(name)
	if err != nil {
		return err
	}
	if spec.Provider != ProviderLocal && !destroyData {
		return fmt.Errorf("refusing to delete remote Disk %s without explicit destroy-data", name)
	}

	guard, err := m.guardSpec(spec)
	if err != nil {
		return err
	}
	defer guard()
	a, err := m.acquire(name, "maintenance", "delete")
	if err != nil {
		return err
	}
	defer a.Close()
	if spec.Provider == ProviderCephRBD {
		if err = m.detach(name); err != nil {
			return err
		}
	}
	switch spec.Provider {
	case ProviderCephRBD:
		cfg, err := m.CephConfig()
		if err != nil {
			return err
		}
		args := append(m.rbdBaseArgs(cfg), "rm", cfg.Pool+"/"+name)
		if out, err := commandOutput("rbd", args...); err != nil {
			return fmt.Errorf("remove Ceph RBD: %w: %s", err, out)
		}
	case ProviderCephFS:
		cfg, err := m.CephConfig()
		if err != nil {
			return err
		}
		args := append(m.cephBaseArgs(cfg), "fs", "subvolume", "rm", cfg.FSName, name)
		if out, err := commandOutput("ceph", args...); err != nil {
			return fmt.Errorf("remove CephFS subvolume: %w: %s", err, out)
		}
	}
	if spec.Provider == ProviderCephFS {
		a.Close()
		if err = m.unmountOnly(name); err != nil {
			return err
		}
	}
	return os.RemoveAll(m.diskDir(name))
}

func (m *Manager) Detach(name string) error {
	unlock, err := m.lock(name)
	if err != nil {
		return err
	}
	defer unlock()
	spec, err := m.Inspect(name)
	if err != nil {
		return err
	}
	guard, err := m.guardSpec(spec)
	if err != nil {
		return err
	}
	defer guard()
	a, err := m.acquire(name, "maintenance", "detach")
	if err != nil {
		// Fenced clients cannot reopen their distributed lock. Only positive
		// native blocklist evidence permits their ordinary (non-lazy) detach.
		if spec.ManagedID == "" {
			return err
		}
		w, e := m.Writer(name)
		if e != nil {
			return err
		}
		cfg, e := m.CephConfig()
		if e != nil {
			return e
		}
		if e = m.waitCephFSBlocklists(cfg, w.Address); e != nil {
			return err
		}
		return m.detach(name)
	}
	// Runtime has stopped before detach. Release the maintenance FD so a
	// CephFS unmount is not held busy by its own open lock inode.
	if err = a.Close(); err != nil {
		return err
	}
	return m.detach(name)
}
func (m *Manager) detach(name string) error {
	spec, err := m.Inspect(name)
	if err != nil {
		return err
	}
	if spec.Provider == ProviderLocal {
		return nil
	}
	target := m.mountPath(name)
	if mounted(target) {
		if out, err := commandOutput("umount", target); err != nil {
			return fmt.Errorf("unmount Disk %s: %w: %s", name, err, out)
		}
	}
	if spec.Provider == ProviderCephRBD {
		devicePath := m.devicePath(name)
		data, err := os.ReadFile(devicePath)
		if err == nil {
			device := strings.TrimSpace(string(data))
			if device != "" {
				cfg, cfgErr := m.CephConfig()
				if cfgErr != nil {
					return cfgErr
				}
				args := append(m.rbdBaseArgs(cfg), "unmap", device)
				if out, unmapErr := commandOutput("rbd", args...); unmapErr != nil {
					return fmt.Errorf("unmap Ceph RBD: %w: %s", unmapErr, out)
				}
			}
			_ = os.Remove(devicePath)
		}
	}
	return nil
}

func (m *Manager) attachRBD(spec Spec) (string, error) {
	if os.Geteuid() != 0 {
		return "", fmt.Errorf("Ceph RBD attachment requires root")
	}
	target := m.mountPath(spec.Name)
	if mounted(target) {
		return target, nil
	}
	cfg, err := m.CephConfig()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(target, 0750); err != nil {
		return "", err
	}

	// Refuse legacy images without mandatory exclusive-lock semantics.
	info, err := commandOutput("rbd", append(m.rbdBaseArgs(cfg), "info", cfg.Pool+"/"+spec.Name, "--format", "json")...)
	if err != nil {
		return "", err
	}
	var image struct {
		Features []string `json:"features"`
	}
	if err = json.Unmarshal([]byte(info), &image); err != nil {
		return "", err
	}
	exclusive := false
	for _, feature := range image.Features {
		if feature == "exclusive-lock" {
			exclusive = true
		}
	}
	if !exclusive {
		return "", fmt.Errorf("RBD requires exclusive-lock image feature")
	}
	device := ""
	if saved, e := os.ReadFile(m.devicePath(spec.Name)); e == nil {
		list, e := commandOutput("rbd", append(m.rbdBaseArgs(cfg), "device", "list", "--format", "json")...)
		if e != nil {
			return "", e
		}
		var mappings []struct {
			Pool   string `json:"pool"`
			Name   string `json:"name"`
			Device string `json:"device"`
		}
		if e = json.Unmarshal([]byte(list), &mappings); e != nil {
			return "", e
		}
		for _, mapping := range mappings {
			if mapping.Device == strings.TrimSpace(string(saved)) && mapping.Pool == cfg.Pool && mapping.Name == spec.Name {
				device = mapping.Device
			}
		}
	}
	if device == "" {
		args := append(m.rbdBaseArgs(cfg), "device", "map", cfg.Pool+"/"+spec.Name, "--exclusive", "--options", "noshare,lock_on_read,lock_timeout=5,mount_timeout=15")
		out, e := commandOutput("rbd", args...)
		if e != nil {
			return "", fmt.Errorf("map Ceph RBD: %w", e)
		}
		device = strings.TrimSpace(out)
		if !strings.HasPrefix(device, "/dev/rbd") {
			return "", fmt.Errorf("invalid mapped RBD device")
		}
		if e = os.WriteFile(m.devicePath(spec.Name), []byte(device+"\n"), 0600); e != nil {
			return "", e
		}
	}

	// Query failure must never be interpreted as an unformatted image.
	if !spec.Initialized {
		format, probeErr := commandOutput("blkid", "-p", "-o", "value", "-s", "TYPE", device)
		if probeErr != nil {
			exit, ok := probeErr.(*exec.ExitError)
			if !ok || exit.ExitCode() != 2 {
				return "", fmt.Errorf("filesystem probe failed: %w", probeErr)
			}
		}
		if strings.TrimSpace(format) == "" {
			if out, err := commandOutput("mkfs.ext4", "-F", "-L", "TITANUS-"+spec.Name, device); err != nil {
				return "", fmt.Errorf("format Ceph RBD: %w: %s", err, out)
			}
		}
		spec.Initialized = true
		if err := writeJSON(m.specPath(spec.Name), spec, 0600); err != nil {
			return "", err
		}
	}

	if out, err := commandOutput("mount", device, target); err != nil {
		return "", fmt.Errorf("mount Ceph RBD: %w: %s", err, out)
	}
	return target, nil
}

func (m *Manager) attachCephFS(spec Spec) (string, error) {
	if os.Geteuid() != 0 {
		return "", fmt.Errorf("CephFS attachment requires root")
	}
	target := m.mountPath(spec.Name)

	cfg, err := m.CephConfig()
	if err != nil {
		return "", err
	}
	if err = m.verifyCephFSFencing(cfg); err != nil {
		return "", err
	}
	if mounted(target) {
		return target, nil
	}
	if err := os.MkdirAll(target, 0750); err != nil {
		return "", err
	}
	client := strings.TrimPrefix(cfg.Client, "client.")
	options := []string{"name=" + client, "fs=" + cfg.FSName, "conf=" + cfg.Conf, "noshare", "recover_session=no"}
	if cfg.Keyring != "" {
		options = append(options, "secretfile="+m.cephSecretPath())
		if out, err := commandOutput("ceph", append(m.cephBaseArgs(cfg), "auth", "get-key", cfg.Client)...); err != nil {
			return "", fmt.Errorf("get CephFS mount secret: %w", err)
		} else if err := os.WriteFile(m.cephSecretPath(), []byte(out), 0600); err != nil {
			return "", err
		}
	}
	source := ":/" + strings.TrimPrefix(spec.RemotePath, "/")
	if out, err := commandOutput("mount", "-t", "ceph", source, target, "-o", strings.Join(options, ",")); err != nil {
		return "", fmt.Errorf("mount CephFS: %w: %s", err, out)
	}
	return target, nil
}

func (m *Manager) rbdBaseArgs(cfg CephConfig) []string {
	args := []string{"--cluster", cfg.Cluster, "--id", strings.TrimPrefix(cfg.Client, "client."), "--conf", cfg.Conf}
	if cfg.Keyring != "" {
		args = append(args, "--keyring", cfg.Keyring)
	}
	return args
}

func (m *Manager) cephBaseArgs(cfg CephConfig) []string {
	args := []string{"--cluster", cfg.Cluster, "--id", strings.TrimPrefix(cfg.Client, "client."), "--conf", cfg.Conf}
	if cfg.Keyring != "" {
		args = append(args, "--keyring", cfg.Keyring)
	}
	return args
}

func ValidateMounts(mounts []Mount) error {
	seen := map[string]bool{}
	for _, mount := range mounts {
		if !diskName.MatchString(mount.Disk) {
			return fmt.Errorf("invalid Disk name %q", mount.Disk)
		}
		if !filepath.IsAbs(mount.Target) || filepath.Clean(mount.Target) == "/" {
			return fmt.Errorf("Disk %s requires an absolute non-root target", mount.Disk)
		}
		target := filepath.Clean(mount.Target)
		for _, protected := range []string{"/proc", "/sys", "/dev", "/.titanus-oldroot", "/.titanus-newroot"} {
			if target == protected || strings.HasPrefix(target, protected+"/") {
				return fmt.Errorf("protected Disk target %s", target)
			}
		}
		if target != mount.Target {
			return fmt.Errorf("Disk target must be canonical")
		}
		if seen[target] {
			return fmt.Errorf("duplicate Disk target %s", target)
		}
		seen[target] = true
	}
	return nil
}

func ParseMount(value string) (Mount, error) {
	parts := strings.Split(value, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return Mount{}, fmt.Errorf("mount must be DISK:/absolute/path[:ro]")
	}
	mount := Mount{Disk: strings.TrimSpace(parts[0]), Target: filepath.Clean(strings.TrimSpace(parts[1]))}
	if len(parts) == 3 {
		if strings.ToLower(strings.TrimSpace(parts[2])) != "ro" {
			return Mount{}, fmt.Errorf("third mount field must be ro")
		}
		mount.ReadOnly = true
	}
	return mount, ValidateMounts([]Mount{mount})
}

func ParseBytes(value string) (int64, error) {
	text := strings.TrimSpace(strings.ToUpper(value))
	multipliers := []struct {
		suffix string
		value  int64
	}{
		{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20},
		{"TB", 1000 * 1000 * 1000 * 1000}, {"GB", 1000 * 1000 * 1000}, {"MB", 1000 * 1000},
		{"T", 1 << 40}, {"G", 1 << 30}, {"M", 1 << 20},
	}
	for _, item := range multipliers {
		if strings.HasSuffix(text, item.suffix) {
			n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(text, item.suffix)), 10, 64)
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("invalid size %q", value)
			}
			return n * item.value, nil
		}
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	return n, nil
}

func requireCommands(names ...string) error {
	for _, name := range names {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("%s is required: %w", name, err)
		}
	}
	return nil
}

func commandOutput(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// JSON and device paths belong to stdout. Ceph may emit transport diagnostics
	// on stderr even for successful commands; never parse those as adapter data.
	output, err := cmd.Output()
	if exit, ok := err.(*exec.ExitError); ok {
		output = append(output, exit.Stderr...)
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return strings.TrimSpace(string(output)), err
}

func mounted(target string) bool {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	clean := filepath.Clean(target)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 4 && fields[4] == clean {
			return true
		}
	}
	return false
}

func writeJSON(path string, value any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	return durable.WriteJSON(path, value, mode)
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func (m *Manager) storageDir() string            { return filepath.Join(m.StateRoot, "storage") }
func (m *Manager) disksDir() string              { return filepath.Join(m.StateRoot, "disks") }
func (m *Manager) diskDir(name string) string    { return filepath.Join(m.disksDir(), name) }
func (m *Manager) specPath(name string) string   { return filepath.Join(m.diskDir(name), "disk.json") }
func (m *Manager) dataPath(name string) string   { return filepath.Join(m.diskDir(name), "data") }
func (m *Manager) mountPath(name string) string  { return filepath.Join(m.diskDir(name), "mount") }
func (m *Manager) devicePath(name string) string { return filepath.Join(m.diskDir(name), "device") }
func (m *Manager) cephSecretPath() string        { return filepath.Join(m.storageDir(), "mount.secret") }
func (m *Manager) cephConfigPath() string        { return filepath.Join(m.storageDir(), "ceph.json") }

func Bind(source, target string, readOnly bool) error {
	if err := os.MkdirAll(target, 0750); err != nil {
		return err
	}
	if err := syscall.Mount(source, target, "", uintptr(syscall.MS_BIND|syscall.MS_REC), ""); err != nil {
		return err
	}
	if readOnly {
		if err := syscall.Mount("", target, "", uintptr(syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY), ""); err != nil {
			_ = syscall.Unmount(target, syscall.MNT_DETACH)
			return err
		}
	}
	return nil
}
