package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/model"
)

// Execute the actual generated monitor bootstrap shell. Root SSH has no sudo
// prefix, so a bare "-u ceph" is not an executable and must never be used.
func TestCephMonitorBootstrapRootAndFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "mkfs-failure"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			scripts := map[string]string{
				"id":        "#!/bin/sh\necho 0\n",
				"install":   "#!/bin/sh\nexit 0\n",
				"runuser":   "#!/bin/sh\n[ \"$1\" = -u ] && [ \"$2\" = ceph ] && [ \"$3\" = -- ] || exit 91\nshift 3\nexec \"$@\"\n",
				"ceph-mon":  "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TITANUS_MON_ARGS\"\nexit \"${TITANUS_MON_EXIT:-0}\"\n",
				"systemctl": "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$TITANUS_SYSTEMCTL_ARGS\"\n",
			}
			for n, s := range scripts {
				if err := os.WriteFile(filepath.Join(dir, n), []byte(s), 0755); err != nil {
					t.Fatal(err)
				}
			}
			monArgs := filepath.Join(dir, "mon-args")
			serviceArgs := filepath.Join(dir, "service-args")
			status := "0"
			if fail {
				status = "23"
			}
			cmd := exec.Command("bash", "-c", cephMonitorCommand(model.NodeSpec{Name: "titanus-bootstrap-regression-" + name}))
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TITANUS_MON_ARGS="+monArgs, "TITANUS_MON_EXIT="+status, "TITANUS_SYSTEMCTL_ARGS="+serviceArgs)
			out, err := cmd.CombinedOutput()
			args, readErr := os.ReadFile(monArgs)
			if readErr != nil || !strings.Contains(string(args), "--mkfs") {
				t.Fatalf("monitor mkfs was not executed through runuser: %s %v", out, readErr)
			}
			if fail {
				if err == nil {
					t.Fatal("mkfs failure was hidden")
				}
				if _, e := os.Stat(serviceArgs); !os.IsNotExist(e) {
					t.Fatal("service started after failed mkfs")
				}
			} else {
				if err != nil {
					t.Fatalf("bootstrap failed: %v %s", err, out)
				}
				args, e := os.ReadFile(serviceArgs)
				if e != nil || !strings.Contains(string(args), "enable\n--now\nceph-mon@") {
					t.Fatalf("monitor service was not started: %s %v", args, e)
				}
			}
		})
	}
}
