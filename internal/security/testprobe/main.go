// testprobe is built only for security integration checks and native runtime CI.
package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/antonismor/Titanus-Core/internal/security"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: probe apply|verify|nonroot MASK [SETUID_HELPER]")
	}
	mask, err := strconv.ParseUint(os.Args[2], 16, 64)
	if err != nil {
		return err
	}
	if os.Args[1] == "apply" || os.Args[1] == "nonroot" {
		runtime.LockOSThread()
		p := security.Policy{}
		if mask == 1<<10 {
			p.Capabilities = []string{"NET_BIND_SERVICE"}
		}
		if os.Args[1] == "nonroot" {
			p.Capabilities = []string{"SETUID"}
		}
		if err := security.Apply(p); err != nil {
			return err
		}
		if os.Args[1] == "nonroot" {
			if err := syscall.Setresuid(1000, 1000, 1000); err != nil {
				return err
			}
			return syscall.Exec(os.Args[3], []string{os.Args[3]}, os.Environ())
		}
		path, err := os.Executable()
		if err != nil {
			return err
		}
		return syscall.Exec(path, []string{path, "verify", os.Args[2]}, os.Environ())
	}
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return err
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		pair := strings.Fields(line)
		if len(pair) >= 2 {
			fields[strings.TrimSuffix(pair[0], ":")] = pair[1]
		}
	}
	for _, name := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		got, err := strconv.ParseUint(fields[name], 16, 64)
		if err != nil || got != mask {
			return fmt.Errorf("%s=%s, want %x", name, fields[name], mask)
		}
	}
	if fields["NoNewPrivs"] != "1" || fields["Seccomp"] != "2" {
		return fmt.Errorf("unsafe kernel status: %s", data)
	}
	if mask == 0 {
		if err := syscall.Mknod("/tmp/titanus-forbidden-device", syscall.S_IFCHR|0600, 0x103); err != syscall.EPERM {
			return fmt.Errorf("mknod got %v, want EPERM", err)
		}
	}
	// getpriority does not require capabilities; the filter must block keyctl
	// itself before argument validation. New syscalls also default to EPERM.
	keyctl := uintptr(250)
	if runtime.GOARCH == "arm64" {
		keyctl = 219
	}
	for _, nr := range []uintptr{keyctl, 9999} {
		_, _, err := syscall.RawSyscall(nr, ^uintptr(0), 0, 0)
		if err != syscall.EPERM {
			return fmt.Errorf("syscall %d got %v, want EPERM", nr, err)
		}
	}
	_, _, errno := syscall.RawSyscall(435, 0, 0, 0)
	if errno != syscall.ENOSYS {
		return fmt.Errorf("clone3 got %v, want ENOSYS", errno)
	}
	if err := syscall.Unshare(syscall.CLONE_NEWUSER); err != syscall.EPERM {
		return fmt.Errorf("unshare got %v", err)
	}
	_, _, errno = syscall.RawSyscall6(syscall.SYS_PRCTL, 38, 0, 0, 0, 0, 0)
	if errno == 0 {
		return fmt.Errorf("no_new_privs was reversible")
	}
	// Exercise ordinary workload I/O, networking, threads, forks and exec.
	file, err := os.CreateTemp("/tmp", "titanus-probe-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.WriteString("OK"); err != nil {
		return err
	}
	file.Close()
	done := make(chan error, 1)
	go func() { _, err := os.ReadFile(name); done <- err }()
	if err := <-done; err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_, err = conn.Write([]byte("OK"))
			conn.Close()
		}
		done <- err
	}()
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 2)
	_, err = io.ReadFull(conn, buf)
	conn.Close()
	if err != nil || string(buf) != "OK" {
		return fmt.Errorf("ordinary socket I/O: %v", err)
	}
	if err := <-done; err != nil {
		return err
	}
	// The verification also runs in a fork/exec descendant of the workload.
	if len(os.Args) < 4 {
		path, err := os.Executable()
		if err != nil {
			return err
		}
		output, err := exec.Command(path, "verify", os.Args[2], "descendant").CombinedOutput()
		if err != nil || !strings.Contains(string(output), "TITANUS_SECURITY_OK") {
			return fmt.Errorf("descendant security: %v: %s", err, output)
		}
	}
	fmt.Println("TITANUS_SECURITY_OK")
	return nil
}
