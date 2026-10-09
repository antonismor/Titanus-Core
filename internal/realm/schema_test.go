package realm

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/version"
)

func TestSchemaTransitionPreservesUnknownExecutionAndRejectsRollback(t *testing.T) {
	root := t.TempDir()
	s, e := Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.CreateTask(Task{Name: "job", Template: UnitTemplate{Source: "app", Command: []string{"/bin/sh", "-c", "exit 7"}}}); e != nil {
		t.Fatal(e)
	}
	task := s.Snapshot().Tasks["job"]
	task.Phase = TaskUnknown
	task.NodeID = "worker"
	task.RunID = "uncertain-original-run"
	if e = s.UpdateTask(task); e != nil {
		t.Fatal(e)
	}
	if e = s.UpsertNode(Node{ID: "worker"}); e != nil {
		t.Fatal(e)
	}
	before := s.Snapshot()
	caps := version.Compatible()
	observations := map[string]version.Capabilities{"controller": caps, "worker": caps}
	if _, e = s.TransitionSchema("schema-transition-lab-001", before.Revision, observations, []string{"controller", "worker"}); e == nil {
		t.Fatal("unadvertised node migrated")
	}
	if e = s.UpsertNode(Node{ID: "worker", Compatibility: &caps}); e != nil {
		t.Fatal(e)
	}
	before = s.Snapshot()
	m, e := s.TransitionSchema("schema-transition-lab-001", before.Revision, observations, []string{"controller", "worker"})
	if e != nil {
		t.Fatal(e)
	}
	after := s.Snapshot()
	migrated := after.Tasks["job"]
	if migrated.Phase != TaskUnknown || migrated.RunID != task.RunID || migrated.UnitID != task.UnitID || migrated.ExecutionID == "" {
		t.Fatal("migration replayed/lost unknown execution")
	}
	if e = ValidateSchemaChange(before, after); e != nil {
		t.Fatal(e)
	}
	if e = ValidateSchemaChange(after, before); e == nil {
		t.Fatal("data rollback admitted")
	}
	if _, e = s.TransitionSchema(m.ID, before.Revision, observations, []string{"controller", "worker"}); e != nil || s.Snapshot().Revision != after.Revision {
		t.Fatal("lost-response retry duplicated migration", e)
	}
	if e = s.UpsertNode(Node{ID: "old-worker"}); e == nil {
		t.Fatal("legacy node admitted schema 1")
	}
	if e = offline.CheckStartup(root); e != nil {
		t.Fatal(e)
	}
	reopened, e := Open(root, "LAB")
	if e != nil || reopened.Snapshot().SchemaVersion != 1 {
		t.Fatal("migration did not survive restart", e)
	}
	raw, e := os.ReadFile(filepath.Join(root, "realm", "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	var fixture State
	if e = json.Unmarshal(raw, &fixture); e != nil {
		t.Fatal(e)
	}
	fixture.SchemaVersion = 2
	if ValidateSchema(fixture) == nil {
		t.Fatal("future schema admitted")
	}
	fixture = cloneState(after)
	changed := fixture.Tasks["job"]
	changed.ExecutionID = taskIdentity("LAB", "job", "different")
	fixture.Tasks["job"] = changed
	if ValidateSchemaChange(after, fixture) == nil {
		t.Fatal("execution identity mutable")
	}
}

type rejectedSchemaConsensus struct{ state State }

func (c *rejectedSchemaConsensus) Snapshot() State    { return cloneState(c.state) }
func (c *rejectedSchemaConsensus) CheckLeader() error { return nil }
func (c *rejectedSchemaConsensus) Apply(State) error {
	return fmt.Errorf("injected interrupted quorum transaction")
}
func TestInterruptedSchemaCommitLeavesFloorAndNoLocalMigration(t *testing.T) {
	root := t.TempDir()
	s, e := Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	before := s.Snapshot()
	c := &rejectedSchemaConsensus{before}
	if e = s.EnableConsensus(c); e != nil {
		t.Fatal(e)
	}
	if _, e = s.TransitionSchema("schema-interrupted-001", before.Revision, map[string]version.Capabilities{"controller": version.Compatible()}, []string{"controller"}); e == nil {
		t.Fatal("failed commit succeeded")
	}
	if s.Snapshot().SchemaVersion != 0 || s.Snapshot().Revision != before.Revision {
		t.Fatal("uncommitted schema leaked")
	}
	if e = offline.CheckSchemaFloor(root); e != nil {
		t.Fatal("prepared floor lost", e)
	}
	if e = offline.PrepareSchemaFloor(root, offline.NewSchemaFloor("LAB", "another-migration-001", 1)); e == nil {
		t.Fatal("different migration overwrote floor")
	}
}
