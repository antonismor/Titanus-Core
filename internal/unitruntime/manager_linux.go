package unitruntime

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/fabric"
	"github.com/antonismor/Titanus-Core/internal/observe"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"github.com/antonismor/Titanus-Core/internal/security"
)

type Config struct {
	SecretKeyring string
	SecretRealm   string
	Observations  *observe.Recorder
	StateRoot     string
	CgroupRoot    string
	InitBinary    string
}

type Manager struct {
	cfg Config
}

func DefaultConfig() Config {
	return Config{
		StateRoot:  "/var/lib/titanus",
		CgroupRoot: "/sys/fs/cgroup/titanus",
		InitBinary: "/usr/local/libexec/titanus-init",
	}
}

func NewManager(cfg Config) *Manager {
	if cfg.StateRoot == "" {
		cfg.StateRoot = "/var/lib/titanus"
	}
	if cfg.CgroupRoot == "" {
		cfg.CgroupRoot = "/sys/fs/cgroup/titanus"
	}
	if cfg.InitBinary == "" {
		cfg.InitBinary = "/usr/local/libexec/titanus-init"
	}
	return &Manager{cfg: cfg}
}

func (m *Manager) Create(spec Spec) (State, error) {
	unlock, err := m.lock()
	if err != nil {
		return State{}, err
	}
	defer unlock()
	return m.create(spec)
}

func (m *Manager) create(spec Spec) (State, error) {
	spec.Normalize()
	if err := spec.Validate(); err != nil {
		return State{}, err
	}

	sourceRoot := m.sourceRoot(spec.Source)
	info, err := os.Stat(sourceRoot)
	if err != nil {
		return State{}, fmt.Errorf("Source %s is unavailable: %w", spec.Source, err)
	}
	if !info.IsDir() {
		return State{}, fmt.Errorf("Source %s rootfs is not a directory", spec.Source)
	}

	unitDir := m.unitDir(spec.ID)
	if _, err := os.Stat(unitDir); err == nil {
		return State{}, fmt.Errorf("Unit %s already exists", spec.ID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return State{}, err
	}

	for _, dir := range []string{
		unitDir,
		filepath.Join(unitDir, "upper"),
		filepath.Join(unitDir, "work"),
		filepath.Join(unitDir, "rootfs"),
		filepath.Join(unitDir, "logs"),
	} {
		if err := os.MkdirAll(dir, 0750); err != nil {
			_ = os.RemoveAll(unitDir)
			return State{}, err
		}
	}

	state := State{
		ID:        spec.ID,
		Status:    StatusCreated,
		CreatedAt: time.Now().UTC(),
	}
	if err := saveJSON(filepath.Join(unitDir, "spec.json"), spec, 0600); err != nil {
		_ = os.RemoveAll(unitDir)
		return State{}, err
	}
	if err := saveJSON(filepath.Join(unitDir, "state.json"), state, 0600); err != nil {
		_ = os.RemoveAll(unitDir)
		return State{}, err
	}
	return state, nil
}

func (m *Manager) Ensure(spec Spec) (State, error) {
	unlock, err := m.lock()
	if err != nil {
		return State{}, err
	}
	defer unlock()
	spec.Normalize()
	if err := spec.Validate(); err != nil {
		return State{}, err
	}
	existingSpec, state, err := m.load(spec.ID)
	if err == nil {
		legacyHealth := existingSpec.Health.Restart == ""
		if legacyHealth && spec.Health.Readiness == nil && spec.Health.Liveness == nil {
			existingSpec.Health = spec.Health
		}
		existingSpec.Normalize()
		if !reflect.DeepEqual(existingSpec, spec) {
			return State{}, fmt.Errorf("Unit %s already exists with a different specification", spec.ID)
		}
		if legacyHealth {
			if err := saveJSON(filepath.Join(m.unitDir(spec.ID), "spec.json"), existingSpec, 0600); err != nil {
				return State{}, err
			}
		}
		return state, nil
	}
	if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "no such file") {
		return State{}, err
	}
	return m.create(spec)
}

func (m *Manager) Start(id string) (State, error) { return m.start(id, false) }

func (m *Manager) start(id string, automatic bool) (State, error) {
	unlock, lockErr := m.lock()
	if lockErr != nil {
		return State{}, lockErr
	}
	defer unlock()
	if os.Geteuid() != 0 {
		return State{}, fmt.Errorf("starting a Unit currently requires root")
	}

	spec, state, err := m.load(id)
	if err != nil {
		return State{}, err
	}
	if automatic && !state.DesiredRunning {
		return state, nil
	}
	if state.PID > 0 && state.Process.StartTicks == 0 && m.belongsToUnit(id, state.PID) {
		state.Process, _ = readProcessIdentity(state.PID)
	}
	if processMatches(state) {
		state.DesiredRunning = true
		if spec.Health.Readiness == nil {
			state.Ready = true
		}
		if err := m.saveState(state); err != nil {
			return State{}, err
		}
		state.Status = StatusActive
		return state, nil
	}
	if state.Status == StatusActive || state.Status == StatusStarting {
		if spec.Network.Fabric {
			_ = fabric.NewManager(m.cfg.StateRoot).Detach(id)
		}
		m.cleanupAfterStop(id)
	}
	// Older persisted Units receive the same hardened defaults on their next
	// start. Already running workloads must be stopped before applying changes.
	spec.Normalize()
	if err := spec.Validate(); err != nil {
		return m.fail(state, err)
	}
	mapping, err := m.allocateMapping(id)
	if err != nil {
		return m.fail(state, err)
	}
	state.UserMapping = mapping
	effectivePolicy := spec.Security
	effectivePolicy.LSMWritePaths = append([]string{}, spec.Security.LSMWritePaths...)
	for _, mount := range spec.Mounts {
		duplicate := false
		for _, p := range effectivePolicy.LSMWritePaths {
			if p == mount.Target {
				duplicate = true
			}
		}
		if !mount.ReadOnly && !duplicate {
			effectivePolicy.LSMWritePaths = append(effectivePolicy.LSMWritePaths, mount.Target)
		}
	}
	policyJSON, err := json.Marshal(effectivePolicy)
	if err != nil {
		return m.fail(state, err)
	}

	state.DesiredRunning = true
	state.HealthFailed = false
	if automatic {
		state.RestartCount++
	} else {
		state.RestartCount = 0
	}
	state.NextStartAt = time.Time{}
	state.Ready = spec.Health.Readiness == nil
	state.Live = true
	state.Readiness = ProbeResult{}
	state.Liveness = ProbeResult{}
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		return State{}, err
	}
	state.RunID = hex.EncodeToString(token[:])
	state.ExitCode = nil
	state.ExitSignal = 0
	state.Process = ProcessIdentity{}
	state.PID = 0
	state.NetworkAddress = ""
	state.Status = StatusStarting
	state.LastError = ""
	if err := m.saveState(state); err != nil {
		return State{}, err
	}

	var keys *secrets.Keyring
	if len(spec.Secrets) > 0 {
		if spec.SecretRealm != m.cfg.SecretRealm {
			return m.fail(state, fmt.Errorf("secret Realm mismatch"))
		}
		keys, err = secrets.Load(m.cfg.SecretKeyring)
		if err != nil {
			return m.fail(state, err)
		}
	}
	environment, err := keys.Environment(spec.SecretRealm, spec.Secrets, spec.SecretBindings, spec.Environment)
	if err != nil {
		return m.fail(state, err)
	}
	if _, err := security.LandlockABI(); err != nil {
		return m.fail(state, err)
	}
	if err := m.mountOverlay(spec); err != nil {
		return m.fail(state, fmt.Errorf("OverlayFS: %w", err))
	}
	diskFiles, diskErr := m.prepareDiskMounts(spec, state.RunID)
	if diskErr != nil {
		_ = m.unmountRootfs(spec.ID)
		return m.fail(state, fmt.Errorf("Disk mounts: %w", diskErr))
	}
	defer func() {
		for _, f := range diskFiles {
			_ = f.Close()
		}
	}()
	if err := m.prepareCgroup(spec); err != nil {
		m.cleanupAfterStop(spec.ID)
		return m.fail(state, fmt.Errorf("cgroup: %w", err))
	}

	logFile, err := m.openLogSink(id)
	if err != nil {
		m.cleanupAfterStop(id)
		return m.fail(state, err)
	}

	rootfs := filepath.Join(m.unitDir(id), "rootfs")
	access, releaseAccess, err := prepareRootAccess(rootfs, mapping, state.RunID)
	if err != nil {
		_ = logFile.Close()
		m.cleanupAfterStop(id)
		return m.fail(state, err)
	}
	defer releaseAccess()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		_ = logFile.Close()
		m.cleanupAfterStop(id)
		return m.fail(state, fmt.Errorf("create runtime readiness pipe: %w", err))
	}

	statusRead, statusWrite, err := os.Pipe()
	if err != nil {
		_ = readyRead.Close()
		_ = readyWrite.Close()
		_ = logFile.Close()
		m.cleanupAfterStop(id)
		return m.fail(state, fmt.Errorf("create startup status pipe: %w", err))
	}
	defer statusRead.Close()
	args := []string{"--unit-child", access, spec.Hostname, "3", string(policyJSON), "4", strconv.Itoa(len(diskFiles)), "--"}
	args = append(args, spec.Command...)
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		_ = readyRead.Close()
		_ = readyWrite.Close()
		_ = statusWrite.Close()
		_ = logFile.Close()
		m.cleanupAfterStop(id)
		return m.fail(state, err)
	}
	defer controlRead.Close()
	monitorJSON, err := json.Marshal(MonitorConfig{DiskLocks: len(diskFiles), Mapping: mapping, Args: args, ExitPath: filepath.Join(m.unitDir(id), "exit.json"), RunID: state.RunID})
	if err != nil {
		_ = controlWrite.Close()
		return m.fail(state, err)
	}
	cmd := exec.Command(m.cfg.InitBinary, "--unit-monitor", string(monitorJSON))
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = environment
	cmd.ExtraFiles = append([]*os.File{readyRead, statusWrite, controlWrite}, diskFiles...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		_ = controlWrite.Close()
		_ = statusWrite.Close()
		_ = readyRead.Close()
		_ = readyWrite.Close()
		_ = logFile.Close()
		m.cleanupAfterStop(id)
		return m.fail(state, fmt.Errorf("start titanus-init: %w", err))
	}
	_ = readyRead.Close()
	_ = statusWrite.Close()
	_ = controlWrite.Close()
	pid, controlErr := awaitMonitorPID(controlRead, 10*time.Second)
	if controlErr != nil {
		_ = readyWrite.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
		m.cleanupAfterStop(id)
		return m.fail(state, controlErr)
	}
	identity, identityErr := readProcessIdentity(pid)
	if identityErr != nil {
		_ = readyWrite.Close()
		_ = cmd.Wait()
		_ = logFile.Close()
		m.cleanupAfterStop(id)
		return m.fail(state, identityErr)
	}
	state.PID = pid
	state.Process = identity

	failStarted := func(cause error) (State, error) {
		_ = readyWrite.Close()
		_ = signalProcess(state, syscall.SIGKILL)
		_ = cmd.Wait()
		_ = logFile.Close()
		_ = fabric.NewManager(m.cfg.StateRoot).Detach(id)
		m.cleanupAfterStop(id)
		return m.fail(state, cause)
	}

	if err := os.WriteFile(filepath.Join(m.cgroupDir(id), "cgroup.procs"), []byte(strconv.Itoa(pid)), 0644); err != nil {
		return failStarted(fmt.Errorf("assign process to cgroup: %w", err))
	}

	if spec.Network.Fabric {
		fabricManager := fabric.NewManager(m.cfg.StateRoot)
		allocation, err := fabricManager.Attach(id, pid, spec.Network.Ports)
		if err != nil {
			return failStarted(fmt.Errorf("Fabric attach: %w", err))
		}
		state.NetworkAddress = allocation.Address
		fabricConfig, err := fabricManager.Config()
		if err != nil {
			return failStarted(fmt.Errorf("Fabric resolver config: %w", err))
		}
		if err := prepareFabricResolver(rootfs, fabricConfig.Gateway); err != nil {
			return failStarted(fmt.Errorf("Fabric resolver: %w", err))
		}
	}

	if spec.Network.Fabric {
		for _, path := range []string{"etc", "etc/resolv.conf"} {
			if err := os.Chown(filepath.Join(rootfs, path), mapping.Base, mapping.Base); err != nil {
				return failStarted(err)
			}
		}
	}

	if _, err := readyWrite.Write([]byte{1}); err != nil {
		return failStarted(fmt.Errorf("release Unit startup barrier: %w", err))
	}
	_ = readyWrite.Close()
	if err := awaitStartup(statusRead, 10*time.Second); err != nil {
		return failStarted(fmt.Errorf("Unit initialization: %w", err))
	}

	state.PID = pid
	state.Status = StatusActive
	state.StartedAt = time.Now().UTC()
	if err := m.saveState(state); err != nil {
		return failStarted(err)
	}

	_ = logFile.Close()
	go func() { _ = cmd.Wait() }()
	return state, nil
}

// The restricted supervisor closes this pipe only after Start confirms the
// workload exec. Errors, premature EOF and timeouts fail closed.
func awaitStartup(file *os.File, timeout time.Duration) error {
	if err := file.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(file, 4096))
	if err != nil {
		return fmt.Errorf("wait for workload exec: %w", err)
	}
	if string(data) != "READY\n" {
		return fmt.Errorf("workload did not exec: %s", strings.TrimSpace(string(data)))
	}
	return nil
}

func (m *Manager) Stop(id string, timeout time.Duration) (State, error) {
	return m.stop(id, timeout, false, "")
}

func (m *Manager) stop(id string, timeout time.Duration, forHealth bool, expectedRun string) (State, error) {
	unlock, lockErr := m.lock()
	if lockErr != nil {
		return State{}, lockErr
	}
	defer unlock()
	_, state, err := m.load(id)
	if err != nil {
		return State{}, err
	}
	if expectedRun != "" && state.RunID != expectedRun {
		return state, nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	if processMatches(state) {
		if err := signalProcess(state, syscall.SIGTERM); err != nil && processMatches(state) {
			return State{}, err
		}
		deadline := time.Now().Add(timeout)
		for processMatches(state) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if processMatches(state) {
			_ = os.WriteFile(filepath.Join(m.cgroupDir(id), "cgroup.kill"), []byte("1"), 0644)
			if err := signalProcess(state, syscall.SIGKILL); err != nil && processMatches(state) {
				return State{}, err
			}
			deadline = time.Now().Add(5 * time.Second)
			for processMatches(state) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if processMatches(state) {
				return State{}, fmt.Errorf("Unit did not terminate; resources retained")
			}
		}
	}

	if spec, _, loadErr := m.load(id); loadErr == nil && spec.Network.Fabric {
		_ = fabric.NewManager(m.cfg.StateRoot).Detach(id)
	}
	if state.RunID != "" {
		deadline := time.Now().Add(2 * time.Second)
		for !m.applyExitRecord(&state) && state.ExitCode == nil && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
	}
	m.cleanupAfterStop(id)
	state.PID = 0
	state.Status = StatusStopped
	state.StoppedAt = time.Now().UTC()
	state.HealthFailed = forHealth
	state.DesiredRunning = forHealth
	state.Ready = false
	state.Live = !forHealth
	if forHealth {
		state.Status = StatusFailed
		state.LastError = "liveness probe failed"
	} else {
		state.LastError = ""
	}
	state.NextStartAt = time.Time{}
	if err := m.saveState(state); err != nil {
		return State{}, err
	}
	return state, nil
}

func (m *Manager) StopLeaseUnit(id string) error {
	_, err := m.Stop(id, 5*time.Second)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file") {
			return nil
		}
		return err
	}
	return nil
}

func (m *Manager) Inspect(id string) (Spec, State, error) {
	unlock, lockErr := m.lock()
	if lockErr != nil {
		return Spec{}, State{}, lockErr
	}
	defer unlock()
	spec, state, err := m.load(id)
	if err != nil {
		return Spec{}, State{}, err
	}
	if state.PID > 0 {
		// Legacy Units can only be adopted when their PID belongs to this Unit's
		// private cgroup. A numeric PID alone never authorizes a signal.
		if state.Process.StartTicks == 0 && m.belongsToUnit(id, state.PID) {
			if identity, err := readProcessIdentity(state.PID); err == nil {
				state.Process = identity
				if err := m.saveState(state); err != nil {
					return Spec{}, State{}, err
				}
			}
		}
		if processMatches(state) {
			state.Status = StatusActive
		} else if state.Status == StatusActive || state.Status == StatusStarting {
			state.Status = StatusFailed
			state.LastError = "Unit process is no longer running or its identity changed"
			m.applyExitRecord(&state)
			state.PID = 0
			state.Ready = false
			state.Live = false
			if spec.Network.Fabric {
				_ = fabric.NewManager(m.cfg.StateRoot).Detach(id)
			}
			m.cleanupAfterStop(id)
			if err := m.saveState(state); err != nil {
				return Spec{}, State{}, err
			}
		}
	}
	if state.PID == 0 && m.applyExitRecord(&state) {
		if err := m.saveState(state); err != nil {
			return Spec{}, State{}, err
		}
	}
	if state.Status == StatusActive && spec.Health.Readiness == nil {
		state.Ready = true
	}
	if state.Status == StatusActive && state.RestartCount > 0 && time.Since(state.StartedAt) > 10*time.Minute {
		state.RestartCount = 0
		if err := m.saveState(state); err != nil {
			return Spec{}, State{}, err
		}
	}
	if state.Status == StatusActive && !state.DesiredRunning {
		state.DesiredRunning = true
		if err := m.saveState(state); err != nil {
			return Spec{}, State{}, err
		}
	}
	return spec, state, nil
}

func (m *Manager) Delete(id string) error {
	unlock, lockErr := m.lock()
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	_, state, err := m.load(id)
	if err != nil {
		return err
	}
	if processMatches(state) {
		return fmt.Errorf("Unit %s is ACTIVE; stop it before deletion", id)
	}
	spec, _, specErr := m.load(id)
	if specErr == nil && spec.Network.Fabric {
		_ = fabric.NewManager(m.cfg.StateRoot).Release(id)
	}
	m.cleanupAfterStop(id)
	return os.RemoveAll(m.unitDir(id))
}

func (m *Manager) List() ([]State, error) {
	root := filepath.Join(m.cfg.StateRoot, "units")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return []State{}, nil
	}
	if err != nil {
		return nil, err
	}
	states := make([]State, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		_, state, err := m.Inspect(entry.Name())
		if err != nil {
			continue
		}
		states = append(states, state)
	}
	return states, nil
}

func (m *Manager) LogsPath(id string) (string, error) {
	if _, _, err := m.load(id); err != nil {
		return "", err
	}
	return filepath.Join(m.unitDir(id), "logs", "unit.log"), nil
}

func prepareFabricResolver(rootfs, gatewayCIDR string) error {
	gatewayIP, _, err := net.ParseCIDR(strings.TrimSpace(gatewayCIDR))
	if err != nil || gatewayIP.To4() == nil {
		return fmt.Errorf("invalid IPv4 Fabric gateway %q", gatewayCIDR)
	}
	etcDir, err := unitMountTarget(rootfs, "/etc")
	if err != nil {
		return err
	}
	path := filepath.Join(etcDir, "resolv.conf")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace Unit resolv.conf: %w", err)
	}
	content := fmt.Sprintf(
		"# Managed by Titanus Fabric\nsearch titanus\nnameserver %s\noptions ndots:1 timeout:2 attempts:2\n",
		gatewayIP.To4().String(),
	)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("write Unit resolv.conf: %w", err)
	}
	return nil
}

func (m *Manager) prepareDiskMounts(spec Spec, run string) (files []*os.File, result error) {
	manager := disk.NewManager(m.cfg.StateRoot)
	rootfs := filepath.Join(m.unitDir(spec.ID), "rootfs")
	targets := []string{}
	defer func() {
		if result != nil {
			for i := len(targets) - 1; i >= 0; i-- {
				_ = syscall.Unmount(targets[i], syscall.MNT_DETACH)
			}
			for _, f := range files {
				_ = f.Close()
			}
			files = nil
		}
	}()
	for _, mount := range spec.Mounts {
		a, err := manager.Acquire(mount.Disk, spec.ID, run)
		if err != nil {
			return files, err
		}
		files = append(files, a.File)
		target, err := unitMountTarget(rootfs, mount.Target)
		if err != nil {
			return files, err
		}
		if err = disk.Bind(a.Path, target, mount.ReadOnly); err != nil {
			return files, err
		}
		targets = append(targets, target)
	}
	return files, nil
}

func (m *Manager) cleanupDiskMounts(spec Spec) {
	rootfs := filepath.Join(m.unitDir(spec.ID), "rootfs")
	for i := len(spec.Mounts) - 1; i >= 0; i-- {
		target, err := unitMountTarget(rootfs, spec.Mounts[i].Target)
		if err == nil {
			_ = syscall.Unmount(target, syscall.MNT_DETACH)
		}
	}
}

func unitMountTarget(rootfs, target string) (string, error) {
	cleanTarget := filepath.Clean(target)
	if !filepath.IsAbs(cleanTarget) || cleanTarget == "/" {
		return "", fmt.Errorf("invalid Unit mount target %q", target)
	}
	relative := strings.TrimPrefix(cleanTarget, string(os.PathSeparator))
	full := filepath.Join(rootfs, relative)
	cleanRoot := filepath.Clean(rootfs)
	if full == cleanRoot || !strings.HasPrefix(full, cleanRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("mount target %q escapes Unit rootfs", target)
	}
	current := cleanRoot
	for _, part := range strings.Split(relative, string(os.PathSeparator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if e := os.Mkdir(current, 0755); e != nil {
				return "", e
			}
			continue
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("mount target traverses non-directory or symlink: %s", current)
		}
	}
	return full, nil
}

func (m *Manager) mountOverlay(spec Spec) error {
	unitDir := m.unitDir(spec.ID)
	merged := filepath.Join(unitDir, "rootfs")
	if mounted(merged) {
		return nil
	}
	mapping, err := m.allocateMapping(spec.ID)
	if err != nil {
		return err
	}
	lower, err := m.prepareMappedSource(spec, mapping)
	if err != nil {
		return err
	}
	data := strings.Join([]string{
		"lowerdir=" + lower,
		"upperdir=" + filepath.Join(unitDir, "upper"),
		"workdir=" + filepath.Join(unitDir, "work"),
	}, ",")
	if err := syscall.Mount("overlay", merged, "overlay", 0, data); err != nil {
		return err
	}
	// Reuse PR #6's traversal fix for a non-root workload identity. The outer
	// host-side Unit state remains private.
	if err := os.Chmod(merged, 0755); err != nil {
		_ = syscall.Unmount(merged, syscall.MNT_DETACH)
		return fmt.Errorf("set Unit rootfs permissions: %w", err)
	}
	return nil
}

func (m *Manager) unmountRootfs(id string) error {
	merged := filepath.Join(m.unitDir(id), "rootfs")
	if !mounted(merged) {
		return nil
	}
	return syscall.Unmount(merged, syscall.MNT_DETACH)
}

func (m *Manager) prepareCgroup(spec Spec) error {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return fmt.Errorf("cgroups v2 is required")
	}
	if err := os.MkdirAll(m.cfg.CgroupRoot, 0755); err != nil {
		return err
	}
	if err := enableControllers(m.cfg.CgroupRoot, []string{"cpu", "memory", "pids"}); err != nil {
		return err
	}

	dir := m.cgroupDir(spec.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}

	limits := map[string]string{
		"memory.max": strconv.FormatInt(spec.MemoryBytes, 10),
		"pids.max":   strconv.Itoa(spec.PidsMax),
		"cpu.max":    fmt.Sprintf("%d 100000", spec.CPUPercent*1000),
	}
	for name, value := range limits {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0644); err != nil {
			return fmt.Errorf("%s=%s: %w", name, value, err)
		}
	}
	return security.EnforceDevices(dir)
}

func enableControllers(cgroupRoot string, required []string) error {
	controllersPath := filepath.Join(cgroupRoot, "cgroup.controllers")
	availableData, err := os.ReadFile(controllersPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", controllersPath, err)
	}
	available := map[string]bool{}
	for _, controller := range strings.Fields(string(availableData)) {
		available[controller] = true
	}
	for _, controller := range required {
		if !available[controller] {
			return fmt.Errorf("cgroup controller %q is not delegated to %s", controller, cgroupRoot)
		}
	}

	commands := make([]string, 0, len(required))
	for _, controller := range required {
		commands = append(commands, "+"+controller)
	}
	path := filepath.Join(cgroupRoot, "cgroup.subtree_control")
	if err := os.WriteFile(path, []byte(strings.Join(commands, " ")), 0644); err != nil {
		return fmt.Errorf("enable cgroup controllers in %s: %w", cgroupRoot, err)
	}
	return nil
}

func (m *Manager) cleanupAfterStop(id string) {
	if spec, state, err := m.load(id); err == nil {
		m.cleanupDiskMounts(spec)
		cleanupRootAccess(state.RunID)
	}
	_ = os.WriteFile(filepath.Join(m.cgroupDir(id), "cgroup.kill"), []byte("1"), 0644)
	_ = m.unmountRootfs(id)
	_ = os.Remove(m.cgroupDir(id))
	if spec, _, err := m.load(id); err == nil {
		seen := map[string]bool{}
		for _, mount := range spec.Mounts {
			if !seen[mount.Disk] {
				seen[mount.Disk] = true
				_ = disk.NewManager(m.cfg.StateRoot).Detach(mount.Disk)
			}
		}
	}
}

func (m *Manager) fail(state State, cause error) (State, error) {
	state.Status = StatusFailed
	state.Ready = false
	state.Live = false
	state.PID = 0
	state.LastError = cause.Error()
	if err := m.saveState(state); err != nil {
		return state, errors.Join(cause, err)
	}
	return state, cause
}

func (m *Manager) load(id string) (Spec, State, error) {
	if !objectName.MatchString(id) {
		return Spec{}, State{}, fmt.Errorf("invalid Unit ID %q", id)
	}
	var spec Spec
	var state State
	if err := loadJSON(filepath.Join(m.unitDir(id), "spec.json"), &spec); err != nil {
		return Spec{}, State{}, fmt.Errorf("load Unit %s specification: %w", id, err)
	}
	if err := loadJSON(filepath.Join(m.unitDir(id), "state.json"), &state); err != nil {
		return Spec{}, State{}, fmt.Errorf("load Unit %s state: %w", id, err)
	}
	return spec, state, nil
}

func (m *Manager) saveState(state State) error {
	path := filepath.Join(m.unitDir(state.ID), "state.json")
	var previous State
	_ = loadJSON(path, &previous)
	if err := saveJSON(path, state, 0600); err != nil {
		return err
	}
	if m.cfg.Observations != nil && (previous.Status != state.Status || previous.Ready != state.Ready || previous.Live != state.Live || previous.RestartCount != state.RestartCount) {
		_ = m.cfg.Observations.Record(observe.Event{Kind: "unit.state", Object: state.ID, State: string(state.Status), Ready: state.Ready, Live: state.Live, Restarts: state.RestartCount})
	}
	return nil
}

func (m *Manager) unitDir(id string) string {
	return filepath.Join(m.cfg.StateRoot, "units", id)
}

func (m *Manager) sourceRoot(name string) string {
	return filepath.Join(m.cfg.StateRoot, "sources", name, "rootfs")
}

func (m *Manager) cgroupDir(id string) string {
	return filepath.Join(m.cfg.CgroupRoot, id)
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
