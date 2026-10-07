package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func main() {
	if e := verify(); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	fmt.Println("TITANUS_ISOLATION_OK")
	if len(os.Args) == 1 {
		time.Sleep(60 * time.Second)
	}
}
func verify() error {
	// Permissions and writable rootfs would allow this write without Landlock.
	for _, path := range []string{"/forbidden", "/alias/forbidden"} {
		fd, e := syscall.Open(path, syscall.O_WRONLY|syscall.O_TRUNC, 0)
		if e == nil {
			syscall.Close(fd)
			return fmt.Errorf("LSM allowed %s", path)
		}
		if e != syscall.EACCES {
			return fmt.Errorf("LSM denial %s: %v", path, e)
		}
	}
	for _, path := range []string{"/data/kmsg", "/data/block"} {
		fd, e := syscall.Open(path, syscall.O_RDONLY, 0)
		if e == nil {
			syscall.Close(fd)
			return fmt.Errorf("device allowed %s", path)
		}
		if e != syscall.EPERM {
			return fmt.Errorf("device denial %s: %v", path, e)
		}
	}
	for _, path := range []string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom"} {
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		f.Close()
	}
	f, e := os.OpenFile("/dev/null", os.O_WRONLY, 0)
	if e != nil {
		return e
	}
	f.Close()
	if e := os.WriteFile("/tmp/allowed", []byte("ok"), 0600); e != nil {
		return e
	}
	if e := os.WriteFile("/data/allowed", []byte("ok"), 0600); e != nil {
		return e
	}
	for _, path := range []string{"/proc/kcore", "/proc/keys", "/proc/kallsyms"} {
		data, e := os.ReadFile(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil || len(data) != 0 {
			return fmt.Errorf("unmasked %s: %v len=%d", path, e, len(data))
		}
	}
	data, e := os.ReadFile("/proc/self/mountinfo")
	if e != nil {
		return e
	}
	for _, path := range []string{"/proc/sys", "/proc/irq", "/proc/bus", "/proc/fs", "/proc/sysrq-trigger"} {
		if _, e := os.Stat(path); os.IsNotExist(e) {
			continue
		}
		found := false
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) > 5 && fields[4] == path && strings.HasPrefix(fields[5], "ro,") {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("proc path not mounted read-only: %s", path)
		}
	}
	// Parent namespace root is never in this map. Compare from the host too.
	data, e = os.ReadFile("/proc/self/uid_map")
	if e != nil {
		return e
	}
	fields := strings.Fields(string(data))
	if len(fields) != 3 || fields[0] != "0" || fields[1] == "0" || fields[2] != "65536" {
		return fmt.Errorf("unsafe uid map %s", data)
	}
	if os.Getuid() != 0 || os.Getgid() != 0 {
		return fmt.Errorf("container root identity lost")
	}
	if len(os.Args) == 1 {
		out, e := exec.Command("/bin/isolation-probe", "descendant").CombinedOutput()
		if e != nil || !strings.Contains(string(out), "TITANUS_ISOLATION_OK") {
			return fmt.Errorf("descendant: %v %s", e, out)
		}
	}
	return nil
}
