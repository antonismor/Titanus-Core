package security

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKernelSecurity(t *testing.T) {
	if os.Getenv("TITANUS_SECURITY_KERNEL_TEST") != "1" {
		t.Skip("run explicitly as root in privileged CI with TITANUS_SECURITY_KERNEL_TEST=1")
	}
	if os.Geteuid() != 0 {
		t.Fatal("kernel security checks require root")
	}
	dir := t.TempDir()
	// Non-root execution of the setuid helper must be able to traverse this dir.
	if err := os.Chmod(filepath.Dir(dir), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "probe")
	if output, err := exec.Command("go", "build", "-o", probe, "./testprobe").CombinedOutput(); err != nil {
		t.Fatalf("build probe: %v\n%s", err, output)
	}
	for _, mask := range []string{"0", "400"} {
		t.Run(mask, func(t *testing.T) {
			output, err := exec.Command(probe, "apply", mask).CombinedOutput()
			if err != nil || !strings.Contains(string(output), "TITANUS_SECURITY_OK") {
				t.Fatalf("kernel enforcement: %v\n%s", err, output)
			}
		})
	}
	t.Run("setuid-cannot-elevate", func(t *testing.T) {
		code := filepath.Join(dir, "setuid.c")
		helper := filepath.Join(dir, "setuid-helper")
		if err := os.WriteFile(code, []byte("#include <unistd.h>\nint main(void) { return getuid() == 1000 && geteuid() == 1000 ? 0 : 1; }\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command("gcc", "-static", "-o", helper, code).CombinedOutput(); err != nil {
			t.Fatalf("build setuid helper: %v\n%s", err, output)
		}
		if err := os.Chmod(helper, 0755|os.ModeSetuid); err != nil {
			t.Fatal(err)
		}
		if output, err := exec.Command(probe, "nonroot", "0", helper).CombinedOutput(); err != nil {
			t.Fatalf("setuid elevation prevention: %v\n%s", err, output)
		}
	})
}
