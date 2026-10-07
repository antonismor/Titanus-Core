package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/antonismor/Titanus-Core/internal/security"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "--unit-child" {
		if err := runUnitChild(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "titanus-init:", err)
			os.Exit(1)
		}
		return
	}
	runProcessSupervisor(os.Args[1:])
}

func runUnitChild(args []string) error {
	if len(args) < 9 {
		return fmt.Errorf("internal usage: titanus-init --unit-child ROOTFS HOSTNAME READY_FD PROFILE UID GID READONLY -- COMMAND [ARGS...]")
	}
	rootfs := filepath.Clean(args[0])
	hostname := args[1]
	readyFD, err := strconv.Atoi(args[2])
	if err != nil || readyFD < 3 {
		return fmt.Errorf("invalid runtime readiness fd %q", args[2])
	}
	uid, err := strconv.Atoi(args[4])
	if err != nil {
		return fmt.Errorf("invalid Unit UID %q", args[4])
	}
	gid, err := strconv.Atoi(args[5])
	if err != nil {
		return fmt.Errorf("invalid Unit GID %q", args[5])
	}
	readOnlyRootFS, err := strconv.ParseBool(args[6])
	if err != nil {
		return fmt.Errorf("invalid Unit read-only-rootfs value %q", args[6])
	}
	securitySpec := security.Spec{
		Profile: args[3], RunAsUID: uid, RunAsGID: gid, ReadOnlyRootFS: readOnlyRootFS,
	}
	securitySpec.Normalize()
	if err := securitySpec.Validate(); err != nil {
		return err
	}
	if args[7] != "--" {
		return fmt.Errorf("missing command separator")
	}
	command := args[8:]
	if len(command) == 0 {
		return fmt.Errorf("missing Unit command")
	}
	if err := waitForRuntime(readyFD); err != nil {
		return err
	}

	if err := syscall.Mount("", "/", "", uintptr(syscall.MS_REC|syscall.MS_PRIVATE), ""); err != nil {
		return fmt.Errorf("make mount namespace private: %w", err)
	}
	if err := syscall.Sethostname([]byte(hostname)); err != nil {
		return fmt.Errorf("set hostname: %w", err)
	}
	if err := pivotInto(rootfs); err != nil {
		return err
	}
	if err := mountProc(); err != nil {
		return err
	}
	if err := mountDevices(); err != nil {
		return err
	}
	if err := mountTmp(); err != nil {
		return err
	}
	if err := bringLoopbackUp(); err != nil {
		return fmt.Errorf("bring loopback up: %w", err)
	}
	if err := security.Apply(securitySpec); err != nil {
		return fmt.Errorf("apply Unit Security Profile: %w", err)
	}

	return execInside(command)
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
	return nil
}

func mountProc() error {
	if err := os.MkdirAll("/proc", 0555); err != nil {
		return err
	}
	if err := syscall.Mount("proc", "/proc", "proc", uintptr(syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC), ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}
	return nil
}

func mountDevices() error {
	if err := os.MkdirAll("/dev", 0755); err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", "/dev", "tmpfs", uintptr(syscall.MS_NOSUID), "mode=755,size=16m"); err != nil {
		return fmt.Errorf("mount /dev tmpfs: %w", err)
	}

	devices := []struct {
		path  string
		major int
		minor int
		mode  uint32
	}{
		{"/dev/null", 1, 3, 0666},
		{"/dev/zero", 1, 5, 0666},
		{"/dev/random", 1, 8, 0666},
		{"/dev/urandom", 1, 9, 0666},
		{"/dev/tty", 5, 0, 0666},
	}
	for _, device := range devices {
		mode := uint32(syscall.S_IFCHR) | device.mode
		if err := syscall.Mknod(device.path, mode, makeDevice(device.major, device.minor)); err != nil {
			return fmt.Errorf("create %s: %w", device.path, err)
		}
	}

	if err := os.MkdirAll("/dev/pts", 0755); err != nil {
		return err
	}
	if err := syscall.Mount("devpts", "/dev/pts", "devpts", uintptr(syscall.MS_NOSUID|syscall.MS_NOEXEC), "newinstance,ptmxmode=0666,mode=0620"); err == nil {
		_ = os.Remove("/dev/ptmx")
		_ = os.Symlink("pts/ptmx", "/dev/ptmx")
	}

	for name, target := range map[string]string{
		"/dev/fd":     "/proc/self/fd",
		"/dev/stdin":  "/proc/self/fd/0",
		"/dev/stdout": "/proc/self/fd/1",
		"/dev/stderr": "/proc/self/fd/2",
	} {
		_ = os.Symlink(target, name)
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

func runProcessSupervisor(args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: titanus-init <command> [args...]")
		os.Exit(64)
	}

	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "titanus-init:", err)
		os.Exit(1)
	}

	signals := make(chan os.Signal, 16)
	signal.Notify(signals)
	go func() {
		for sig := range signals {
			if s, ok := sig.(syscall.Signal); ok {
				_ = syscall.Kill(-cmd.Process.Pid, s)
			}
		}
	}()

	err := cmd.Wait()
	signal.Stop(signals)
	close(signals)

	if err == nil {
		return
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				os.Exit(128 + int(status.Signal()))
			}
			os.Exit(status.ExitStatus())
		}
	}
	fmt.Fprintln(os.Stderr, "titanus-init:", err)
	os.Exit(1)
}
