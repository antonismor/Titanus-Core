package unitruntime

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// ProcessIdentity survives daemon restarts and rejects both PID reuse and boot
// changes. A pidfd pins the identity across validation and signal delivery.
type ProcessIdentity struct {
	BootID     string `json:"boot_id"`
	StartTicks uint64 `json:"start_ticks"`
}

func readProcessIdentity(pid int) (ProcessIdentity, error) {
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ProcessIdentity{}, err
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ProcessIdentity{}, err
	}
	return parseProcessIdentity(strings.TrimSpace(string(boot)), string(stat))
}

func parseProcessIdentity(boot, stat string) (ProcessIdentity, error) {
	// comm may contain spaces and ')' characters; fields begin after its final ')'.
	i := strings.LastIndex(stat, ")")
	if i < 0 {
		return ProcessIdentity{}, fmt.Errorf("invalid proc stat")
	}
	fields := strings.Fields(stat[i+1:])
	if len(fields) < 20 || fields[0] == "Z" || fields[0] == "X" {
		return ProcessIdentity{}, fmt.Errorf("process is absent or exited")
	}
	ticks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || ticks == 0 || boot == "" {
		return ProcessIdentity{}, fmt.Errorf("invalid process identity")
	}
	return ProcessIdentity{BootID: boot, StartTicks: ticks}, nil
}

func processMatches(state State) bool {
	if state.PID <= 0 || state.Process.StartTicks == 0 {
		return false
	}
	identity, err := readProcessIdentity(state.PID)
	return err == nil && identity == state.Process
}

func signalProcess(state State, signal syscall.Signal) error {
	if state.PID <= 0 || state.Process.StartTicks == 0 {
		return fmt.Errorf("Unit has no verified process identity")
	}
	fd, _, errno := syscall.Syscall(434, uintptr(state.PID), 0, 0) // pidfd_open, amd64 and arm64
	if errno != 0 {
		return errno
	}
	defer syscall.Close(int(fd))
	if !processMatches(state) {
		return fmt.Errorf("Unit process identity changed; refusing signal")
	}
	_, _, errno = syscall.Syscall6(424, fd, uintptr(signal), 0, 0, 0, 0) // pidfd_send_signal
	if errno != 0 {
		return errno
	}
	return nil
}
