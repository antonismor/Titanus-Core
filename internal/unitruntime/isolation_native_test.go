package unitruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/security"
	"github.com/antonismor/Titanus-Core/internal/source"
)

func TestNativeIsolation(t *testing.T) {
	if os.Getenv("TITANUS_ISOLATION_KERNEL_TEST") != "1" {
		t.Skip("requires native root kernel integration")
	}
	if os.Geteuid() != 0 {
		t.Fatal("requires root")
	}
	root := t.TempDir()
	cg := filepath.Join("/sys/fs/cgroup", fmt.Sprintf("titanus-isolation-%d", os.Getpid()))
	m := NewManager(Config{StateRoot: root, CgroupRoot: cg, InitBinary: os.Getenv("TITANUS_INIT_BINARY")})
	t.Cleanup(func() { _ = os.Remove(cg) })
	src := filepath.Join(root, "input")
	if e := source.CopyMapped("/tmp/titanus-rootfs", src, 0, 65536); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(src, "forbidden"), []byte("untouched"), 0666); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(filepath.Join(src, "forbidden"), 0666); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("/", filepath.Join(src, "alias")); e != nil {
		t.Fatal(e)
	}
	if e := source.NewManager(root).ImportDirectory("probe", src); e != nil {
		t.Fatal(e)
	}
	var previous int
	for _, id := range []string{"isolation-a", "isolation-b"} {
		t.Run(id, func(t *testing.T) {
			disks := disk.NewManager(root)
			if _, e := disks.Create(disk.Spec{Name: id, Provider: disk.ProviderLocal, SizeBytes: 64 << 20}); e != nil {
				t.Fatal(e)
			}
			spec := Spec{ID: id, Source: "probe", Command: []string{"/bin/isolation-probe"}, Mounts: []disk.Mount{{Disk: id, Target: "/data"}}}
			if _, e := m.Create(spec); e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { _, _ = m.Stop(id, time.Second); _ = m.Delete(id) })
			mapping, e := m.allocateMapping(id)
			if e != nil {
				t.Fatal(e)
			}
			if mapping.Base == previous {
				t.Fatal("different Units share host identity")
			}
			previous = mapping.Base
			data := filepath.Join(root, "disks", id, "data")
			if e := os.Chown(data, mapping.Base, mapping.Base); e != nil {
				t.Fatal(e)
			}
			for _, node := range []struct {
				name   string
				mode   uint32
				device int
			}{{"kmsg", syscall.S_IFCHR | 0666, 0x10b}, {"block", syscall.S_IFBLK | 0666, 0x700}} {
				if e := syscall.Mknod(filepath.Join(data, node.name), node.mode, node.device); e != nil {
					t.Fatal(e)
				}
				if e := os.Chmod(filepath.Join(data, node.name), 0666); e != nil {
					t.Fatal(e)
				}
			}
			state, e := m.Start(id)
			if e != nil {
				t.Fatal(e)
			}
			if state.Status != StatusActive {
				t.Fatal(state)
			}
			hostStatus, e := os.ReadFile(fmt.Sprintf("/proc/%d/status", state.PID))
			if e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(string(hostStatus), fmt.Sprintf("Uid:\t%d\t%d", mapping.Base, mapping.Base)) {
				t.Fatalf("host credentials: %s", hostStatus)
			}
			hostMap, e := os.ReadFile(fmt.Sprintf("/proc/%d/uid_map", state.PID))
			if e != nil {
				t.Fatal(e)
			}
			fields := strings.Fields(string(hostMap))
			if len(fields) != 3 || fields[1] != fmt.Sprint(mapping.Base) {
				t.Fatalf("mapping: %s", hostMap)
			}
			deadline := time.Now().Add(10 * time.Second)
			for {
				logs, _ := os.ReadFile(filepath.Join(m.unitDir(id), "logs", "unit.log"))
				if strings.Contains(string(logs), "TITANUS_ISOLATION_OK") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("kernel probe failed: %s", logs)
				}
				time.Sleep(50 * time.Millisecond)
			}
			_, _ = m.Stop(id, time.Second)
			st, e := os.Stat(filepath.Join(data, "allowed"))
			if e != nil {
				t.Fatal(e)
			}
			if st.Sys().(*syscall.Stat_t).Uid != uint32(mapping.Base) {
				t.Fatal("persisted writer uses host root")
			}
			// Stable map and enforcement after a fresh manager adoption/restart.
			m = NewManager(m.cfg)
			again, e := m.Start(id)
			if e != nil || again.UserMapping != mapping {
				t.Fatalf("restart: %+v %v", again, e)
			}
			_, _ = m.Stop(id, time.Second)
		})
	}
	t.Run("LSM-setup-fails-before-exec", func(t *testing.T) {
		spec := Spec{ID: "lsm-fail", Source: "probe", Command: []string{"/bin/sh", "-c", "echo BAD_EXEC"}, Security: security.Policy{LSMWritePaths: []string{"/missing-policy-directory"}}}
		if _, e := m.Create(spec); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = m.Delete(spec.ID) })
		state, e := m.Start(spec.ID)
		if e == nil || state.Status != StatusFailed || state.PID != 0 {
			t.Fatalf("LSM failure accepted: %+v %v", state, e)
		}
		logs, _ := os.ReadFile(filepath.Join(m.unitDir(spec.ID), "logs", "unit.log"))
		if strings.Contains(string(logs), "BAD_EXEC") {
			t.Fatal("workload exec before LSM enforcement")
		}
		if _, e := os.Stat(m.cgroupDir(spec.ID)); !os.IsNotExist(e) {
			t.Fatal("failed Unit leaked cgroup")
		}
	})
}
