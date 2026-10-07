//go:build linux

package security

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const (
	prCapBsetDrop     = 24
	prSetNoNewPrivs   = 38
	prSetSeccomp      = 22
	seccompModeFilter = 2

	seccompRetKillProcess = 0x80000000
	seccompRetErrno       = 0x00050000
	seccompRetAllow       = 0x7fff0000

	bpfLD  = 0x00
	bpfW   = 0x00
	bpfABS = 0x20
	bpfJMP = 0x05
	bpfJEQ = 0x10
	bpfJGE = 0x30
	bpfK   = 0x00
	bpfRET = 0x06

	linuxCapabilityVersion3 = 0x20080522
)

type sockFilter struct {
	Code uint16
	Jt   uint8
	Jf   uint8
	K    uint32
}

type sockFprog struct {
	Len    uint16
	Filter *sockFilter
}

type capUserHeader struct {
	Version uint32
	PID     int32
}

type capUserData struct {
	Effective   uint32
	Permitted   uint32
	Inheritable uint32
}

func Apply(spec Spec) error {
	spec.Normalize()
	if err := spec.Validate(); err != nil {
		return err
	}
	if spec.ReadOnlyRootFS {
		if err := syscall.Mount("", "/", "", uintptr(syscall.MS_REMOUNT|syscall.MS_RDONLY), ""); err != nil {
			return fmt.Errorf("remount Unit rootfs read-only: %w", err)
		}
	}

	if spec.Profile == ProfileRestricted {
		if err := dropCapabilityBoundingSet(); err != nil {
			return fmt.Errorf("drop capability bounding set: %w", err)
		}
		if err := prctl(prSetNoNewPrivs, 1, 0, 0, 0); err != nil {
			return fmt.Errorf("PR_SET_NO_NEW_PRIVS: %w", err)
		}
		if err := installRestrictedSeccomp(); err != nil {
			return fmt.Errorf("install seccomp filter: %w", err)
		}
	}

	if spec.RunAsGID != 0 || spec.RunAsUID != 0 {
		if err := syscall.Setgroups([]int{}); err != nil {
			return fmt.Errorf("clear supplementary groups: %w", err)
		}
	}
	if spec.RunAsGID != 0 {
		if err := syscall.Setgid(spec.RunAsGID); err != nil {
			return fmt.Errorf("setgid(%d): %w", spec.RunAsGID, err)
		}
	}
	if spec.RunAsUID != 0 {
		if err := syscall.Setuid(spec.RunAsUID); err != nil {
			return fmt.Errorf("setuid(%d): %w", spec.RunAsUID, err)
		}
	}

	if spec.Profile == ProfileRestricted {
		if err := clearProcessCapabilities(); err != nil {
			return fmt.Errorf("clear process capabilities: %w", err)
		}
	}
	return nil
}

func ApplyProfile(value string) error {
	return Apply(Spec{Profile: value})
}

func dropCapabilityBoundingSet() error {
	last := 63
	if data, err := os.ReadFile("/proc/sys/kernel/cap_last_cap"); err == nil {
		if parsed, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && parsed >= 0 && parsed <= 255 {
			last = parsed
		}
	}
	for capability := 0; capability <= last; capability++ {
		if err := prctl(prCapBsetDrop, uintptr(capability), 0, 0, 0); err != nil {
			if errors.Is(err, syscall.EINVAL) {
				continue
			}
			return fmt.Errorf("capability %d: %w", capability, err)
		}
	}
	return nil
}

func clearProcessCapabilities() error {
	header := capUserHeader{Version: linuxCapabilityVersion3}
	data := [2]capUserData{}
	_, _, errno := syscall.RawSyscall(
		syscall.SYS_CAPSET,
		uintptr(unsafe.Pointer(&header)),
		uintptr(unsafe.Pointer(&data[0])),
		0,
	)
	runtime.KeepAlive(header)
	runtime.KeepAlive(data)
	if errno != 0 {
		return errno
	}
	return nil
}

func installRestrictedSeccomp() error {
	arch, rejectX32, err := auditArchitecture()
	if err != nil {
		return err
	}
	filters := []sockFilter{
		stmt(bpfLD|bpfW|bpfABS, 4),
		jump(bpfJMP|bpfJEQ|bpfK, arch, 1, 0),
		stmt(bpfRET|bpfK, seccompRetKillProcess),
		stmt(bpfLD|bpfW|bpfABS, 0),
	}
	if rejectX32 {
		filters = append(filters,
			jump(bpfJMP|bpfJGE|bpfK, 0x40000000, 0, 1),
			stmt(bpfRET|bpfK, seccompRetKillProcess),
		)
	}
	for _, number := range restrictedSyscalls() {
		filters = append(filters,
			jump(bpfJMP|bpfJEQ|bpfK, uint32(number), 0, 1),
			stmt(bpfRET|bpfK, seccompRetErrno|uint32(syscall.EPERM)),
		)
	}
	filters = append(filters, stmt(bpfRET|bpfK, seccompRetAllow))
	program := sockFprog{Len: uint16(len(filters)), Filter: &filters[0]}
	_, _, errno := syscall.RawSyscall6(
		syscall.SYS_PRCTL,
		prSetSeccomp,
		seccompModeFilter,
		uintptr(unsafe.Pointer(&program)),
		0, 0, 0,
	)
	runtime.KeepAlive(filters)
	runtime.KeepAlive(program)
	if errno != 0 {
		return errno
	}
	return nil
}

func auditArchitecture() (uint32, bool, error) {
	switch runtime.GOARCH {
	case "amd64":
		return 0xc000003e, true, nil
	case "arm64":
		return 0xc00000b7, false, nil
	default:
		return 0, false, fmt.Errorf("restricted seccomp profile is not implemented for %s", runtime.GOARCH)
	}
}

func restrictedSyscalls() []uintptr {
	return []uintptr{
		syscall.SYS_PTRACE,
		syscall.SYS_MOUNT,
		syscall.SYS_UMOUNT2,
		syscall.SYS_REBOOT,
		syscall.SYS_SWAPON,
		syscall.SYS_SWAPOFF,
		syscall.SYS_INIT_MODULE,
		syscall.SYS_DELETE_MODULE,
		syscall.SYS_KEXEC_LOAD,
		syscall.SYS_PERF_EVENT_OPEN,
		setnsSyscallNumber(),
		syscall.SYS_UNSHARE,
		syscall.SYS_PIVOT_ROOT,
		syscall.SYS_ACCT,
		syscall.SYS_SYSLOG,
	}
}

func setnsSyscallNumber() uintptr {
	switch runtime.GOARCH {
	case "amd64":
		return 308
	case "arm64":
		return 268
	default:
		return ^uintptr(0)
	}
}

func stmt(code uint16, k uint32) sockFilter {
	return sockFilter{Code: code, K: k}
}

func jump(code uint16, k uint32, jt, jf uint8) sockFilter {
	return sockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

func prctl(option, arg2, arg3, arg4, arg5 uintptr) error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, option, arg2, arg3, arg4, arg5, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
