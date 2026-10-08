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
	if e != nil || usage.CPUUsec == 0 || usage.MemoryBytes == 0 {
		t.Fatalf("no actual cgroup evidence: %+v %v", usage, e)
	}
	t.Log("TITANUS_NATIVE_TASK_SECRET_AUTOSCALE_OK")
}
