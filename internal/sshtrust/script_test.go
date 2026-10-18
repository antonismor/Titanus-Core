package sshtrust

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runRemoteScript(t *testing.T, home, id, installer string, peers []string, known string) ([]byte, error) {
	t.Helper()
	payload, _ := json.Marshal(map[string]any{"id": id, "installer": installer, "peers": peers, "known": known})
	cmd := exec.Command("python3", "-c", remotePython, string(payload))
	cmd.Env = append(os.Environ(), "HOME="+home)
	return cmd.CombinedOutput()
}
func TestRealKeyGenerationResumeAndPreservation(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("remote script intentionally requires root")
	}
	for _, tool := range []string{"python3", "ssh-keygen"} {
		if _, e := exec.LookPath(tool); e != nil {
			t.Skip(tool + " missing")
		}
	}
	home := privateDir(t)
	id := RealmID("LAB")
	installer := testKey(100)
	os.Mkdir(filepath.Join(home, ".ssh"), 0700)
	auth := filepath.Join(home, ".ssh", "authorized_keys")
	existing := "# existing operator\n" + testKey(101) + " legacy\n"
	os.WriteFile(auth, []byte(existing), 0644)
	first, err := runRemoteScript(t, home, id, installer, nil, "")
	if err != nil {
		t.Fatalf("%v: %s", err, first)
	}
	keyPath := filepath.Join(home, ".ssh", "titanus", id, "id_ed25519")
	before, _ := os.ReadFile(keyPath)
	for i := 0; i < 2; i++ {
		result, e := runRemoteScript(t, home, id, installer, []string{testKey(102)}, "192.0.2.1 "+testKey(103)+"\n")
		if e != nil {
			t.Fatalf("%v: %s", e, result)
		}
		if string(result) != string(first) {
			t.Fatal("public key rotated on resume")
		}
	}
	after, _ := os.ReadFile(keyPath)
	if !bytes.Equal(before, after) {
		t.Fatal("private key replaced")
	}
	content, _ := os.ReadFile(auth)
	if !strings.HasPrefix(string(content), existing) || strings.Count(string(content), testKey(102)) != 1 || strings.Count(string(content), installer) != 1 {
		t.Fatal("existing keys lost or new keys duplicated")
	}
	for _, path := range []string{auth, keyPath, filepath.Join(filepath.Dir(keyPath), "known_hosts")} {
		i, e := os.Stat(path)
		if e != nil || i.Mode().Perm() != 0600 {
			t.Fatalf("unsafe permissions: %s", path)
		}
	}
	// A symlink must fail without modifying its target.
	os.Remove(auth)
	target := filepath.Join(home, "untouched")
	os.WriteFile(target, []byte("keep"), 0600)
	os.Symlink(target, auth)
	if _, e := runRemoteScript(t, home, id, installer, nil, ""); e == nil {
		t.Fatal("symlink accepted")
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != "keep" {
		t.Fatal("symlink target changed")
	}
}
