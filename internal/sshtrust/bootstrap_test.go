package sshtrust

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/model"
)

func testKey(i byte) string {
	b := append([]byte{0, 0, 0, 11}, []byte("ssh-ed25519")...)
	b = append(b, 0, 0, 0, 32)
	for j := 0; j < 32; j++ {
		b = append(b, i)
	}
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(b)
}

type fakeRunner struct {
	hosts           []Host
	pairs           []string
	passwords       int
	failPair, prior string
	authCalls       int
	commands        []string
}

func (f *fakeRunner) Run(_ context.Context, _ []byte, password []byte, name string, args ...string) ([]byte, error) {
	if name == "ssh-keyscan" {
		return []byte(args[len(args)-1] + " " + testKey(99) + "\n"), nil
	}
	if name == "sh" {
		return []byte(f.prior), nil
	}
	if name == "ssh-keygen" {
		if args[0] == "-q" {
			return nil, os.WriteFile(args[len(args)-1], []byte("fixture"), 0600)
		}
		return []byte(testKey(100)), nil
	}
	if name != "ssh" {
		return nil, fmt.Errorf("unexpected command")
	}
	f.authCalls++
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "StrictHostKeyChecking=yes") || !strings.Contains(joined, "IdentityAgent=none") {
		return nil, fmt.Errorf("unsafe SSH options")
	}
	if len(password) > 0 {
		f.passwords++
		if strings.Contains(joined, string(password)) {
			return nil, fmt.Errorf("password in argv")
		}
	}
	source := args[len(args)-2]
	command := args[len(args)-1]
	f.commands = append(f.commands, command)
	if strings.HasPrefix(command, "python3 ") {
		for i, h := range f.hosts {
			if source == h.Node.SSHUser+"@"+h.Node.ManagementIP {
				return []byte(testKey(byte(i + 1))), nil
			}
		}
	}
	pair := source + " " + command
	f.pairs = append(f.pairs, pair)
	if f.failPair != "" && strings.Contains(pair, f.failPair) {
		return nil, fmt.Errorf("blocked direction")
	}
	return []byte("TITANUS_MESH_OK"), nil
}
func inventory(n int) []Host {
	h := make([]Host, n)
	for i := range h {
		key := testKey(99)
		h[i] = Host{Node: model.NodeSpec{Name: fmt.Sprintf("node%d", i), ManagementIP: fmt.Sprintf("192.0.2.%d", i+1), SSHUser: "root", SSHPort: 22}, Key: key, Fingerprint: fingerprint(key)}
	}
	return h
}
func TestBootstrapVerifiesEveryDirectedPairAndPreservesInstallerKey(t *testing.T) {
	for _, count := range []int{1, 2, 9} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			hosts := inventory(count)
			f := &fakeRunner{hosts: hosts}
			dir := privateDir(t)
			b := Bootstrap{Runner: f, Directory: dir}
			for run := 0; run < 2; run++ {
				report, err := b.Apply(context.Background(), "LAB", hosts, []byte("sensitive-password"))
				if err != nil {
					t.Fatal(err)
				}
				if report.Expected != count*(count-1) || report.Verified != report.Expected {
					t.Fatal(report)
				}
			}
			if len(f.pairs) != 2*count*(count-1) || f.passwords != 2*count {
				t.Fatal("mesh skipped or password used after key installation")
			}
			key, err := os.ReadFile(filepath.Join(dir, "id_ed25519"))
			if err != nil || string(key) != "fixture" {
				t.Fatal("installer key replaced")
			}
			// Only public keys and public pins are sent to the remote script.
			for _, cmd := range f.commands {
				if strings.Contains(cmd, "sensitive-password") {
					t.Fatal("credential in remote command")
				}
			}
		})
	}
}
func TestBootstrapFailsOnOneMissingDirection(t *testing.T) {
	hosts := inventory(3)
	f := &fakeRunner{hosts: hosts, failPair: "root@192.0.2.3 ssh"}
	report, err := (Bootstrap{Runner: f, Directory: privateDir(t)}).Apply(context.Background(), "LAB", hosts, []byte("secret"))
	if err == nil || report.Verified == report.Expected || !strings.Contains(err.Error(), "full-mesh") {
		t.Fatalf("%+v %v", report, err)
	}
}
func TestChangedHostKeyFailsBeforeAuthentication(t *testing.T) {
	f := &fakeRunner{prior: "192.0.2.1 " + testKey(98)}
	dir := privateDir(t)
	pin := filepath.Join(dir, "known_hosts")
	os.WriteFile(pin, []byte(f.prior), 0600)
	_, err := (Bootstrap{Runner: f, Directory: dir}).Discover(context.Background(), []model.NodeSpec{inventory(1)[0].Node})
	if err == nil || !strings.Contains(err.Error(), "host key changed") || f.authCalls != 0 {
		t.Fatal(err)
	}
}
func TestInvalidInventoriesNeverAuthenticate(t *testing.T) {
	for _, hosts := range [][]Host{inventory(0), inventory(10), {inventory(1)[0], inventory(1)[0]}} {
		f := &fakeRunner{}
		_, err := (Bootstrap{Runner: f, Directory: privateDir(t)}).Apply(context.Background(), "LAB", hosts, []byte("secret"))
		if err == nil || f.authCalls != 0 {
			t.Fatal("invalid inventory authenticated")
		}
	}
}
func TestUnsafeLocalFilesRefused(t *testing.T) {
	hosts := inventory(1)
	dir := privateDir(t)
	target := filepath.Join(privateDir(t), "other")
	os.WriteFile(target, []byte("unchanged"), 0600)
	os.Symlink(target, filepath.Join(dir, "known_hosts"))
	f := &fakeRunner{hosts: hosts}
	_, err := (Bootstrap{Runner: f, Directory: dir}).Apply(context.Background(), "LAB", hosts, []byte("secret"))
	raw, _ := os.ReadFile(target)
	if err == nil || string(raw) != "unchanged" || f.authCalls != 0 {
		t.Fatal("followed unsafe symlink")
	}
}

func privateDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	return d
}
