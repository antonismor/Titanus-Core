package unitruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessIdentityHandlesCommAndRejectsZombies(t *testing.T) {
	fields := []string{"S", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "0", "123"}
	stat := "1 (strange ) process name) " + strings.Join(fields, " ")
	identity, err := parseProcessIdentity("boot", stat)
	if err != nil || identity.StartTicks != 123 {
		t.Fatalf("comm parsing: %+v %v", identity, err)
	}
	if _, err := parseProcessIdentity("boot", strings.Replace(stat, ") S ", ") Z ", 1)); err == nil {
		t.Fatal("zombie considered live")
	}
}

func TestPidfdRejectsReusedPIDAndOldBoot(t *testing.T) {
	identity, err := readProcessIdentity(os.Getpid())
	if err != nil {
		data, readErr := os.ReadFile("/proc/self/stat")
		if readErr == nil && strings.Fields(string(data))[0] != fmt.Sprint(os.Getpid()) {
			t.Skip("workspace /proc belongs to a different PID namespace; pidfd verified on native CI")
		}
		t.Fatal(err)
	}
	state := State{PID: os.Getpid(), Process: identity}
	if err := signalProcess(state, 0); err != nil {
		t.Fatal(err)
	}
	state.Process.StartTicks++
	if processMatches(state) || signalProcess(state, 0) == nil {
		t.Fatal("PID reuse accepted")
	}
	state.Process = identity
	state.Process.BootID = "previous-boot"
	if processMatches(state) || signalProcess(state, 0) == nil {
		t.Fatal("old boot accepted")
	}
}

func TestRecoveryUsesRunSpecificExitRecord(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sources", "source", "rootfs"), 0755); err != nil {
		t.Fatal(err)
	}
	m := NewManager(Config{StateRoot: root, CgroupRoot: filepath.Join(root, "cgroup")})
	state, err := m.Create(Spec{ID: "unit", Source: "source", Command: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := parseProcessIdentity("boot", string(data))
	if err != nil {
		t.Fatal(err)
	}
	identity.BootID = "old-boot"
	state.PID = os.Getpid()
	state.Process = identity
	state.Status = StatusActive
	state.RunID = "current"
	if err := m.saveState(state); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(m.unitDir(state.ID), "exit.json")
	if err := saveJSON(path, ExitRecord{RunID: "old", Code: 0, ExitedAt: time.Now().UTC()}, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.Recover(); err != nil {
		t.Fatal(err)
	}
	_, state, err = m.Inspect("unit")
	if err != nil || state.Status != StatusFailed || state.ExitCode != nil || state.PID != 0 {
		t.Fatalf("stale record accepted: %+v %v", state, err)
	}
	if err := saveJSON(path, ExitRecord{RunID: "current", Code: 7, ExitedAt: time.Now().UTC()}, 0600); err != nil {
		t.Fatal(err)
	}
	_, state, err = m.Inspect("unit")
	if err != nil || state.ExitCode == nil || *state.ExitCode != 7 || state.Status != StatusFailed {
		t.Fatalf("exit record not restored: %+v %v", state, err)
	}
	if _, err := m.Stop("unit", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// Stop must never signal this unrelated live process from the old boot.
	if err := syscall.Kill(os.Getpid(), 0); err != nil {
		t.Fatal(fmt.Errorf("unrelated PID was signaled: %w", err))
	}
}
