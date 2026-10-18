package realm

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/version"
)

// WorkerFailover is a retained, quorum-committed exclusion intent. Completion
// records confirmed physical power-off; it never stands in for a storage fence.
type WorkerFailover struct {
	ID               string                `json:"id"`
	NodeID           string                `json:"node_id"`
	Address          string                `json:"address"`
	JoinedAt         time.Time             `json:"joined_at"`
	Assignments      map[string]Assignment `json:"assignments"`
	CreatedAt        time.Time             `json:"created_at"`
	CompletedAt      time.Time             `json:"completed_at,omitempty"`
	RecoveredAt      time.Time             `json:"recovered_at,omitempty"`
	RecoveryEvidence string                `json:"recovery_evidence,omitempty"`
}

// SeedNativeHA is restricted to an empty, offline, newly installed Realm.
// Existing installations are neither converted nor automatically migrated.
func (s *Store) SeedNativeHA() error {
	state := s.Snapshot()
	if s.haManaged || s.consensus != nil || len(state.Nodes) > 0 || len(state.Fleets) > 0 || len(state.Assignments) > 0 || len(state.Tasks) > 0 || len(state.Disks) > 0 || len(state.Gateways) > 0 || len(state.Sources) > 0 || len(state.Secrets) > 0 {
		return fmt.Errorf("Native HA seed requires an empty offline Realm; use a separately reviewed migration for existing installations")
	}
	if state.NativeHA {
		return nil
	}
	caps := version.Compatible()
	for target := state.SchemaVersion + 1; target <= 3; target++ {
		if _, err := s.TransitionSchemaTo(fmt.Sprintf("native-ha-initial-schema-%d", target), s.Snapshot().Revision, target, map[string]version.Capabilities{"initial-seed": caps}, []string{"initial-seed"}); err != nil {
			return err
		}
	}
	s.lock()
	defer s.unlock()
	// Fresh genesis receipts use one canonical epoch on every voter. They are
	// initial schema metadata, not online migration timestamps. This preserves
	// the exact seed binding across separately prepared controller files.
	for index, m := range s.data.SchemaMigrations {
		m.CommittedAt = time.Unix(0, 0).UTC()
		s.data.SchemaMigrations[index] = m
	}
	s.data.NativeHA = true
	return s.commitLocked()
}

func validateWorkerFailovers(s State) error {
	if len(s.WorkerFailovers) > 128 || (!s.NativeHA && len(s.WorkerFailovers) > 0) {
		return fmt.Errorf("invalid Native HA recovery inventory")
	}
	pending := map[string]bool{}
	for id, f := range s.WorkerFailovers {
		b, e := hex.DecodeString(id)
		if e != nil || len(b) != 32 || f.ID != id || f.NodeID == "" || f.Address == "" || f.JoinedAt.IsZero() || f.CreatedAt.IsZero() {
			return fmt.Errorf("invalid Worker fence identity")
		}
		n, ok := s.Nodes[f.NodeID]
		if !ok || n.ID != f.NodeID || n.Address != f.Address || !n.JoinedAt.Equal(f.JoinedAt) {
			return fmt.Errorf("Worker identity differs from retained fence")
		}
		if f.RecoveredAt.IsZero() {
			if !n.PowerQuarantined || pending[f.NodeID] {
				return fmt.Errorf("unrecovered Worker fence requires unique durable quarantine")
			}
			pending[f.NodeID] = true
		} else if f.CompletedAt.IsZero() || len(strings.TrimSpace(f.RecoveryEvidence)) < 48 {
			return fmt.Errorf("Worker recovery lacks completed fence and exclusion evidence")
		}
		for id, a := range f.Assignments {
			if id != a.ID || a.NodeID != f.NodeID {
				return fmt.Errorf("Worker fence assignment mismatch")
			}
		}
	}
	return nil
}

func validateWorkerFailoverChange(before, after State) error {
	for id, old := range before.WorkerFailovers {
		next, ok := after.WorkerFailovers[id]
		if !ok {
			return fmt.Errorf("retained Worker fence cannot be removed")
		}
		normalized := next
		if old.CompletedAt.IsZero() {
			normalized.CompletedAt = old.CompletedAt
		}
		if old.RecoveredAt.IsZero() {
			normalized.RecoveredAt = old.RecoveredAt
			normalized.RecoveryEvidence = old.RecoveryEvidence
		}
		if !reflect.DeepEqual(normalized, old) {
			return fmt.Errorf("immutable Worker fence changed")
		}
	}
	return nil
}

func (s *Store) BeginWorkerFailover(nodeID string) (WorkerFailover, error) {
	if err := s.CheckLeader(); err != nil {
		return WorkerFailover{}, err
	}
	s.lock()
	defer s.unlock()
	if !s.data.NativeHA || s.data.SchemaVersion != 3 {
		return WorkerFailover{}, fmt.Errorf("Native HA policy/schema required")
	}
	for _, f := range s.data.WorkerFailovers {
		if f.NodeID == nodeID && f.RecoveredAt.IsZero() {
			return f, nil
		}
	}
	n, ok := s.data.Nodes[nodeID]
	if !ok || n.State != NodeUnreachable || n.PowerQuarantined || !hasCapability(n.Capabilities, model.CapabilityExecution) || hasCapability(n.Capabilities, model.CapabilityControl) {
		return WorkerFailover{}, fmt.Errorf("only an unreachable, separate EXECUTION Worker can be fenced")
	}
	if len(s.data.WorkerFailovers) >= 128 {
		return WorkerFailover{}, fmt.Errorf("Worker fence retention exhausted")
	}
	assignments := map[string]Assignment{}
	for id, a := range s.data.Assignments {
		if a.NodeID == nodeID {
			assignments[id] = a
		}
	}
	if len(assignments) == 0 {
		return WorkerFailover{}, fmt.Errorf("Worker has no Fleet assignments to recover")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return WorkerFailover{}, err
	}
	f := WorkerFailover{ID: hex.EncodeToString(b), NodeID: nodeID, Address: n.Address, JoinedAt: n.JoinedAt, Assignments: assignments, CreatedAt: time.Now().UTC()}
	if s.data.WorkerFailovers == nil {
		s.data.WorkerFailovers = map[string]WorkerFailover{}
	}
	s.data.WorkerFailovers[f.ID] = f
	n.PowerQuarantined = true
	s.data.Nodes[nodeID] = n
	return f, s.commitLocked()
}

func (s *Store) CheckWorkerFailover(f WorkerFailover) error {
	if err := s.CheckLeader(); err != nil {
		return err
	}
	state := s.Snapshot()
	stored, ok := state.WorkerFailovers[f.ID]
	if !ok || !reflect.DeepEqual(stored, f) || !stored.CompletedAt.IsZero() || !state.Nodes[f.NodeID].PowerQuarantined {
		return fmt.Errorf("Worker fence intent changed")
	}
	return nil
}

func (s *Store) CompleteWorkerFailover(f WorkerFailover) error {
	if err := s.CheckLeader(); err != nil {
		return err
	}
	s.lock()
	defer s.unlock()
	stored, ok := s.data.WorkerFailovers[f.ID]
	if !ok || !reflect.DeepEqual(stored, f) || !stored.CompletedAt.IsZero() || !s.data.Nodes[f.NodeID].PowerQuarantined {
		return fmt.Errorf("Worker fence intent changed")
	}
	stored.CompletedAt = time.Now().UTC()
	s.data.WorkerFailovers[f.ID] = stored
	return s.commitLocked()
}

// WorkerFenced binds a retirement to the exact old assignment incarnation.
func WorkerFenced(state State, a Assignment) bool {
	for _, f := range state.WorkerFailovers {
		old, ok := f.Assignments[a.ID]
		if ok && !f.CompletedAt.IsZero() && f.RecoveredAt.IsZero() && failoverID(old) == failoverID(a) {
			return true
		}
	}
	return false
}
