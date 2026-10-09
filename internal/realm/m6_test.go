package realm

import (
	"bytes"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"github.com/antonismor/Titanus-Core/internal/version"
	"testing"
	"time"
)

func schemaTwoStore(t *testing.T) *Store {
	t.Helper()
	s, e := Open(t.TempDir(), "LAB")
	if e != nil {
		t.Fatal(e)
	}
	for target := 1; target <= 2; target++ {
		if _, e = s.TransitionSchemaTo("m6-schema-transition-00"+string(rune('0'+target)), s.Snapshot().Revision, target, map[string]version.Capabilities{"controller": version.Compatible()}, []string{"controller"}); e != nil {
			t.Fatal(e)
		}
	}
	return s
}
func TestRotationPreservesVersionsAndRejectsInterruptedOrIncompleteKeys(t *testing.T) {
	s := schemaTwoStore(t)
	old := &secrets.Keyring{Active: "one", Keys: map[string][]byte{"one": bytes.Repeat([]byte{1}, 32)}}
	r, e := old.Encrypt("LAB", "password", 1, []byte("SECRET_ROTATION_PROOF"))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.PutSecret(r); e != nil {
		t.Fatal(e)
	}
	if e = s.CreateTask(Task{Name: "retained", Template: UnitTemplate{Source: "app", Command: []string{"app"}, Secrets: []secrets.Ref{{Name: "password", Version: 1, Environment: "PASSWORD"}}}}); e != nil {
		t.Fatal(e)
	}
	before := s.Snapshot()
	keys := &secrets.Keyring{Active: "two", Keys: map[string][]byte{"two": bytes.Repeat([]byte{2}, 32)}}
	if _, e = s.RotateSecrets("rotation-proof-001", "two", before.Revision, keys); e == nil {
		t.Fatal("missing decrypt key admitted")
	}
	if s.Snapshot().Revision != before.Revision {
		t.Fatal("failed rotation partially committed")
	}
	keys.Keys["one"] = old.Keys["one"]
	marker, e := s.RotateSecrets("rotation-proof-001", "two", before.Revision, keys)
	if e != nil {
		t.Fatal(e)
	}
	after := s.Snapshot()
	v := after.Secrets["password"][0]
	plain, e := keys.Decrypt(v)
	if e != nil || string(plain) != "SECRET_ROTATION_PROOF" || v.Version != 1 || v.KeyID != "two" || after.Tasks["retained"].Template.Secrets[0].Version != 1 {
		t.Fatal("reencryption broke pinned references", e)
	}
	if _, e = old.Decrypt(v); e == nil {
		t.Fatal("rotation did not replace encryption")
	}
	if _, e = keys.Decrypt(r); e != nil {
		t.Fatal("retained old specs no longer decrypt")
	}
	again, e := s.RotateSecrets(marker.ID, marker.Target, before.Revision, keys)
	if e != nil || again != marker || s.Snapshot().Revision != after.Revision {
		t.Fatal("uncertain response repeated rotation", e)
	}
}
func TestScheduleAtomicAttemptIdentityAndUnknownHold(t *testing.T) {
	s := schemaTwoStore(t)
	now := time.Now().UTC()
	j := TaskSchedule{Name: "nightly", Template: UnitTemplate{Source: "app", Command: []string{"app"}}, DueAt: now.Add(time.Second), MaxRuns: 1, MaxAttempts: 2, RetrySeconds: 1}
	if e := s.CreateTaskSchedule(j); e != nil {
		t.Fatal(e)
	}
	if e := s.DispatchScheduledTask(j.Name, now); e != nil {
		t.Fatal(e)
	}
	if len(s.Snapshot().Tasks) != 0 {
		t.Fatal("scheduled before due time")
	}
	if e := s.DispatchScheduledTask(j.Name, now.Add(2*time.Second)); e != nil {
		t.Fatal(e)
	}
	state := s.Snapshot()
	first := state.Tasks[state.TaskSchedules[j.Name].ActiveTask]
	if first.ExecutionID == "" {
		t.Fatal("attempt lacks immutable identity")
	}
	first.Phase = TaskUnknown
	first.RunID = "uncertain"
	first.NodeID = "original"
	if e := s.UpdateTask(first); e != nil {
		t.Fatal(e)
	}
	first = s.Snapshot().Tasks[first.Name]
	if e := s.FinishScheduledTask(j.Name, first, now.Add(time.Minute)); e == nil {
		t.Fatal("UNKNOWN execution replayed")
	}
	s.DispatchScheduledTask(j.Name, now.Add(time.Hour))
	if len(s.Snapshot().Tasks) != 1 {
		t.Fatal("unknown attempt silently retried")
	}
	code := 7
	first.Phase = TaskFailed
	first.ExitCode = &code
	if e := s.UpdateTask(first); e != nil {
		t.Fatal(e)
	}
	first = s.Snapshot().Tasks[first.Name]
	if e := s.FinishScheduledTask(j.Name, first, now.Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	if e := s.DispatchScheduledTask(j.Name, now.Add(2*time.Minute)); e != nil {
		t.Fatal(e)
	}
	state = s.Snapshot()
	second := state.Tasks[state.TaskSchedules[j.Name].ActiveTask]
	if second.ExecutionID == first.ExecutionID || second.UnitID == first.UnitID || second.Name == first.Name || len(state.Tasks) != 2 {
		t.Fatal("retry reused original immutable execution")
	}
}
