package unitruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/source"
)

func TestNativeDiskOwnership(t *testing.T) {
	if os.Getenv("TITANUS_ISOLATION_KERNEL_TEST") != "1" {
		t.Skip("native root runtime integration")
	}
	root := t.TempDir()
	cg := filepath.Join("/sys/fs/cgroup", fmt.Sprintf("titanus-disk-%d", os.Getpid()))
	m := NewManager(Config{StateRoot: root, CgroupRoot: cg, InitBinary: os.Getenv("TITANUS_INIT_BINARY")})
	if err := source.NewManager(root).ImportDirectory("app", "/tmp/titanus-rootfs"); err != nil {
		t.Fatal(err)
	}
	d := disk.NewManager(root)
	if _, err := d.Create(disk.Spec{Name: "data", SizeBytes: 64 << 20}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"writer", "competitor"} {
		if _, err := m.Create(Spec{ID: id, Source: "app", Mounts: []disk.Mount{{Disk: "data", Target: "/data"}}, Command: []string{"/bin/sh", "-ec", "echo proof > /data/proof; sleep 120"}}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = m.Stop(id, time.Second); _ = m.Delete(id) })
	}
	mapping, err := m.allocateMapping("writer")
	if err != nil {
		t.Fatal(err)
	}
	if err = d.ProvisionOwnership("data", mapping.Base, mapping.Base); err != nil {
		t.Fatal(err)
	}
	state, err := m.Start("writer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start("competitor"); err == nil {
		t.Fatal("started simultaneous writer")
	}
	if _, err = d.Snapshot("data", "live"); err == nil {
		t.Fatal("snapshotted live writer")
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", state.PID))
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Fields(string(stat)[strings.LastIndex(string(stat), ")")+2:])
	monitor, err := strconv.Atoi(fields[1])
	if err != nil || monitor <= 1 {
		t.Fatal("invalid host monitor")
	}
	if err = syscall.Kill(monitor, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// PID 1 is alive after the host monitor dies and must still own the data.
	time.Sleep(100 * time.Millisecond)
	if !processMatches(state) {
		t.Fatal("monitor death killed Unit unexpectedly")
	}
	if _, err = d.Acquire("data", "stale", "new"); err == nil {
		t.Fatal("monitor SIGKILL freed live attachment")
	}
	recovered := NewManager(m.cfg)
	if err = recovered.Recover(); err != nil {
		t.Fatal(err)
	}
	if _, err = recovered.Stop("writer", time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Snapshot("data", "stopped"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Restore("data", "stopped", "restored"); err != nil {
		t.Fatal(err)
	}
	proof, err := os.ReadFile(filepath.Join(root, "disks", "restored", "data", "proof"))
	if err != nil || strings.TrimSpace(string(proof)) != "proof" {
		t.Fatal("runtime restore lost data", err)
	}
	t.Cleanup(func() { os.Remove(cg) })
}
