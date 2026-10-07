//go:build linux

package security

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// Apply must be called on a locked OS thread immediately before syscall.Exec.
// Capabilities and no_new_privs are thread attributes; exec keeps the locked
// thread and discards the remaining Go runtime threads. Seccomp uses TSYNC so
// even the helper's other threads cannot run outside the filter.
func Apply(p Policy) error {
	p.Normalize()
	if err := p.Validate(); err != nil {
		return err
	}
	filter, err := DefaultFilter(runtime.GOARCH)
	if err != nil {
		return err
	}
	if p.ReadOnlyRootFS {
		if err := syscall.Mount("", "/", "", uintptr(syscall.MS_REMOUNT|syscall.MS_RDONLY), ""); err != nil {
			return fmt.Errorf("remount Unit rootfs read-only: %w", err)
		}
	}
	if err := prctl(38, 1, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	if err := dropCapabilities(p.capabilityMask(), p.RunAsUID, p.RunAsGID); err != nil {
		return err
	}
	program := syscall.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	nr := uintptr(317)
	if runtime.GOARCH == "arm64" {
		nr = 277
	}
	// SECCOMP_SET_MODE_FILTER=1, SECCOMP_FILTER_FLAG_TSYNC=1.
	r, _, errno := syscall.RawSyscall(nr, 1, 1, uintptr(unsafe.Pointer(&program)))
	runtime.KeepAlive(filter)
	if errno != 0 {
		return fmt.Errorf("install seccomp with TSYNC: %w", errno)
	}
	if r != 0 {
		return fmt.Errorf("seccomp TSYNC failed on thread %d", r)
	}
	return nil
}

func prctl(option, arg2, arg3 uintptr) error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, option, arg2, arg3, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func dropCapabilities(mask uint64, uid, gid int) error {
	// Clear inherited ambient privileges before applying an explicit set.
	if err := prctl(47, 4, 0); err != nil {
		return fmt.Errorf("clear ambient capabilities: %w", err)
	}
	// Disable root's special exec semantics, lock it, disable setuid fixups and
	// lock KEEP_CAPS off. UID 0 cannot regain the dropped sets by executing.
	const securebits = 1 | 2 | 4 | 8 | 32
	if err := prctl(28, securebits, 0); err != nil {
		return fmt.Errorf("lock securebits: %w", err)
	}
	// Probe through PR_CAPBSET_READ rather than /proc: the Unit's proc mount
	// need not expose the host's cap_last_cap. Drop every kernel-supported cap.
	for cap := uint(0); ; cap++ {
		_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, 23, uintptr(cap), 0, 0, 0, 0)
		if errno == syscall.EINVAL {
			break
		}
		if errno != 0 {
			return fmt.Errorf("read capability bounding set: %w", errno)
		}
		if cap >= 64 || mask&(uint64(1)<<cap) == 0 {
			if err := prctl(24, uintptr(cap), 0); err != nil {
				return fmt.Errorf("drop bounding capability %d: %w", cap, err)
			}
		}
	}
	// Change only the locked exec thread's credentials. Runtime helper threads
	// are discarded by exec; using Go's all-thread setters here would mutate
	// threads whose capability/securebits state has not been changed.
	for _, call := range []struct{ nr, a, b, c uintptr }{
		{syscall.SYS_SETGROUPS, 0, 0, 0},
		{syscall.SYS_SETRESGID, uintptr(gid), uintptr(gid), uintptr(gid)},
		{syscall.SYS_SETRESUID, uintptr(uid), uintptr(uid), uintptr(uid)},
	} {
		_, _, errno := syscall.RawSyscall(call.nr, call.a, call.b, call.c)
		if errno != 0 {
			return fmt.Errorf("set workload credentials: %w", errno)
		}
	}
	header := struct {
		Version uint32
		PID     int32
	}{Version: 0x20080522}
	type capData struct{ Effective, Permitted, Inheritable uint32 }
	data := [2]capData{
		{uint32(mask), uint32(mask), uint32(mask)},
		{uint32(mask >> 32), uint32(mask >> 32), uint32(mask >> 32)},
	}
	_, _, errno := syscall.RawSyscall(syscall.SYS_CAPSET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&data[0])), 0)
	if errno != 0 {
		return fmt.Errorf("set workload capabilities: %w", errno)
	}
	// Explicitly retained application privileges survive exec via ambient caps,
	// even with NOROOT locked. Default mask=0 leaves all five sets empty.
	for cap := uint(0); cap < 64; cap++ {
		if mask&(uint64(1)<<cap) != 0 {
			if err := prctl(47, 2, uintptr(cap)); err != nil {
				return fmt.Errorf("raise ambient capability %d: %w", cap, err)
			}
		}
	}
	return nil
}
