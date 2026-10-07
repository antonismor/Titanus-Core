package unitruntime

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func awaitMonitorPID(file *os.File, timeout time.Duration) (int, error) {
	if err := file.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}
	data, err := io.ReadAll(io.LimitReader(file, 64))
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("host monitor did not report namespace PID")
	}
	return pid, nil
}

func (m *Manager) lock() (func(), error) {
	if err := os.MkdirAll(m.cfg.StateRoot, 0750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(m.cfg.StateRoot, "runtime.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

func (m *Manager) belongsToUnit(id string, pid int) bool {
	data, err := os.ReadFile(filepath.Join(m.cgroupDir(id), "cgroup.procs"))
	if err != nil {
		return false
	}
	for _, value := range strings.Fields(string(data)) {
		if value == strconv.Itoa(pid) {
			return true
		}
	}
	return false
}

func (m *Manager) applyExitRecord(state *State) bool {
	var record ExitRecord
	if state.RunID == "" || loadJSON(filepath.Join(m.unitDir(state.ID), "exit.json"), &record) != nil || record.RunID != state.RunID {
		return false
	}
	if state.ExitCode != nil && *state.ExitCode == record.Code && state.StoppedAt == record.ExitedAt {
		return false
	}
	state.ExitCode = &record.Code
	state.ExitSignal = record.Signal
	state.StoppedAt = record.ExitedAt
	if state.HealthFailed {
		state.Status = StatusFailed
		state.LastError = "liveness probe failed"
	} else if state.Status != StatusStopped {
		if record.Code == 0 {
			state.Status = StatusStopped
			state.LastError = ""
		} else {
			state.Status = StatusFailed
			state.LastError = fmt.Sprintf("workload exited with code %d", record.Code)
		}
	}
	return true
}

// Recover reconciles persisted Units after a daemon/node restart. Live verified
// processes are adopted; dead processes have runtime resources detached. The
// independent host monitor continues to publish exit results while daemon is
// absent. Rebooted nodes reject the previous boot's process identities.
func (m *Manager) Recover() error {
	entries, err := os.ReadDir(filepath.Join(m.cfg.StateRoot, "units"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, _, err := m.Inspect(entry.Name()); err != nil {
			return fmt.Errorf("recover Unit %s: %w", entry.Name(), err)
		}
	}
	return nil
}
