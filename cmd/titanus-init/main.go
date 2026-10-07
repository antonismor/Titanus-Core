package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: titanus-init <command> [args...]")
		os.Exit(64)
	}

	cmd := exec.Command(os.Args[1], os.Args[2:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "titanus-init:", err)
		os.Exit(1)
	}

	signals := make(chan os.Signal, 16)
	signal.Notify(signals)
	go func() {
		for sig := range signals {
			if s, ok := sig.(syscall.Signal); ok {
				_ = syscall.Kill(-cmd.Process.Pid, s)
			}
		}
	}()

	err := cmd.Wait()
	signal.Stop(signals)
	close(signals)

	if err == nil {
		return
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			if status.Signaled() {
				os.Exit(128 + int(status.Signal()))
			}
			os.Exit(status.ExitStatus())
		}
	}
	fmt.Fprintln(os.Stderr, "titanus-init:", err)
	os.Exit(1)
}
