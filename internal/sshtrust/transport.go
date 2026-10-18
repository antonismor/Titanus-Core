// Package sshtrust establishes explicit, Realm-scoped OpenSSH trust without
// changing sshd policy, replacing existing keys, or persisting passwords.
package sshtrust

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

type Runner interface {
	Run(context.Context, []byte, []byte, string, ...string) ([]byte, error)
}

// ExecRunner sends a bootstrap password to sshpass through an inherited pipe.
// It is never included in argv, the environment, a file, or command output.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, input, password []byte, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if len(password) > 0 {
		args = append([]string{"-d", "3", name}, args...)
		name = "sshpass"
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewReader(input)
	var out bytes.Buffer
	cmd.Stdout = &out
	// Authentication and remote output may contain secrets; never return stderr.
	cmd.Stderr = io.Discard
	if len(password) > 0 {
		rd, wr, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		defer rd.Close()
		cmd.ExtraFiles = []*os.File{rd}
		if err = cmd.Start(); err != nil {
			wr.Close()
			return nil, fmt.Errorf("cannot start SSH bootstrap transport")
		}
		_, writeErr := wr.Write(append(append([]byte(nil), password...), '\n'))
		wr.Close()
		err = cmd.Wait()
		if writeErr != nil || err != nil {
			return nil, fmt.Errorf("SSH bootstrap command failed (check authentication, host trust and prerequisites)")
		}
	} else if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s command failed", name)
	}
	return out.Bytes(), nil
}

func CheckTools() error {
	for _, name := range []string{"ssh", "ssh-keyscan", "ssh-keygen", "sshpass"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("SSH trust bootstrap requires %s; install openssh-client and sshpass", name)
		}
	}
	return nil
}
