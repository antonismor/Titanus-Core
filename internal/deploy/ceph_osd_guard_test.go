package deploy

import (
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOSDCreationRefusesExistingSignaturesWithoutChangingBytes(t *testing.T) {
	for _, name := range []string{"wipefs", "mkfs.ext4"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s unavailable", name)
		}
	}
	for _, populated := range []bool{false, true} {
		name := "empty"
		if populated {
			name = "existing-filesystem"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			image := filepath.Join(dir, "selected device.img")
			f, err := os.Create(image)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.Truncate(32 << 20); err != nil {
				t.Fatal(err)
			}
			f.Close()
			if populated {
				if out, err := exec.Command("mkfs.ext4", "-F", "-q", image).CombinedOutput(); err != nil {
					t.Fatalf("fixture: %v: %s", err, out)
				}
			}
			before, err := os.ReadFile(image)
			if err != nil {
				t.Fatal(err)
			}
			bin := filepath.Join(dir, "bin")
			if err = os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(dir, "ceph-volume.calls")
			stub := "#!/bin/sh\n[ \"$1 $2\" = \"lvm create\" ] || exit 99\nprintf '%s\\n' \"$*\" >> \"$TITANUS_OSD_CREATE_LOG\"\n"
			if err = os.WriteFile(filepath.Join(bin, "ceph-volume"), []byte(stub), 0700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-ec", "SUDO=; "+cephOSDCreateCommand(image))
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TITANUS_OSD_CREATE_LOG="+log)
			out, runErr := cmd.CombinedOutput()
			calls, readErr := os.ReadFile(log)
			if populated {
				if runErr == nil || !os.IsNotExist(readErr) || !strings.Contains(string(out), "refusing to initialize") {
					t.Fatalf("existing filesystem admitted: %v %s %q", runErr, out, calls)
				}
			} else if runErr != nil || readErr != nil || !strings.Contains(string(calls), "lvm create --data "+image) {
				t.Fatalf("empty device was not admitted to native creation: %v %s %q", runErr, out, calls)
			}
			after, err := os.ReadFile(image)
			if err != nil {
				t.Fatal(err)
			}
			if sha256.Sum256(before) != sha256.Sum256(after) {
				t.Fatal("read-only signature decision changed selected device bytes")
			}
		})
	}
}

func TestOSDCreationFailsClosedWhenSignatureProbeFails(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "ceph-volume.calls")
	for name, body := range map[string]string{
		"wipefs":      "#!/bin/sh\necho 'signature probe failed' >&2\nexit 37\n",
		"ceph-volume": "#!/bin/sh\necho CALLED >> \"$TITANUS_OSD_CREATE_LOG\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("bash", "-ec", "SUDO=; "+cephOSDCreateCommand("/dev/fixture"))
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TITANUS_OSD_CREATE_LOG="+log)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "signature probe failed") {
		t.Fatalf("probe error accepted: %v %s", err, out)
	}
	if _, err = os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("native Ceph creation ran after failed probe")
	}
}
