package realm

import (
	"bytes"
	"encoding/json"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTaskDurabilityAndSecretReferences(t *testing.T) {
	root := t.TempDir()
	s, e := Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	k := &secrets.Keyring{Active: "one", Keys: map[string][]byte{"one": bytes.Repeat([]byte{1}, 32)}}
	r, e := k.Encrypt("LAB", "db", 1, []byte("UNIQUE_SECRET_CANARY"))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.PutSecret(r); e != nil {
		t.Fatal(e)
	}
	template := UnitTemplate{Source: "app", Command: []string{"/bin/sh", "-c", "exit 7"}, Secrets: []secrets.Ref{{Name: "db", Version: 1, Environment: "DB_PASSWORD"}}}
	if e = s.CreateTask(Task{Name: "job", Template: template}); e != nil {
		t.Fatal(e)
	}
	if e = s.CreateTask(Task{Name: "job", Template: template}); e == nil {
		t.Fatal("duplicate execution accepted")
	}
	task := s.Snapshot().Tasks["job"]
	task.Phase = TaskDispatched
	task.NodeID = "node"
	if e = s.UpdateTask(task); e != nil {
		t.Fatal(e)
	}
	reopened, e := Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	if reopened.Snapshot().Tasks["job"].Phase != TaskDispatched {
		t.Fatal("dispatch lost")
	}
	if e = s.DeleteSecret("db"); e == nil {
		t.Fatal("referenced secret deleted")
	}
	raw, _ := os.ReadFile(filepath.Join(root, "realm", "state.json"))
	if bytes.Contains(raw, []byte("UNIQUE_SECRET_CANARY")) {
		t.Fatal("plaintext in state")
	}
	task.Phase = TaskUnknown
	if e = s.UpdateTask(task); e != nil {
		t.Fatal(e)
	}
	if e = s.CancelTask("job"); e != nil {
		t.Fatal(e)
	}
	task.Phase = TaskCancelled
	if e = s.UpdateTask(task); e != nil {
		t.Fatal(e)
	}
	if e = s.UpdateTask(task); e == nil {
		t.Fatal("terminal Task changed")
	}
	var out State
	json.Unmarshal(raw, &out)
	if out.Tasks["job"].Template.Health.Restart != "never" {
		t.Fatal("Task can restart")
	}
}
func TestAutoscaleCooldownDurableLowWindowAndBounds(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root, "LAB")
	f := Fleet{Name: "web", Instances: 4, MinimumAvailable: 1, Template: UnitTemplate{Source: "app", Command: []string{"app"}, CPUPercent: 50}}
	if e := s.PutFleet(f); e != nil {
		t.Fatal(e)
	}
	if e := s.PutAutoscaler(Autoscaler{Fleet: "web", Min: 1, Max: 8, TargetCPU: 60, CooldownSeconds: 10, DownscaleSeconds: 30}); e != nil {
		t.Fatal(e)
	}
	now := time.Now().Add(11 * time.Second)
	if e := s.ApplyAutoscale("web", 1, now); e != nil {
		t.Fatal(e)
	}
	if s.Snapshot().Fleets["web"].Instances != 4 {
		t.Fatal("unstable downscale")
	}
	s, _ = Open(root, "LAB")
	if e := s.ApplyAutoscale("web", 1, now.Add(31*time.Second)); e != nil {
		t.Fatal(e)
	}
	if s.Snapshot().Fleets["web"].Instances != 1 {
		t.Fatal("downscale window lost")
	}
	s.ApplyAutoscale("web", 100, now.Add(32*time.Second))
	if s.Snapshot().Fleets["web"].Instances != 1 {
		t.Fatal("cooldown ignored")
	}
	s.ApplyAutoscale("web", 100, now.Add(42*time.Second))
	if s.Snapshot().Fleets["web"].Instances != 8 {
		t.Fatal("max not enforced")
	}
	if s.Snapshot().Fleets["web"].Generation != 1 {
		t.Fatal("scale rolled template")
	}
}
