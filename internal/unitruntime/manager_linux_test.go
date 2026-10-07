package unitruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStartupRequiresSuccessfulExecMarker(t *testing.T) {
	for _, payload := range []string{"READY\n", "", "ERROR: denied\n", "READY\nERROR: exec failed\n"} {
		t.Run(fmt.Sprintf("%q", payload), func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			_, _ = w.Write([]byte(payload))
			_ = w.Close()
			err = awaitStartup(r, time.Second)
			if (err == nil) != (payload == "READY\n") {
				t.Fatalf("payload %q: %v", payload, err)
			}
		})
	}
}

func TestStartupTimeout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if err := awaitStartup(r, 20*time.Millisecond); err == nil {
		t.Fatal("startup must time out")
	}
}

func TestLegacySpecGetsHardenedDefaults(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sources", "busybox", "rootfs"), 0755); err != nil {
		t.Fatal(err)
	}
	m := NewManager(Config{StateRoot: root})
	if _, err := m.Create(Spec{ID: "legacy", Source: "busybox", Command: []string{"/bin/sh"}}); err != nil {
		t.Fatal(err)
	}
	s, _, err := m.Inspect("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if s.Security.NoNewPrivileges == nil || !*s.Security.NoNewPrivileges || len(s.Security.Capabilities) != 0 {
		t.Fatalf("unsafe persisted policy: %+v", s.Security)
	}
}

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

func TestResolverRejectsSourceEtcSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	path := filepath.Join(outside, "resolv.conf")
	if err := os.WriteFile(path, []byte("host resolver"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "etc")); err != nil {
		t.Fatal(err)
	}
	if err := prepareFabricResolver(root, "10.249.0.1/24"); err == nil {
		t.Fatal("host resolver escape accepted")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "host resolver" {
		t.Fatal("host resolver modified")
	}
}
