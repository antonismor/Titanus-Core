package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	buildversion "github.com/antonismor/Titanus-Core/internal/version"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/antonismor/Titanus-Core/internal/unitruntime"

	"github.com/antonismor/Titanus-Core/internal/security"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "--version-json") {
		buildversion.Print(os.Args[1] == "--version-json")
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "--unit-log-sink" {
		if err := unitruntime.RunLogSink(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, `{"kind":"unit.log_sink.failed"}`)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && (os.Args[1] == "--probe-net" || os.Args[1] == "--probe-worker") {
		var err error
		if os.Args[1] == "--probe-net" {
			err = unitruntime.EnterProbeNamespace(os.Args[2])
		} else {
			err = unitruntime.ProbeWorker(os.Args[2])
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "--unit-monitor" {
		if err := unitruntime.RunMonitor(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "titanus-monitor:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) >= 6 && os.Args[1] == "--supervise" {
		fd, err := strconv.Atoi(os.Args[2])
		count, countErr := strconv.Atoi(os.Args[3])
		if err != nil || fd < 3 || countErr != nil || count < 0 || count > 64 || os.Args[4] != "--" {
			os.Exit(64)
		}
		status := os.NewFile(uintptr(fd), "startup-status")
		syscall.CloseOnExec(fd)
		// Empty read-only storage lock FDs stay only in PID 1. Go's child exec
		// closes them; applications cannot unlock the inherited open description.
		for i := 0; i < count; i++ {
			syscall.CloseOnExec(6 + i)
		}
		code, err := supervise(os.Args[5:], status)
		if err != nil {
			fmt.Fprintln(status, "ERROR:", err)
			fmt.Fprintln(os.Stderr, err)
			code = 1
		}
		_ = status.Close()
		os.Exit(code)
	}
	if len(os.Args) >= 2 && os.Args[1] == "--unit-child" {
		if err := runUnitChild(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "titanus-init:", err)
			os.Exit(1)
		}
		return
	}
	runProcessSupervisor(os.Args[1:])
}

func runUnitChild(args []string) (result error) {
	if len(args) < 8 {
		return fmt.Errorf("internal usage: titanus-init --unit-child ROOTFS HOSTNAME READY_FD SECURITY_JSON STATUS_FD -- COMMAND [ARGS...]")
	}
	statusFD, err := strconv.Atoi(args[4])
	if err != nil || statusFD < 3 {
		return fmt.Errorf("invalid startup status fd")
	}
	status := os.NewFile(uintptr(statusFD), "titanus-startup-status")
	syscall.CloseOnExec(statusFD)
	defer func() {
		if result != nil {
			_, _ = fmt.Fprintln(status, "ERROR:", result)
		}
		_ = status.Close()
	}()
	var policy security.Policy
	if err := json.Unmarshal([]byte(args[3]), &policy); err != nil {
		return fmt.Errorf("decode security policy: %w", err)
	}
	policy.Normalize()
	if err := policy.Validate(); err != nil {
		return err
	}
	// Keep an executable descriptor across pivot_root. Re-exec after applying
	// thread credentials gives every supervisor thread the restricted identity.
	executable, err := os.Open("/proc/self/exe")
	if err != nil {
		return err
	}
	defer executable.Close()
	runtime.LockOSThread()
	// Do not unlock after security changes: this thread must execute workload.
	rootfs := filepath.Clean(args[0])
	syscall.CloseOnExec(5)
	hostname := args[1]
	readyFD, err := strconv.Atoi(args[2])
	if err != nil || readyFD < 3 {
		return fmt.Errorf("invalid runtime readiness fd %q", args[2])
	}
	count, countErr := strconv.Atoi(args[5])
	if countErr != nil || count < 0 || count > 64 {
		return fmt.Errorf("invalid disk lock count")
	}
	if args[6] != "--" {
		return fmt.Errorf("missing command separator")
	}
	command := args[7:]
	if len(command) == 0 {
		return fmt.Errorf("missing Unit command")
	}
	if err := waitForRuntime(readyFD); err != nil {
		return err
	}

	if err := syscall.Mount("", "/", "", uintptr(syscall.MS_REC|syscall.MS_PRIVATE), ""); err != nil {
		return fmt.Errorf("make mount namespace private: %w", err)
	}
	// The parent exports only this rootfs through a temporary mapped-owner
	// access path. Resolve it in this mount namespace; a host-opened directory
	// descriptor would still reference the original host mount tree.
	if err := syscall.Chdir(rootfs); err != nil {
		return fmt.Errorf("enter mapped rootfs: %w", err)
	}
	// Bind onto a child mountpoint and then resolve that path. A descriptor's
	// cwd still refers to the inherited locked mount after a self-bind on '.'.
	// The new child bind is owned by this namespace and can be pivoted safely.
	const stage = ".titanus-newroot"
	if info, e := os.Lstat(stage); e == nil {
		if !info.IsDir() {
			return fmt.Errorf("reserved root staging path is not a directory")
		}
		if e := os.Remove(stage); e != nil {
			return fmt.Errorf("reserved root staging path is not empty: %w", e)
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	if e := os.Mkdir(stage, 0700); e != nil {
		return e
	}
	rootfs = stage
	if err := syscall.Mount(".", rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind mapped root: %w", err)
	}
	if err := syscall.Sethostname([]byte(hostname)); err != nil {
		return fmt.Errorf("set hostname: %w", err)
	}
	// A user-namespace proc mount must be created while the inherited full
	// proc is still visible; mounting it after detaching the old root is denied.
	if err := mountProc(rootfs); err != nil {
		return err
	}
	if err := mountDevices(rootfs); err != nil {
		return err
	}
	if err := pivotInto(rootfs); err != nil {
		return err
	}
	if err := protectProc(); err != nil {
		return err
	}
	if err := mountTmp(); err != nil {
		return err
	}
	if err := bringLoopbackUp(); err != nil {
		return fmt.Errorf("bring loopback up: %w", err)
	}

	if err := security.SealRootFS(policy); err != nil {
		return fmt.Errorf("workload rootfs: %w", err)
	}
	if err := security.ApplyLandlock(policy, int(executable.Fd())); err != nil {
		return fmt.Errorf("workload LSM: %w", err)
	}
	if err := security.Apply(policy); err != nil {
		return fmt.Errorf("workload security: %w", err)
	}
	// Keep only the status pipe across re-exec; it closes after workload Start
	// confirms exec. No host state descriptors enter the namespace.
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(statusFD), syscall.F_SETFD, 0); errno != 0 {
		return errno
	}
	for i := 0; i < count; i++ {
		fd := 6 + i
		flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_GETFL, 0)
		if errno != 0 || int(flags)&syscall.O_ACCMODE != syscall.O_RDONLY {
			return fmt.Errorf("invalid read-only storage lock fd")
		}
		if _, _, errno = syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, 0); errno != 0 {
			return errno
		}
	}
	argv := append([]string{"titanus-init", "--supervise", strconv.Itoa(statusFD), strconv.Itoa(count), "--"}, command...)
	return syscall.Exec(fmt.Sprintf("/proc/self/fd/%d", executable.Fd()), argv, os.Environ())
}

func waitForRuntime(fd int) error {
	file := os.NewFile(uintptr(fd), "titanus-runtime-ready")
	if file == nil {
		return fmt.Errorf("open runtime readiness fd")
	}
	defer file.Close()
	var signal [1]byte
	n, err := file.Read(signal[:])
	if err != nil {
		return fmt.Errorf("wait for runtime readiness: %w", err)
	}
	if n != 1 || signal[0] != 1 {
		return fmt.Errorf("invalid runtime readiness signal")
	}
	return nil
}

func pivotInto(rootfs string) error {
	if err := syscall.Chdir(rootfs); err != nil {
		return fmt.Errorf("chdir rootfs: %w", err)
	}
	oldRoot := ".titanus-oldroot"
	if err := os.MkdirAll(oldRoot, 0700); err != nil {
		return fmt.Errorf("create old-root directory: %w", err)
	}
	if err := syscall.PivotRoot(".", oldRoot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir new root: %w", err)
	}
	if err := syscall.Unmount("/.titanus-oldroot", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	if err := os.Remove("/.titanus-oldroot"); err != nil {
		return fmt.Errorf("remove old-root directory: %w", err)
	}
	if err := os.Remove("/.titanus-newroot"); err != nil {
		return fmt.Errorf("remove root staging directory: %w", err)
	}
	return nil
}

func mountProc(rootfs string) error {
	target := filepath.Join(rootfs, "proc")
	if info, e := os.Lstat(target); e == nil {
		if !info.IsDir() {
			return fmt.Errorf("Source proc path must be a directory")
		}
	} else if os.IsNotExist(e) {
		if e := os.Mkdir(target, 0555); e != nil {
			return e
		}
	} else {
		return e
	}
	if err := syscall.Mount("proc", target, "proc", uintptr(syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC), ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}
	return nil
}

func mountDevices(rootfs string) error {
	directory := filepath.Join(rootfs, "dev")
	if info, err := os.Lstat(directory); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("Source dev path must be a directory")
		}
	} else if os.IsNotExist(err) {
		if err := os.Mkdir(directory, 0755); err != nil {
			return err
		}
	} else {
		return err
	}
	if err := syscall.Mount("tmpfs", directory, "tmpfs", syscall.MS_NOSUID, "mode=755,size=16m"); err != nil {
		return fmt.Errorf("mount /dev tmpfs: %w", err)
	}
	// Bind while source devices are attached in this mount namespace.
	// Detached source descriptors cannot be bound after pivot_root.
	for _, name := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		target := filepath.Join(directory, name)
		file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0666)
		if err != nil {
			return err
		}
		_ = file.Close()
		if err := syscall.Mount("/dev/"+name, target, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind safe device %s: %w", name, err)
		}
		if err := syscall.Mount("", target, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_NOSUID|syscall.MS_NOEXEC, ""); err != nil {
			return err
		}
	}
	pts := filepath.Join(directory, "pts")
	if err := os.Mkdir(pts, 0755); err != nil {
		return err
	}
	if err := syscall.Mount("devpts", pts, "devpts", syscall.MS_NOSUID|syscall.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620,max=256"); err != nil {
		return fmt.Errorf("mount private devpts: %w", err)
	}
	if err := os.Symlink("pts/ptmx", filepath.Join(directory, "ptmx")); err != nil {
		return err
	}
	for name, target := range map[string]string{"fd": "/proc/self/fd", "stdin": "/proc/self/fd/0", "stdout": "/proc/self/fd/1", "stderr": "/proc/self/fd/2"} {
		if err := os.Symlink(target, filepath.Join(directory, name)); err != nil {
			return err
		}
	}
	return nil
}

func mountTmp() error {
	if err := os.MkdirAll("/tmp", 01777); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", "/tmp", "tmpfs", uintptr(syscall.MS_NOSUID|syscall.MS_NODEV), "mode=1777,size=64m"); err != nil {
		return fmt.Errorf("mount /tmp: %w", err)
	}
	return nil
}

func bringLoopbackUp() error {
	iface, err := net.InterfaceByName("lo")
	if err != nil {
		return err
	}
	fd, err := syscall.Socket(syscall.AF_NETLINK, syscall.SOCK_RAW, syscall.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	const (
		rtmNewLink = 16
		nlmRequest = 1
		nlmAck     = 4
		iffUp      = 1
	)

	msg := make([]byte, 32)
	binary.LittleEndian.PutUint32(msg[0:4], uint32(len(msg)))
	binary.LittleEndian.PutUint16(msg[4:6], rtmNewLink)
	binary.LittleEndian.PutUint16(msg[6:8], nlmRequest|nlmAck)
	binary.LittleEndian.PutUint32(msg[8:12], 1)
	msg[16] = syscall.AF_UNSPEC
	binary.LittleEndian.PutUint32(msg[20:24], uint32(iface.Index))
	binary.LittleEndian.PutUint32(msg[24:28], iffUp)
	binary.LittleEndian.PutUint32(msg[28:32], iffUp)

	addr := &syscall.SockaddrNetlink{Family: syscall.AF_NETLINK}
	if err := syscall.Sendto(fd, msg, 0, addr); err != nil {
		return err
	}

	buf := make([]byte, 4096)
	n, _, err := syscall.Recvfrom(fd, buf, 0)
	if err != nil {
		return err
	}
	if n >= 20 && binary.LittleEndian.Uint16(buf[4:6]) == syscall.NLMSG_ERROR {
		code := int32(binary.LittleEndian.Uint32(buf[16:20]))
		if code != 0 {
			return syscall.Errno(-code)
		}
	}
	return nil
}

func makeDevice(major, minor int) int {
	return (major << 8) | minor
}

func execInside(command []string) error {
	path := command[0]
	if !filepath.IsAbs(path) {
		resolved, err := exec.LookPath(path)
		if err != nil {
			return err
		}
		path = resolved
	}
	return syscall.Exec(path, command, os.Environ())
}

// supervise owns all wait4 calls, including adopted orphans. os/exec.Wait
// must not race with a namespace-wide reaper.
func supervise(args []string, status *os.File) (int, error) {
	if len(args) == 0 {
		return 64, fmt.Errorf("missing command")
	}
	// Also behave as a subreaper when used outside a PID namespace.
	if _, _, errno := syscall.Syscall6(syscall.SYS_PRCTL, 36, 1, 0, 0, 0, 0); errno != 0 {
		return 1, errno
	}
	signals := make(chan os.Signal, 32)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGUSR1, syscall.SIGUSR2, syscall.SIGCHLD)
	defer signal.Stop(signals)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 1, err
	}
	defer cmd.Process.Release()
	if status != nil {
		if _, err := status.Write([]byte("READY\n")); err != nil {
			_ = cmd.Process.Kill()
			return 1, err
		}
		_ = status.Close()
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		var wait syscall.WaitStatus
		for {
			pid, err := syscall.Wait4(-1, &wait, syscall.WNOHANG, nil)
			if err == syscall.EINTR {
				continue
			}
			if err != nil && err != syscall.ECHILD {
				return 1, err
			}
			if pid <= 0 {
				break
			}
			if pid == cmd.Process.Pid {
				// Kill the remaining process group; exiting namespace PID 1 also kills
				// descendants that created their own process groups or sessions.
				_ = syscall.Kill(-pid, syscall.SIGKILL)
				if wait.Signaled() {
					return 128 + int(wait.Signal()), nil
				}
				return wait.ExitStatus(), nil
			}
		}
		select {
		case sig := <-signals:
			if s, ok := sig.(syscall.Signal); ok && s != syscall.SIGCHLD {
				_ = syscall.Kill(-cmd.Process.Pid, s)
			}
		case <-ticker.C:
		}
	}
}

func runProcessSupervisor(args []string) {
	code, err := supervise(args, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "titanus-init:", err)
	}
	os.Exit(code)
}
