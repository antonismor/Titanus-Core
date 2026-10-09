package realm

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"regexp"
	"time"

	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/version"
)

var migrationID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{15,127}$`)
var executionID = regexp.MustCompile(`^[a-f0-9]{64}$`)

type SchemaMigration struct {
	ID          string    `json:"id"`
	From        int       `json:"from"`
	To          int       `json:"to"`
	Revision    uint64    `json:"revision"`
	CommittedAt time.Time `json:"committed_at"`
}

func taskIdentity(realm, name, salt string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(realm+"\x00"+name+"\x00"+salt)))
}
func ValidateSchema(s State) error {
	if s.SchemaVersion < 0 || s.SchemaVersion > version.MaxSchema {
		return fmt.Errorf("unsupported Realm schema %d", s.SchemaVersion)
	}
	if s.SchemaVersion < 2 && (len(s.TaskSchedules) > 0 || len(s.SecretRotations) > 0) {
		return fmt.Errorf("schema-two orchestration state in older schema")
	}
	if s.SchemaVersion < 2 {
		for _, a := range s.Autoscalers {
			if a.TargetMemory != 0 || a.TargetPids != 0 {
				return fmt.Errorf("new metrics require schema two")
			}
		}
	}
	if s.SchemaVersion == 0 {
		if len(s.SchemaMigrations) != 0 {
			return fmt.Errorf("legacy state cannot contain schema migrations")
		}
		for _, t := range s.Tasks {
			if t.ExecutionID != "" {
				return fmt.Errorf("legacy Task contains versioned execution identity")
			}
		}
		return nil
	}
	if len(s.SchemaMigrations) != s.SchemaVersion {
		return fmt.Errorf("schema requires complete committed migration chain")
	}
	previousRevision := uint64(0)
	for i, m := range s.SchemaMigrations {
		if !migrationID.MatchString(m.ID) || m.From != i || m.To != i+1 || m.Revision <= previousRevision || m.Revision > s.Revision || m.CommittedAt.IsZero() {
			return fmt.Errorf("invalid schema migration marker")
		}
		previousRevision = m.Revision
	}
	seen := map[string]bool{}
	for name, t := range s.Tasks {
		if name != t.Name || !executionID.MatchString(t.ExecutionID) || seen[t.ExecutionID] {
			return fmt.Errorf("missing/duplicate immutable Task execution identity")
		}
		seen[t.ExecutionID] = true
	}
	return ValidateOrchestrationState(s)
}
func ValidateSchemaChange(before, after State) error {
	if e := ValidateSchema(after); e != nil {
		return e
	}
	if after.SchemaVersion < before.SchemaVersion {
		return fmt.Errorf("data schema rollback is unsupported; recover a pre-migration authenticated backup")
	}
	if before.SchemaVersion >= 2 {
		if len(after.SecretRotations) < len(before.SecretRotations) || (len(before.SecretRotations) > 0 && !reflect.DeepEqual(after.SecretRotations[:len(before.SecretRotations)], before.SecretRotations)) {
			return fmt.Errorf("immutable secret rotation receipt changed")
		}
		if len(after.SecretRotations) > len(before.SecretRotations)+1 || (len(after.SecretRotations) > len(before.SecretRotations) && after.SecretRotations[len(after.SecretRotations)-1].Revision != after.Revision) {
			return fmt.Errorf("rotation receipt must bind its single commit")
		}
		for name, j := range before.TaskSchedules {
			n, ok := after.TaskSchedules[name]
			if !ok || !reflect.DeepEqual(schedulePolicy(j), schedulePolicy(n)) {
				return fmt.Errorf("immutable Task schedule policy changed")
			}
		}
		for name, t := range before.Tasks {
			n, ok := after.Tasks[name]
			if !ok || n.UnitID != t.UnitID || !reflect.DeepEqual(n.Template, t.Template) || !reflect.DeepEqual(n.RequiredLabels, t.RequiredLabels) {
				return fmt.Errorf("retained Task attempt specification changed")
			}
		}
	}
	if before.SchemaVersion > 0 {
		if len(after.SchemaMigrations) < len(before.SchemaMigrations) || !reflect.DeepEqual(after.SchemaMigrations[:len(before.SchemaMigrations)], before.SchemaMigrations) {
			return fmt.Errorf("immutable schema migration changed")
		}
		for name, t := range before.Tasks {
			if n, ok := after.Tasks[name]; ok && n.ExecutionID != t.ExecutionID {
				return fmt.Errorf("Task execution identity changed")
			}
		}
	}
	if after.SchemaVersion > before.SchemaVersion && !(before.SchemaVersion == 0 && after.SchemaVersion == 1) {
		if after.SchemaVersion != before.SchemaVersion+1 {
			return fmt.Errorf("schema transitions must advance one step")
		}
		m := after.SchemaMigrations[len(after.SchemaMigrations)-1]
		if m.Revision != before.Revision+1 {
			return fmt.Errorf("schema transition revision mismatch")
		}
		normalized := cloneState(after)
		normalized.SchemaVersion = before.SchemaVersion
		normalized.SchemaMigrations = before.SchemaMigrations
		normalized.Revision = before.Revision
		normalized.UpdatedAt = before.UpdatedAt
		if !reflect.DeepEqual(normalized, cloneState(before)) {
			return fmt.Errorf("schema transition changed operational state")
		}
	}
	if before.SchemaVersion == 0 && after.SchemaVersion == 1 {
		m := after.SchemaMigrations[0]
		if m.Revision != before.Revision+1 {
			return fmt.Errorf("schema migration is not transactional")
		}
		// Migration changes only schema metadata and adds identity to existing Tasks.
		// Task phase, Unit/run, lease and uncertain outcomes are preserved.
		for name, t := range before.Tasks {
			n, ok := after.Tasks[name]
			if !ok {
				return fmt.Errorf("schema migration removed Task")
			}
			if n.ExecutionID != taskIdentity(before.Name, name, m.ID) {
				return fmt.Errorf("incorrect migrated Task identity")
			}
			n.ExecutionID = ""
			if !sameTask(t, n) {
				return fmt.Errorf("schema migration changed Task execution state")
			}
		}
		if len(after.Tasks) != len(before.Tasks) {
			return fmt.Errorf("schema migration added Task")
		}
		normalized := cloneState(after)
		normalized.SchemaVersion = 0
		normalized.SchemaMigrations = nil
		normalized.Revision = before.Revision
		normalized.UpdatedAt = before.UpdatedAt
		for name, t := range normalized.Tasks {
			t.ExecutionID = ""
			normalized.Tasks[name] = t
		}
		if !reflect.DeepEqual(normalized, cloneState(before)) {
			return fmt.Errorf("schema migration changed non-schema state")
		}

	}
	return nil
}
func sameTask(a, b Task) bool { return reflect.DeepEqual(a, b) }

// TransitionSchema is one revision-checked quorum transaction. The caller must
// independently verify and prepare every original voter/node before supplying
// observations. Retrying the same committed ID is safe after a lost response.
func (s *Store) TransitionSchema(id string, expected uint64, observations map[string]version.Capabilities, required []string) (SchemaMigration, error) {
	return s.TransitionSchemaTo(id, expected, 1, observations, required)
}
func (s *Store) TransitionSchemaTo(id string, expected uint64, target int, observations map[string]version.Capabilities, required []string) (SchemaMigration, error) {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	s.lock()
	defer s.unlock()
	if !migrationID.MatchString(id) {
		return SchemaMigration{}, fmt.Errorf("migration ID must be 16-128 safe characters")
	}
	for _, m := range s.data.SchemaMigrations {
		if m.ID == id {
			if m.To != target {
				return SchemaMigration{}, fmt.Errorf("migration identity reused for different target")
			}
			return m, nil
		}
	}
	if target < 1 || target > version.MaxSchema || target != s.data.SchemaVersion+1 || s.data.Revision != expected {
		return SchemaMigration{}, fmt.Errorf("schema/revision changed; inspect committed state before retrying")
	}
	if len(required) == 0 {
		return SchemaMigration{}, fmt.Errorf("membership observations required")
	}
	seen := map[string]bool{}
	for _, n := range required {
		c, ok := observations[n]
		if n == "" || seen[n] || !ok || !c.Supports(target) {
			return SchemaMigration{}, fmt.Errorf("node %s lacks target-schema compatibility", n)
		}
		seen[n] = true
	}
	for id, n := range s.data.Nodes {
		if !seen[id] || !version.Admits(n.Compatibility, target) {
			return SchemaMigration{}, fmt.Errorf("registered node %s is not upgraded", id)
		}
	}
	root := s.stateRoot
	if e := offline.PrepareSchemaFloor(root, offline.NewSchemaFloor(s.data.Name, id, target)); e != nil {
		return SchemaMigration{}, e
	}
	m := SchemaMigration{id, s.data.SchemaVersion, target, s.data.Revision + 1, time.Now().UTC()}
	s.data.SchemaVersion = target
	s.data.SchemaMigrations = append(s.data.SchemaMigrations, m)
	for name, t := range s.data.Tasks {
		if target == 1 {
			t.ExecutionID = taskIdentity(s.data.Name, name, id)
		}
		s.data.Tasks[name] = t
	}
	if e := s.commitLocked(); e != nil {
		return SchemaMigration{}, e
	}
	return m, nil
}
