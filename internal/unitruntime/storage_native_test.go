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
		if _, err := m.Create(Spec{ID: id, Source: "app", Mounts: []disk.Mount{{Disk: "data", Target: "/data"}}, Command: []string{"/bin/sh", "-ec", "if [ -e /proc/self/fd/6 ]; then exit 97; fi; echo proof > /data/proof; while [ ! -e /data/flood ]; do sleep 1; done; /bin/busybox head -c 3145728 /dev/zero; echo LOG_BOUND_OK; sleep 120"}}); err != nil {
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
	if err = os.WriteFile(filepath.Join(root, "disks", "data", "data", "flood"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, e := m.ReadLogs("writer")
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(data), "LOG_BOUND_OK") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("independent logger stopped after monitor SIGKILL")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, name := range []string{"unit.log", "unit.log.1"} {
		info, e := os.Stat(filepath.Join(m.unitDir("writer"), "logs", name))
		if e != nil || info.Size() > UnitLogLimit {
			t.Fatal("log byte bound failed", name, e)
		}
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

func TestNativeLogSinkFailure(t *testing.T) {
	if os.Getenv("TITANUS_ISOLATION_KERNEL_TEST") != "1" {
		t.Skip("native root log sink")
	}
	m := NewManager(Config{StateRoot: t.TempDir(), InitBinary: os.Getenv("TITANUS_INIT_BINARY")})
	if err := os.MkdirAll(filepath.Join(m.unitDir("sink-failure"), "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	out, err := m.openLogSink("sink-failure")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	path := filepath.Join(m.unitDir("sink-failure"), "logs", "unit.log")
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err = out.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	data := []byte(strings.Repeat("output-secret", 200000))
	if n, e := out.Write(data); e != nil || n != len(data) {
		t.Fatal("failed sink stopped draining workload", n, e)
	}
	out.Close()
	if !m.LogSinkFailed("sink-failure") {
		t.Fatal("sink storage failure not reported")
	}
}
