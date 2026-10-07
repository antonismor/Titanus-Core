//go:build linux

package security

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

// landlock-v1 requires ABI 3, including REFER and TRUNCATE. No disabled or
// older-kernel fallback is permitted. It intentionally does not claim ioctl,
// network or newer-ABI coverage: devices and networking have separate controls.
const landlockRights uint64 = (1 << 15) - 1
const landlockRead uint64 = 1 | 1<<2 | 1<<3
const landlockWrite uint64 = landlockRights &^ (1<<6 | 1<<11) // never MAKE_CHAR/BLOCK

func LandlockABI() (int, error) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return 0, fmt.Errorf("unsupported Landlock architecture")
	}
	r, _, e := syscall.RawSyscall(444, 0, 0, 1)
	if e != 0 {
		return 0, fmt.Errorf("Landlock is required: %w", e)
	}
	if r < 3 {
		return int(r), fmt.Errorf("landlock-v1 requires ABI >= 3, got %d", r)
	}
	return int(r), nil
}

// ApplyLandlock is called on the locked exec thread inside the final Unit
// filesystem. The immediate re-exec removes all pre-existing runtime threads.
func ApplyLandlock(p Policy, executableFD int) error {
	p.Normalize()
	if e := p.Validate(); e != nil {
		return e
	}
	if _, e := LandlockABI(); e != nil {
		return e
	}
	rights := landlockRights
	fd, _, e := syscall.RawSyscall(444, uintptr(unsafe.Pointer(&rights)), 8, 0)
	if e != 0 {
		return fmt.Errorf("create Landlock ruleset: %w", e)
	}
	defer syscall.Close(int(fd))
	addFD := func(pathFD int, access uint64) error {
		// Kernel UAPI is packed: u64 access + s32 fd, exactly 12 bytes.
		var rule [12]byte
		binary.LittleEndian.PutUint64(rule[:8], access)
		binary.LittleEndian.PutUint32(rule[8:], uint32(pathFD))
		_, _, e := syscall.RawSyscall6(445, fd, 1, uintptr(unsafe.Pointer(&rule[0])), 0, 0, 0)
		runtime.KeepAlive(rule)
		if e != 0 {
			return e
		}
		return nil
	}
	add := func(path string, access uint64) error {
		pathFD, e := syscall.Open(path, 0x200000|syscall.O_CLOEXEC, 0) // O_PATH
		if e != nil {
			return fmt.Errorf("open LSM path %s: %w", path, e)
		}
		defer syscall.Close(pathFD)
		if e := addFD(pathFD, access); e != nil {
			return fmt.Errorf("LSM path %s: %w", path, e)
		}
		return nil
	}
	if e := add("/", landlockRead); e != nil {
		return e
	}
	// The init executable is outside the pivoted filesystem. Grant this single
	// already-open file read/exec; close it on exec, never grant the old host tree.
	if executableFD >= 0 {
		if e := addFD(executableFD, 1|1<<2); e != nil {
			return e
		}
	}
	for _, path := range p.LSMWritePaths {
		resolved, e := filepath.EvalSymlinks(path)
		if e != nil {
			return e
		}
		checked := p
		checked.LSMWritePaths = []string{resolved}
		if e := checked.Validate(); e != nil {
			return e
		}
		info, e := os.Stat(resolved)
		if e != nil {
			return e
		}
		if !info.IsDir() {
			return fmt.Errorf("LSM writable path must be directory: %s", path)
		}
		if e := add(resolved, landlockWrite); e != nil {
			return e
		}
	}
	for _, path := range []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty"} {
		if e := add(path, 1<<1|1<<2); e != nil {
			return e
		}
	}
	if e := add("/dev/pts", 1<<1|1<<2|1<<3); e != nil {
		return e
	}
	if e := prctl(38, 1, 0); e != nil {
		return e
	}
	_, _, e = syscall.RawSyscall(446, fd, 0, 0)
	if e != 0 {
		return fmt.Errorf("enforce Landlock: %w", e)
	}
	return nil
}
