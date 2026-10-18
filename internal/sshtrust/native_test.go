package sshtrust

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/model"
)

// This opt-in test runs only on disposable GitHub runners. Each SSH server has
// a separate host key, UID-0 alias/home and loopback IP. It never contacts lab IPs.
func TestNativeSSHPasswordBootstrapAndFullMesh(t *testing.T) {
	if os.Getenv("TITANUS_SSH_TEST") != "1" {
		t.Skip("opt-in isolated SSH integration")
	}
	if os.Getuid() != 0 {
		t.Fatal("isolated SSH integration requires root")
	}
	if err := CheckTools(); err != nil {
		t.Fatal(err)
	}
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll("/run/sshd", 0755)
	root := privateDir(t)
	// sshd checks every ancestor; Go's test parent may be world-readable, never writable.
	ctx := context.Background()
	nodes := []model.NodeSpec{}
	password := []byte("Titanus-test-only-" + strconv.Itoa(os.Getpid()))
	run := func(name string, args ...string) {
		t.Helper()
		cmd := exec.Command(name, args...)
		if out, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("%s: %v: %s", name, e, out)
		}
	}
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("tssh%d_%d", os.Getpid(), i)
		home := filepath.Join(root, name)
		if e := os.Mkdir(home, 0700); e != nil {
			t.Fatal(e)
		}
		// User entries exist solely in the ephemeral runner; no real account password is changed.
		run("useradd", "-o", "-u", "0", "-M", "-d", home, "-s", "/bin/sh", name)
		t.Cleanup(func() { exec.Command("userdel", "-f", name).Run() })
		cmd := exec.Command("chpasswd")
		cmd.Stdin = strings.NewReader(name + ":" + string(password) + "\n")
		if e := cmd.Run(); e != nil {
			t.Fatal("set isolated SSH account password:", e)
		}
		key := filepath.Join(root, name+"-hostkey")
		run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
		ip := fmt.Sprintf("127.0.0.%d", i+2)
		listener, e := net.Listen("tcp", ip+":0")
		if e != nil {
			t.Fatal(e)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		listener.Close()
		config := filepath.Join(root, name+"-sshd.conf")
		text := fmt.Sprintf("Port %d\nListenAddress %s\nHostKey %s\nPidFile %s\nPasswordAuthentication yes\nKbdInteractiveAuthentication no\nPermitRootLogin yes\nUsePAM no\nStrictModes yes\nAllowUsers %s\nAuthorizedKeysFile %s/.ssh/authorized_keys\nLogLevel ERROR\n", port, ip, key, filepath.Join(root, name+".pid"), name, home)
		if e = os.WriteFile(config, []byte(text), 0600); e != nil {
			t.Fatal(e)
		}
		logFile, e := os.Create(filepath.Join(root, name+".log"))
		if e != nil {
			t.Fatal(e)
		}
		process := exec.Command(sshd, "-D", "-e", "-f", config)
		process.Stderr = logFile
		if e = process.Start(); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { process.Process.Kill(); process.Wait(); logFile.Close() })
		ready := false
		for j := 0; j < 100; j++ {
			c, e := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), 50*time.Millisecond)
			if e == nil {
				c.Close()
				ready = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !ready {
			raw, _ := os.ReadFile(logFile.Name())
			t.Fatalf("test sshd did not start: %s", raw)
		}
		nodes = append(nodes, model.NodeSpec{Name: name, ManagementIP: ip, SSHUser: name, SSHPort: port})
	}
	bootstrap := Bootstrap{Runner: ExecRunner{}, Directory: filepath.Join(root, "installer")}
	hosts, err := bootstrap.Discover(ctx, nodes)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		report, e := bootstrap.Apply(ctx, "SSH-CI", hosts, password)
		if e != nil {
			t.Fatal(e)
		}
		if report.Verified != 6 || report.Expected != 6 {
			t.Fatalf("incomplete real mesh: %+v", report)
		}
	}
	// Wrong password must stop, and never be included in returned errors.
	_, err = bootstrap.Apply(ctx, "SSH-CI", hosts, []byte("wrong-sensitive-password"))
	if err == nil || strings.Contains(err.Error(), "wrong-sensitive-password") {
		t.Fatalf("unsafe authentication failure: %v", err)
	}
}
