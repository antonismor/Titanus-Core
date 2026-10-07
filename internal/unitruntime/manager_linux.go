package unitruntime

import (
	"errors"
	"fmt"
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
)

type Config struct {
	StateRoot  string
	CgroupRoot string
	InitBinary string
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
	spec.Normalize()
	if err := spec.Validate(); err != nil {
		return State{}, err
	}
	existingSpec, state, err := m.load(spec.ID)
	if err == nil {
		existingSpec.Normalize()
		if !reflect.DeepEqual(existingSpec, spec) {
			return State{}, fmt.Errorf("Unit %s already exists with a different specification", spec.ID)
		}
		return state, nil
	}
	if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "no such file") {
		return State{}, err
	}
	return m.Create(spec)
}

func (m *Manager) Start(id string) (State, error) {
	if os.Geteuid() != 0 {
		return State{}, fmt.Errorf("starting a Unit currently requires root")
	}

	spec, state, err := m.load(id)
	if err != nil {
		return State{}, err
	}
	if state.PID > 0 && processAlive(state.PID) {
		state.Status = StatusActive
		return state, nil
	}

	state.Status = StatusStarting
	state.LastError = ""
	if err := m.saveState(state); err != nil {
		return State{}, err
	}

	if err := m.mountOverlay(spec); err != nil {
		return m.fail(state, fmt.Errorf("OverlayFS: %w", err))
	}
	if err := m.prepareDiskMounts(spec); err != nil {
		_ = m.unmountRootfs(spec.ID)
		return m.fail(state, fmt.Errorf("Disk mounts: %w", err))
	}
	if err := m.prepareCgroup(spec); err != nil {
		m.cleanupDiskMounts(spec)
		_ = m.unmountRootfs(spec.ID)
		return m.fail(state, fmt.Errorf("cgroup: %w", err))
	}

	logPath := filepath.Join(m.unitDir(id), "logs", "unit.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0640)
	if err != nil {
		m.cleanupAfterStop(id)
		return m.fail(state, err)
	}

	rootfs := filepath.Join(m.unitDir(id), "rootfs")
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		_ = logFile.Close()
		m.cleanupAfterStop(id)
		return m.fail(state, fmt.Errorf("create runtime readiness pipe: %w", err))
	}

	args := []string{"--unit-child", rootfs, spec.Hostname, "3", "--"}
	args = append(args, spec.Command...)
	cmd := exec.Command(m.cfg.InitBinary, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = append(os.Environ(), spec.Environment...)
	cmd.ExtraFiles = []*os.File{readyRead}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWPID |
			syscall.CLONE_NEWNS |
			syscall.CLONE_NEWIPC |
			syscall.CLONE_NEWNET,
		Setsid: true,
	}

	if err := cmd.Start(); err != nil {
		_ = readyRead.Close()
		_ = readyWrite.Close()
		_ = logFile.Close()
		m.cleanupAfterStop(id)
		return m.fail(state, fmt.Errorf("start titanus-init: %w", err))
	}
	_ = readyRead.Close()
	pid := cmd.Process.Pid

	failStarted := func(cause error) (State, error) {
		_ = readyWrite.Close()
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_ = logFile.Close()
		_ = fabric.NewManager(m.cfg.StateRoot).Detach(id)
		m.cleanupAfterStop(id)
		return m.fail(state, cause)
	}

	if err := os.WriteFile(filepath.Join(m.cgroupDir(id), "cgroup.procs"), []byte(strconv.Itoa(pid)), 0644); err != nil {
		return failStarted(fmt.Errorf("assign process to cgroup: %w", err))
	}

	if spec.Network.Fabric {
		allocation, err := fabric.NewManager(m.cfg.StateRoot).Attach(id, pid, spec.Network.Ports)
		if err != nil {
			return failStarted(fmt.Errorf("Fabric attach: %w", err))
		}
		state.NetworkAddress = allocation.Address
	}

	if _, err := readyWrite.Write([]byte{1}); err != nil {
		return failStarted(fmt.Errorf("release Unit startup barrier: %w", err))
	}
	_ = readyWrite.Close()

	state.PID = pid
	state.Status = StatusActive
	state.StartedAt = time.Now().UTC()
	if err := m.saveState(state); err != nil {
		return failStarted(err)
	}

	_ = logFile.Close()
	_ = cmd.Process.Release()
	return state, nil
}

func (m *Manager) Stop(id string, timeout time.Duration) (State, error) {
	_, state, err := m.load(id)
	if err != nil {
		return State{}, err
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	if state.PID > 0 && processAlive(state.PID) {
		_ = syscall.Kill(state.PID, syscall.SIGTERM)
		deadline := time.Now().Add(timeout)
		for processAlive(state.PID) && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if processAlive(state.PID) {
			_ = os.WriteFile(filepath.Join(m.cgroupDir(id), "cgroup.kill"), []byte("1"), 0644)
			_ = syscall.Kill(state.PID, syscall.SIGKILL)
		}
	}

	if spec, _, loadErr := m.load(id); loadErr == nil && spec.Network.Fabric {
		_ = fabric.NewManager(m.cfg.StateRoot).Detach(id)
	}
	m.cleanupAfterStop(id)
	state.PID = 0
	state.Status = StatusStopped
	state.StoppedAt = time.Now().UTC()
	state.LastError = ""
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
	spec, state, err := m.load(id)
	if err != nil {
		return Spec{}, State{}, err
	}
	if state.PID > 0 {
		if processAlive(state.PID) {
			state.Status = StatusActive
		} else if state.Status == StatusActive || state.Status == StatusStarting {
			state.Status = StatusFailed
			state.LastError = "Unit process is no longer running"
			state.PID = 0
			_ = m.saveState(state)
		}
	}
	return spec, state, nil
}

func (m *Manager) Delete(id string) error {
	_, state, err := m.load(id)
	if err != nil {
		return err
	}
	if state.PID > 0 && processAlive(state.PID) {
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

func (m *Manager) prepareDiskMounts(spec Spec) error {
	if len(spec.Mounts) == 0 {
		return nil
	}
	manager := disk.NewManager(m.cfg.StateRoot)
	rootfs := filepath.Join(m.unitDir(spec.ID), "rootfs")
	mountedTargets := make([]string, 0, len(spec.Mounts))

	for _, mount := range spec.Mounts {
		source, err := manager.Resolve(mount.Disk)
		if err != nil {
			for i := len(mountedTargets) - 1; i >= 0; i-- {
				_ = syscall.Unmount(mountedTargets[i], syscall.MNT_DETACH)
			}
			return fmt.Errorf("resolve Disk %s: %w", mount.Disk, err)
		}
		target, err := unitMountTarget(rootfs, mount.Target)
		if err != nil {
			for i := len(mountedTargets) - 1; i >= 0; i-- {
				_ = syscall.Unmount(mountedTargets[i], syscall.MNT_DETACH)
			}
			return err
		}
		if err := disk.Bind(source, target, mount.ReadOnly); err != nil {
			for i := len(mountedTargets) - 1; i >= 0; i-- {
				_ = syscall.Unmount(mountedTargets[i], syscall.MNT_DETACH)
			}
			return fmt.Errorf("bind Disk %s to %s: %w", mount.Disk, mount.Target, err)
		}
		mountedTargets = append(mountedTargets, target)
	}
	return nil
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
	return full, nil
}

func (m *Manager) mountOverlay(spec Spec) error {
	unitDir := m.unitDir(spec.ID)
	merged := filepath.Join(unitDir, "rootfs")
	if mounted(merged) {
		return nil
	}
	data := strings.Join([]string{
		"lowerdir=" + m.sourceRoot(spec.Source),
		"upperdir=" + filepath.Join(unitDir, "upper"),
		"workdir=" + filepath.Join(unitDir, "work"),
	}, ",")
	return syscall.Mount("overlay", merged, "overlay", 0, data)
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
	return nil
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
	if spec, _, err := m.load(id); err == nil {
		m.cleanupDiskMounts(spec)
	}
	_ = m.unmountRootfs(id)
	_ = os.WriteFile(filepath.Join(m.cgroupDir(id), "cgroup.kill"), []byte("1"), 0644)
	_ = os.Remove(m.cgroupDir(id))
}

func (m *Manager) fail(state State, cause error) (State, error) {
	state.Status = StatusFailed
	state.PID = 0
	state.LastError = cause.Error()
	_ = m.saveState(state)
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
	return saveJSON(filepath.Join(m.unitDir(state.ID), "state.json"), state, 0600)
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

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
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
