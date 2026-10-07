//go:build linux

package security

import (
	"fmt"
	"syscall"
)

type archProfile struct {
	audit   uint32
	clone   uint32
	clone3  uint32
	allowed []uint32
}

const (
	actionKill   uint32 = 0x80000000 // SECCOMP_RET_KILL_PROCESS
	actionAllow  uint32 = 0x7fff0000
	actionDenied uint32 = 0x00050000 | uint32(syscall.EPERM)
	actionNoSys  uint32 = 0x00050000 | uint32(syscall.ENOSYS)
)

// DefaultFilter is an architecture-checked, default-deny cBPF allowlist. New
// kernel syscalls remain blocked until explicitly reviewed in a profile update.
func DefaultFilter(arch string) ([]syscall.SockFilter, error) {
	var profile archProfile
	switch arch {
	case "amd64":
		profile = amd64Profile()
	case "arm64":
		profile = arm64Profile()
	default:
		return nil, fmt.Errorf("seccomp profile unsupported on %s", arch)
	}
	stmt := func(code uint16, k uint32) syscall.SockFilter { return syscall.SockFilter{Code: code, K: k} }
	jump := func(code uint16, k uint32, jt, jf uint8) syscall.SockFilter {
		return syscall.SockFilter{Code: code, K: k, Jt: jt, Jf: jf}
	}
	filter := []syscall.SockFilter{
		stmt(0x20, 4),                   // LD W ABS seccomp_data.arch
		jump(0x15, profile.audit, 1, 0), // JEQ native ABI
		stmt(0x06, actionKill),
		stmt(0x20, 0), // LD W ABS seccomp_data.nr
	}
	if arch == "amd64" {
		// Reject the x32 ABI, including the historical 512..547 confusion range.
		filter = append(filter, jump(0x35, 0x40000000, 0, 1), stmt(0x06, actionKill))
	}
	filter = append(filter,
		// clone3 has pointer-based flags cBPF cannot inspect; ENOSYS allows libc
		// to fall back to clone, whose namespace flags we can safely inspect.
		jump(0x15, profile.clone3, 0, 1), stmt(0x06, actionNoSys),
		jump(0x15, profile.clone, 0, 4),
		stmt(0x20, 16),               // low 32 bits of clone flags (args[0])
		jump(0x45, 0x7e020080, 0, 1), // JSET namespace flags + CLONE_NEWTIME
		stmt(0x06, actionDenied),
		stmt(0x06, actionAllow),
	)
	for _, nr := range profile.allowed {
		filter = append(filter, jump(0x15, nr, 0, 1), stmt(0x06, actionAllow))
	}
	filter = append(filter, stmt(0x06, actionDenied))
	return filter, nil
}
