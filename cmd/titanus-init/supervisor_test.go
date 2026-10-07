package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSupervisorHelper(t *testing.T) {
	if os.Getenv("TITANUS_SUPERVISOR_TEST") != "1" {
		return
	}
	code, err := supervise([]string{"/bin/sh", "-c", os.Getenv("TITANUS_SUPERVISOR_SCRIPT")}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(255)
	}
	os.Exit(code)
}

func testSupervisor(t *testing.T, script string) (*exec.Cmd, *bufio.Scanner) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSupervisorHelper$")
	cmd.Env = append(os.Environ(), "TITANUS_SUPERVISOR_TEST=1", "TITANUS_SUPERVISOR_SCRIPT="+script)
	cmd.Stderr = os.Stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, bufio.NewScanner(pipe)
}

func TestSupervisorReapsOrphansAndPreservesExitCode(t *testing.T) {
	data, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Fields(string(data))[0] != fmt.Sprint(os.Getpid()) {
		t.Skip("workspace /proc belongs to a different PID namespace; orphan reaping verified on native CI")
	}
	cmd, output := testSupervisor(t, `(sleep 0.1 & echo $!); sleep 1; exit 7`)
	if !output.Scan() {
		t.Fatal("missing orphan PID")
	}
	pid, err := strconv.Atoi(output.Text())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(800 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); os.IsNotExist(err) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d", pid)); !os.IsNotExist(err) {
		t.Fatal("adopted orphan was not reaped while main child was alive")
	}
	err = cmd.Wait()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 7 {
		t.Fatalf("main exit code lost: %v", err)
	}
}

func TestSupervisorForwardsTerminationToWorkloadGroup(t *testing.T) {
	cmd, output := testSupervisor(t, `trap 'exit 23' TERM; echo READY; while :; do sleep 1; done`)
	if !output.Scan() || strings.TrimSpace(output.Text()) != "READY" {
		t.Fatal("workload not ready")
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 23 {
			t.Fatalf("termination not forwarded: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not terminate")
	}
}
