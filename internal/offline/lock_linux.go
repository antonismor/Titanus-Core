// Package offline coordinates host-wide maintenance with supported Titanus
// processes. It does not stop services, workloads or external storage clients.
package offline

import (
	"fmt"
	"os"
	"syscall"
)

const lockPath = "/run/titanus-offline.lock"

func Shared() (*os.File, error) {
	// Unprivileged local planning/diagnostics cannot mutate root-owned host state.
	if os.Geteuid() != 0 {
		return nil, nil
	}
	return acquire(lockPath, false)
}

func Exclusive() (*os.File, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("offline host maintenance requires root")
	}
	return acquire(lockPath, true)
}

func acquire(path string, exclusive bool) (*os.File, error) {
	fd, e := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(fd), "titanus-offline-lock")
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, fmt.Errorf("unsafe offline lock")
	}
	mode := syscall.LOCK_SH | syscall.LOCK_NB
	if exclusive {
		mode = syscall.LOCK_EX | syscall.LOCK_NB
	}
	if e = syscall.Flock(fd, mode); e != nil {
		f.Close()
		return nil, fmt.Errorf("Titanus is running or offline maintenance is active: %w", e)
	}
	return f, nil
}

func CheckStartup(stateRoot string) error {
	for _, path := range []string{stateRoot + ".recovery-pending", stateRoot + ".backup-pending"} {
		if _, e := os.Lstat(path); !os.IsNotExist(e) {
			return fmt.Errorf("unfinished offline maintenance blocks startup: %s", path)
		}
	}
	return nil
}
