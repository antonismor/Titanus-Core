package remote

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/model"
)

type Result struct {
	Stdout   string
	Stderr   string
	Duration time.Duration
}

type SSHExecutor struct {
	ConnectTimeout time.Duration
	CommandTimeout time.Duration
}

func NewSSHExecutor() *SSHExecutor {
	return &SSHExecutor{
		ConnectTimeout: 5 * time.Second,
		CommandTimeout: 20 * time.Second,
	}
}

func (e *SSHExecutor) CopyFile(node model.NodeSpec, localPath, remotePath string) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", localPath)
	}
	timeout := e.CommandTimeout
	if timeout < 2*time.Minute {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	connectSeconds := int(e.ConnectTimeout.Seconds())
	if connectSeconds < 1 {
		connectSeconds = 5
	}
	target := node.SSHUser + "@" + node.ManagementIP + ":" + remotePath
	args := []string{
		"-q",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=" + strconv.Itoa(connectSeconds),
		"-P", strconv.Itoa(node.SSHPort),
		localPath,
		target,
	}
	cmd := exec.CommandContext(ctx, "scp", args...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("SCP timed out after %s", timeout)
	}
	if err != nil {
		return fmt.Errorf("scp %s to %s: %w: %s", localPath, node.Name, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (e *SSHExecutor) Run(node model.NodeSpec, command string) (Result, error) {
	started := time.Now()
	timeout := e.CommandTimeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	connectSeconds := int(e.ConnectTimeout.Seconds())
	if connectSeconds < 1 {
		connectSeconds = 5
	}

	target := node.SSHUser + "@" + node.ManagementIP
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "ConnectTimeout=" + strconv.Itoa(connectSeconds),
		"-p", strconv.Itoa(node.SSHPort),
		target,
		command,
	}

	cmd := exec.CommandContext(ctx, "ssh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	result := Result{
		Stdout:   strings.TrimSpace(stdout.String()),
		Stderr:   strings.TrimSpace(stderr.String()),
		Duration: time.Since(started),
	}

	if ctx.Err() == context.DeadlineExceeded {
		return result, fmt.Errorf("SSH command timed out after %s", timeout)
	}
	if err != nil {
		if result.Stderr != "" {
			return result, fmt.Errorf("%w: %s", err, result.Stderr)
		}
		return result, err
	}
	return result, nil
}
