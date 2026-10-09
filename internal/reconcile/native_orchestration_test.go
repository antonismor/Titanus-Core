//go:build linux

package reconcile

import (
	"bytes"
	"encoding/json"
	"github.com/antonismor/Titanus-Core/internal/lease"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type nativeOrchestration struct{ *nativeRollout }

func (n *nativeOrchestration) InspectUnit(_, id string) (unitruntime.Spec, unitruntime.State, error) {
	return n.m.Inspect(id)
}
func (n *nativeOrchestration) UnitUsage(_, id string) (unitruntime.Usage, error) {
	return n.m.Usage(id)
}
func TestNativeOrchestration(t *testing.T) {
	if os.Getenv("TITANUS_NATIVE_ORCHESTRATION_TEST") != "1" {
		t.Skip("requires native kernel, init and prepared Source")
	}
	root := t.TempDir()
	sources := source.NewManager(root)
	if e := sources.ImportDirectory("app", "/tmp/titanus-rootfs"); e != nil {
		t.Fatal(e)
	}
	keys := &secrets.Keyring{Active: "one", Keys: map[string][]byte{"one": bytes.Repeat([]byte{3}, 32)}}
	raw, _ := json.Marshal(keys)
	keyPath := filepath.Join(root, "private-keys.json")
	if e := os.WriteFile(keyPath, raw, 0600); e != nil {
		t.Fatal(e)
	}
	cfg := unitruntime.Config{StateRoot: root, CgroupRoot: "/sys/fs/cgroup/titanus-orchestration", InitBinary: os.Getenv("TITANUS_INIT_BINARY"), SecretKeyring: keyPath, SecretRealm: "LAB"}
	m := unitruntime.NewManager(cfg)
	leases := lease.NewManager(m)
	defer leases.Close()
	defer func() {
		items, _ := m.List()
		for _, u := range items {
			m.Stop(u.ID, time.Second)
			m.Delete(u.ID)
		}
	}()
	s, e := realm.Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.UpsertNode(realm.Node{ID: "native", Address: "local", State: realm.NodeReady, Capabilities: []model.Capability{model.CapabilityExecution}, Resources: realm.Resources{MemoryBytes: 4 << 30, CPUMilliCapacity: 4000}}); e != nil {
		t.Fatal(e)
	}
	upgradeM6(t, s)
	record, e := keys.Encrypt("LAB", "password", 1, []byte("NATIVE_SECRET_CANARY"))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.PutSecret(record); e != nil {
		t.Fatal(e)
	}
	task := realm.Task{Name: "exit-seven", Template: realm.UnitTemplate{Source: "app", Command: []string{"/bin/sh", "-ec", `test "${#PASSWORD}" -eq 20; echo TASK_SECRET_PRESENT; exit 7`}, MemoryBytes: 64 << 20, CPUPercent: 50, PidsMax: 64, Secrets: []secrets.Ref{{Name: "password", Version: 1, Environment: "PASSWORD"}}}}
	// NATIVE_SECRET_CANARY has 20 bytes. The value itself is never in a spec.
	if e = s.CreateTask(task); e != nil {
		t.Fatal(e)
	}
	n := &nativeOrchestration{&nativeRollout{m: m, leases: leases, t: t}}
	c := &Controller{Store: s, Nodes: n, Sources: sources}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if e = c.Once(); e != nil {
			t.Fatal(e)
		}
		if s.Snapshot().Tasks[task.Name].Terminal() {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	result := s.Snapshot().Tasks[task.Name]
	if result.Phase != realm.TaskFailed || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("real exit code lost: %+v", result)
	}
	specData, _ := os.ReadFile(filepath.Join(root, "units", result.UnitID, "spec.json"))
	stateData, _ := os.ReadFile(filepath.Join(root, "realm", "state.json"))
	for _, data := range [][]byte{specData, stateData} {
		if bytes.Contains(data, []byte("NATIVE_SECRET_CANARY")) {
			t.Fatal("plaintext secret persisted")
		}
	}
	logs, e := m.ReadLogs(result.UnitID)
	if e != nil || !strings.Contains(string(logs), "TASK_SECRET_PRESENT") || strings.Contains(string(logs), "NATIVE_SECRET_CANARY") {
		t.Fatalf("secret injection failed or leaked: %s %v", logs, e)
	}
	// Daemon/controller reconstruction must retain the terminal Task and RunID.
	reopened, e := realm.Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	c.Store = reopened
	for i := 0; i < 3; i++ {
		if e = c.Once(); e != nil {
			t.Fatal(e)
		}
	}
	_, u, e := m.Inspect(result.UnitID)
	if e != nil || u.RunID != result.RunID {
		t.Fatal("completed Task executed again")
	}
	// Missing local decryption key must fail before workload exec.
	missing := task
	missing.Name = "missing-key"
	if e = reopened.CreateTask(missing); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(keyPath, keyPath+".held"); e != nil {
		t.Fatal(e)
	}
	c.Once()
	_, failed, e := m.Inspect("task-missing-key")
	if e != nil || failed.Status != unitruntime.StatusFailed || failed.PID != 0 {
		t.Fatal("missing encryption key allowed exec")
	}
	os.Rename(keyPath+".held", keyPath)

	// Re-encrypt retained versions without changing the workload's pinned ref.
	if e = secrets.Provision(keyPath, "two", bytes.Repeat([]byte{7}, 32), false); e != nil {
		t.Fatal(e)
	}
	if e = secrets.Provision(keyPath, "two", nil, true); e != nil {
		t.Fatal(e)
	}
	rotated, e := secrets.Load(keyPath)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = reopened.RotateSecrets("native-execution-rotation-001", "two", reopened.Snapshot().Revision, rotated); e != nil {
		t.Fatal(e)
	}
	j := realm.TaskSchedule{Name: "retry-proof", Template: task.Template, DueAt: time.Now().Add(2 * time.Second), MaxRuns: 1, MaxAttempts: 2, RetrySeconds: 1}
	if e = reopened.CreateTaskSchedule(j); e != nil {
		t.Fatal(e)
	}
	c.Once()
	if reopened.Snapshot().TaskSchedules[j.Name].ActiveTask != "" {
		t.Fatal("native schedule dispatched early")
	}
	deadline = time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if e = c.Once(); e != nil {
			t.Fatal(e)
		}
		state := reopened.Snapshot()
		if state.TaskSchedules[j.Name].Done {
			break
		}
		// Reconstruct the controller between attempts; identities are state-owned.
		if state.TaskSchedules[j.Name].Attempt == 2 {
			c = &Controller{Store: reopened, Nodes: n, Sources: sources}
		}
		time.Sleep(150 * time.Millisecond)
	}
	state := reopened.Snapshot()
	if !state.TaskSchedules[j.Name].Done {
		t.Fatal("native acknowledged retry did not complete")
	}
	units := map[string]bool{}
	runs := map[string]bool{}
	executions := map[string]bool{}
	for name, attempt := range state.Tasks {
		if strings.HasPrefix(name, "sch-") {
			if attempt.Phase != realm.TaskFailed || attempt.ExitCode == nil || *attempt.ExitCode != 7 {
				t.Fatal("retry lost confirmed exit")
			}
			units[attempt.UnitID] = true
			runs[attempt.RunID] = true
			executions[attempt.ExecutionID] = true
		}
	}
	if len(units) != 2 || len(runs) != 2 || len(executions) != 2 {
		t.Fatal("retry reused native Unit/run/execution identity")
	}
	for id := range units {
		spec, _, e := m.Inspect(id)
		if e != nil || len(spec.SecretBindings) != 1 || spec.SecretBindings[0].Record.KeyID != "two" {
			t.Fatal("native retry did not use reencrypted pinned version", e)
		}
	}
	t.Log("TITANUS_NATIVE_SCHEDULE_RETRY_ROTATION_OK")
	// Actual cgroup CPU deltas, not allocated resources, drive the Fleet scale.
	fleet := realm.Fleet{Name: "burn", Instances: 1, MaxSurge: 1, Template: realm.UnitTemplate{Source: "app", Command: []string{"/bin/sh", "-ec", "while :; do :; done"}, MemoryBytes: 64 << 20, CPUPercent: 50, PidsMax: 64}}
	if e = reopened.PutFleet(fleet); e != nil {
		t.Fatal(e)
	}
	if e = reopened.PutAutoscaler(realm.Autoscaler{Fleet: "burn", Min: 1, Max: 2, TargetCPU: 60, CooldownSeconds: 10, DownscaleSeconds: 30}); e != nil {
		t.Fatal(e)
	}
	deadline = time.Now().Add(35 * time.Second)
	for time.Now().Before(deadline) {
		if e = c.Once(); e != nil {
			t.Fatal(e)
		}
		if reopened.Snapshot().Fleets["burn"].Instances == 2 {
			break
		}
		time.Sleep(1100 * time.Millisecond)
	}
	if reopened.Snapshot().Fleets["burn"].Instances != 2 {
		t.Fatal("actual workload CPU failed to scale")
	}
	usage, e := m.Usage("burn-001-g1")
	if e != nil || usage.CPUUsec == 0 || usage.MemoryBytes == 0 || usage.Pids == 0 {
		t.Fatalf("no actual cgroup evidence: %+v %v", usage, e)
	}
	// The same actual samples supply additional memory/PID pressure. Use a
	// separate disk-free Fleet so CPU policy results cannot cause this scale.
	memoryFleet := fleet
	memoryFleet.Name = "memory-pressure"
	memoryFleet.Template.Command = []string{"/bin/sh", "-ec", "i=0; while [ $i -lt 24 ]; do sleep 120 & i=$((i+1)); done; wait"}
	memoryFleet.Template.PidsMax = 64
	if e = reopened.PutFleet(memoryFleet); e != nil {
		t.Fatal(e)
	}
	if e = reopened.PutAutoscaler(realm.Autoscaler{Fleet: memoryFleet.Name, Min: 1, Max: 2, TargetPids: 20, CooldownSeconds: 10, DownscaleSeconds: 30}); e != nil {
		t.Fatal(e)
	}
	deadline = time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if e = c.Once(); e != nil {
			t.Fatal(e)
		}
		if reopened.Snapshot().Fleets[memoryFleet.Name].Instances == 2 {
			break
		}
		time.Sleep(1100 * time.Millisecond)
	}
	if reopened.Snapshot().Fleets[memoryFleet.Name].Instances != 2 {
		t.Fatal("actual PID pressure failed to autoscale")
	}
	measured, e := m.Usage("memory-pressure-001-g1")
	if e != nil || measured.Pids < 20 || measured.MemoryBytes == 0 {
		t.Fatal("additional metrics were not actual cgroup samples", e)
	}
	memFleet := fleet
	memFleet.Name = "memory-only"
	memFleet.Template.MemoryBytes = 64 << 20
	memFleet.Template.Command = []string{"/bin/sh", "-ec", `awk 'BEGIN {for(i=0;i<24000;i++) a[i]=sprintf("%01024d",i); print "MEMORY_HELD"; system("sleep 120")}'`}
	if e = reopened.PutFleet(memFleet); e != nil {
		t.Fatal(e)
	}
	if e = reopened.PutAutoscaler(realm.Autoscaler{Fleet: memFleet.Name, Min: 1, Max: 2, TargetMemory: 20, CooldownSeconds: 10, DownscaleSeconds: 30}); e != nil {
		t.Fatal(e)
	}
	deadline = time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		if e = c.Once(); e != nil {
			t.Fatal(e)
		}
		if reopened.Snapshot().Fleets[memFleet.Name].Instances == 2 {
			break
		}
		time.Sleep(1100 * time.Millisecond)
	}
	if reopened.Snapshot().Fleets[memFleet.Name].Instances != 2 {
		t.Fatal("actual memory pressure failed to autoscale")
	}
	memUsage, e := m.Usage("memory-only-001-g1")
	if e != nil || memUsage.MemoryBytes < 16<<20 {
		t.Fatal("memory scaling lacked actual allocation evidence", e)
	}
	t.Log("TITANUS_NATIVE_ADDITIONAL_MEMORY_PIDS_METRICS_OK", measured.MemoryBytes, measured.Pids)
	t.Log("TITANUS_NATIVE_TASK_SECRET_AUTOSCALE_OK")
}
