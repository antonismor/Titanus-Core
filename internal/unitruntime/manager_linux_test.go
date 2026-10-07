package unitruntime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPrepareFabricResolverReplacesSourceResolverSafely(t *testing.T) {
	rootfs := t.TempDir()
	etc := filepath.Join(rootfs, "etc")
	if err := os.MkdirAll(etc, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/run/systemd/resolve/stub-resolv.conf", filepath.Join(etc, "resolv.conf")); err != nil {
		t.Fatal(err)
	}
	if err := prepareFabricResolver(rootfs, "10.240.1.1/24"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(etc, "resolv.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("Titanus resolver must replace rootfs resolver symlinks with a local regular file")
	}
	data, err := os.ReadFile(filepath.Join(etc, "resolv.conf"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, wanted := range []string{"search titanus", "nameserver 10.240.1.1", "options ndots:1"} {
		if !strings.Contains(text, wanted) {
			t.Fatalf("resolver missing %q:\n%s", wanted, text)
		}
	}
}

func TestPrepareFabricResolverRejectsIPv6Gateway(t *testing.T) {
	if err := prepareFabricResolver(t.TempDir(), "fd00::1/64"); err == nil {
		t.Fatal("expected IPv6 gateway to be rejected by Fabric v1 resolver")
	}
}

func TestWaitForExecStatusTreatsCleanEOFAsSuccessfulExec(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	defer read.Close()
	if err := waitForExecStatus(read, time.Second); err != nil {
		t.Fatalf("clean exec-status EOF rejected: %v", err)
	}
}

func TestWaitForExecStatusSurfacesPreExecFailure(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := write.WriteString("apply Unit Security Profile: operation not permitted\n"); err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	defer read.Close()
	err = waitForExecStatus(read, time.Second)
	if err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("expected child startup error, got %v", err)
	}
}
