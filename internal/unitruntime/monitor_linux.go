package unitruntime

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

type MonitorConfig struct {
	Args     []string `json:"args"`
	ExitPath string   `json:"exit_path"`
	RunID    string   `json:"run_id"`
}

type ExitRecord struct {
	RunID    string    `json:"run_id"`
	Code     int       `json:"code"`
	Signal   int       `json:"signal,omitempty"`
	ExitedAt time.Time `json:"exited_at"`
}

// RunMonitor stays in the host namespaces and waits for namespace init. It
// holds no daemon lifetime dependency and never passes host state files into a
// Unit. Only this host process can publish an exit record.
func RunMonitor(encoded string) error {
	var cfg MonitorConfig
	if err := json.Unmarshal([]byte(encoded), &cfg); err != nil {
		return err
	}
	if cfg.RunID == "" || !filepath.IsAbs(cfg.ExitPath) || len(cfg.Args) == 0 {
		return fmt.Errorf("invalid monitor configuration")
	}
	// Mark every inherited monitor descriptor CLOEXEC before spawning init.
	// ExtraFiles clears CLOEXEC only on the child's explicitly passed 3/4.
	for _, fd := range []int{3, 4, 5} {
		syscall.CloseOnExec(fd)
	}
	ready := os.NewFile(3, "startup-barrier")
	status := os.NewFile(4, "startup-status")
	control := os.NewFile(5, "process-identity")
	defer ready.Close()
	defer status.Close()
	defer control.Close()
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(binary, cfg.Args...)
	cmd.Env = os.Environ()
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{ready, status}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUTS | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWIPC | syscall.CLONE_NEWNET, Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(control, "%d\n", cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	_ = control.Close()
	_ = ready.Close()
	_ = status.Close()
	err = cmd.Wait()
	record := ExitRecord{RunID: cfg.RunID, ExitedAt: time.Now().UTC()}
	if cmd.ProcessState != nil {
		wait := cmd.ProcessState.Sys().(syscall.WaitStatus)
		if wait.Signaled() {
			record.Signal = int(wait.Signal())
			record.Code = 128 + record.Signal
		} else {
			record.Code = wait.ExitStatus()
		}
	} else {
		record.Code = 255
	}
	if saveErr := saveJSON(cfg.ExitPath, record, 0600); saveErr != nil {
		return saveErr
	}
	// A workload's failure is recorded data, not a monitor execution failure.
	if _, ok := err.(*exec.ExitError); err != nil && !ok {
		return err
	}
	return nil
}
