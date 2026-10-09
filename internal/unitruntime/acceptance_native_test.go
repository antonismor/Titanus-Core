//go:build linux

package unitruntime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/version"
)

func TestNativeAcceptanceLifecycle(t *testing.T) {
	if os.Getenv("TITANUS_ACCEPTANCE_NATIVE_TEST") != "1" {
		t.Skip("requires native privileged disposable runner")
	}
	cycles, e := strconv.Atoi(os.Getenv("TITANUS_ACCEPTANCE_CYCLES"))
	if e != nil || cycles < 3 || cycles > 32 || os.Geteuid() != 0 {
		t.Fatal("root and 3..32 cycles required")
	}
	root := t.TempDir()
	if e = source.NewManager(root).ImportDirectory("acceptance", "/tmp/titanus-rootfs"); e != nil {
		t.Fatal(e)
	}
	cgroup := fmt.Sprintf("/sys/fs/cgroup/titanus-acceptance-%d", os.Getpid())
	m := NewManager(Config{StateRoot: root, CgroupRoot: cgroup, InitBinary: os.Getenv("TITANUS_INIT_BINARY")})
	t.Cleanup(func() {
		items, _ := m.List()
		for _, u := range items {
			_, _ = m.Stop(u.ID, time.Second)
			_ = m.Delete(u.ID)
		}
		_ = os.Remove(cgroup)
	})
	start := time.Now()
	latencies := []float64{}
	runs := map[string]bool{}
	for i := 0; i < cycles; i++ {
		id := fmt.Sprintf("bench-%d", i)
		canary := fmt.Sprintf("INTEGRITY_%d", i)
		if _, e = m.Create(Spec{ID: id, Source: "acceptance", MemoryBytes: 32 << 20, CPUPercent: 25, PidsMax: 16, Command: []string{"/bin/sh", "-ec", "echo " + canary + "; sleep 60"}}); e != nil {
			t.Fatal(e)
		}
		begin := time.Now()
		state, e := m.Start(id)
		if e != nil || state.Status != StatusActive || state.PID <= 0 || state.RunID == "" || runs[state.RunID] {
			t.Fatal("start/identity failure", e, state.Status)
		}
		latencies = append(latencies, time.Since(begin).Seconds())
		runs[state.RunID] = true
		deadline := time.Now().Add(5 * time.Second)
		for {
			p, err := m.LogsPath(id)
			b, readErr := os.ReadFile(p)
			if err == nil && readErr == nil && strings.Count(string(b), canary+"\n") == 1 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("actual workload log integrity absent")
			}
			time.Sleep(20 * time.Millisecond)
		}
		usage, e := m.Usage(id)
		if e != nil || usage.RunID != state.RunID || usage.MemoryBytes == 0 || usage.Pids == 0 {
			t.Fatal("actual fresh cgroup sample missing", e)
		}
		stopped, e := m.Stop(id, time.Second)
		if e != nil || stopped.PID != 0 || stopped.RunID != state.RunID {
			t.Fatal("stop did not confirm original run", e)
		}
		if e = m.Delete(id); e != nil {
			t.Fatal(e)
		}
		if _, e = os.Stat(filepath.Join(cgroup, id)); !os.IsNotExist(e) {
			t.Fatal("cgroup leaked after delete", e)
		}
	}
	items, e := m.List()
	if e != nil || len(items) != 0 {
		t.Fatal("fixture cleanup incomplete", e)
	}
	sort.Float64s(latencies)
	evidence, _ := json.Marshal(map[string]any{"test": "native_lifecycle", "architecture": version.Info().Arch,
		"revision": os.Getenv("TITANUS_ACCEPTANCE_REVISION"), "topology": "1 disposable native kernel",
		"cycles": cycles, "duration_seconds": time.Since(start).Seconds(), "start_p50_seconds": latencies[len(latencies)/2],
		"start_p95_seconds": latencies[(len(latencies)-1)*95/100], "start_max_seconds": latencies[len(latencies)-1],
		"distinct_runs": len(runs), "log_and_cgroup_integrity_verified": true, "units_and_cgroups_cleaned": true,
		"independent_host_acceptance": false})
	t.Log("TITANUS_ACCEPTANCE_EVIDENCE " + string(evidence))
}
