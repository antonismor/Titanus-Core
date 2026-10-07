package main

import (
	"fmt"
	"os"
	"syscall"
)

// A fresh PID-namespace proc mount is further narrowed. Missing optional kernel
// paths may be absent; any other error aborts startup. No host proc bind mount.
func protectProc() error {
	for _, path := range []string{"/proc/sys", "/proc/sysrq-trigger", "/proc/irq", "/proc/bus", "/proc/fs"} {
		if _, e := os.Lstat(path); os.IsNotExist(e) {
			continue
		} else if e != nil {
			return e
		}
		if e := syscall.Mount(path, path, "", syscall.MS_BIND|syscall.MS_REC, ""); e != nil {
			return fmt.Errorf("protect %s: %w", path, e)
		}
		if e := syscall.Mount("", path, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); e != nil {
			return fmt.Errorf("readonly %s: %w", path, e)
		}
	}
	const directory = "/.titanus-proc-mask"
	if e := os.MkdirAll(directory, 0700); e != nil {
		return e
	}
	if e := syscall.Mount("tmpfs", directory, "tmpfs", syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "size=4k,mode=700"); e != nil {
		return e
	}
	mask := directory + "/empty"
	if e := os.WriteFile(mask, nil, 0444); e != nil {
		return e
	}
	for _, path := range []string{"/proc/kcore", "/proc/keys", "/proc/timer_list", "/proc/interrupts", "/proc/sched_debug", "/proc/kallsyms", "/proc/slabinfo"} {
		if _, e := os.Lstat(path); os.IsNotExist(e) {
			continue
		} else if e != nil {
			return e
		}
		if e := syscall.Mount(mask, path, "", syscall.MS_BIND, ""); e != nil {
			return fmt.Errorf("mask %s: %w", path, e)
		}
		if e := syscall.Mount("", path, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY|syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, ""); e != nil {
			return e
		}
	}
	if e := syscall.Unmount(directory, 0); e != nil {
		return e
	}
	if e := os.Remove(directory); e != nil {
		return e
	}
	return nil
}
