package realm

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
)

type StorageFailover struct {
	ID          string                  `json:"id"`
	Assignment  Assignment              `json:"assignment"`
	Catalogs    map[string]disk.Catalog `json:"catalogs"`
	Completed   bool                    `json:"completed"`
	CreatedAt   time.Time               `json:"created_at"`
	CompletedAt time.Time               `json:"completed_at,omitempty"`
}

func failoverID(a Assignment) string {
	b, _ := json.Marshal(struct {
		ID, Node, Token string
		Generation      uint64
		Created         time.Time
	}{a.ID, a.NodeID, a.LeaseToken, a.Generation, a.CreatedAt})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// Catalog registration is deliberately offline. Snapshots are inventories of
// actual native objects, not fabricated cross-node copies of node-local data.
func (s *Store) PutDiskCatalog(c disk.Catalog) error {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	s.lock()
	defer s.unlock()
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Spec.ManagedID != "" {
		return fmt.Errorf("catalog cannot contain a local realization marker")
	}
	for _, a := range s.data.Assignments {
		if a.Fleet == c.Fleet {
			return fmt.Errorf("catalog registration requires offline Fleet without assignments")
		}
	}
	if f, ok := s.data.Fleets[c.Fleet]; ok && f.Instances > 1 {
		return fmt.Errorf("catalog-bound Fleet permits at most one instance")
	}
	if s.data.Disks == nil {
		s.data.Disks = map[string]disk.Catalog{}
	}
	if old, ok := s.data.Disks[c.Spec.Name]; ok {
		if old.ID() != c.ID() || old.Fleet != c.Fleet || old.LocalNode != c.LocalNode || old.AutoFailover != c.AutoFailover {
			return fmt.Errorf("registered Disk identity/ownership is immutable")
		}
	}
	for _, f := range s.data.Fleets {
		for _, m := range f.Template.Mounts {
			if m.Disk == c.Spec.Name && f.Name != c.Fleet {
				return fmt.Errorf("Disk is referenced by another Fleet")
			}
		}
	}
	s.data.Disks[c.Spec.Name] = c
	return s.commitLocked()
}

// Allocation is committed before execution and retained across node handoff.
// The high half of the reserved mapping pool belongs to cluster workloads.
func (s *Store) UnitMapping(id string) (unitruntime.IDMapping, error) {
	s.lock()
	defer s.unlock()
	if s.data.UnitMappings == nil {
		s.data.UnitMappings = map[string]unitruntime.IDMapping{}
	}
	a, ok := s.data.Assignments[id]
	if !ok {
		return unitruntime.IDMapping{}, fmt.Errorf("unknown assignment")
	}
	key, err := StorageMappingIdentity(s.data, a)
	if err != nil {
		return unitruntime.IDMapping{}, err
	}
	if m, ok := s.data.UnitMappings[key]; ok {
		return m, nil
	}
	base := 1073741824
	for _, m := range s.data.UnitMappings {
		if m.Base >= base {
			base = m.Base + 65536
		}
	}
	if base > 2147418112 {
		return unitruntime.IDMapping{}, fmt.Errorf("Realm mapping pool exhausted")
	}
	m := unitruntime.IDMapping{Base: base, Size: 65536}
	s.data.UnitMappings[key] = m
	if err := s.commitLocked(); err != nil {
		return unitruntime.IDMapping{}, err
	}
	return m, nil
}

func (s *Store) RecordStorageWriters(a Assignment, writers map[string]disk.Writer) error {
	s.lock()
	defer s.unlock()
	current, ok := s.data.Assignments[a.ID]
	if !ok || failoverID(current) != failoverID(a) {
		return fmt.Errorf("assignment changed before writer publication")
	}
	for name, w := range writers {
		c, ok := s.data.Disks[name]
		if !ok || c.Fleet != a.Fleet || disk.ValidateWriter(c, w) != nil {
			return fmt.Errorf("writer does not match committed catalog")
		}
	}
	if reflect.DeepEqual(current.StorageWriters, writers) {
		return nil
	}
	current.StorageWriters = writers
	s.data.Assignments[a.ID] = current
	return s.commitLocked()
}

func (s *Store) BeginStorageFailover(a Assignment) (StorageFailover, error) {
	s.lock()
	defer s.unlock()
	id := failoverID(a)
	if existing, ok := s.data.StorageFailovers[id]; ok {
		return existing, nil
	}
	current, ok := s.data.Assignments[a.ID]
	if !ok || failoverID(current) != id || !reflect.DeepEqual(current.StorageWriters, a.StorageWriters) {
		return StorageFailover{}, fmt.Errorf("assignment/writer changed before fencing")
	}
	n, ok := s.data.Nodes[a.NodeID]
	if !ok || n.State != NodeUnreachable || a.LeaseExpiresAt.IsZero() || time.Now().Before(a.LeaseExpiresAt.Add(2*time.Second)) {
		return StorageFailover{}, fmt.Errorf("unreachable node and expired dispatch lease required")
	}
	f, ok := s.data.Fleets[a.Fleet]
	if !ok {
		return StorageFailover{}, fmt.Errorf("deleted Fleet requires manual fencing")
	}
	f, err := f.ForGeneration(a.Generation)
	if err != nil {
		return StorageFailover{}, err
	}
	intent := StorageFailover{ID: id, Assignment: current, Catalogs: map[string]disk.Catalog{}, CreatedAt: time.Now().UTC()}
	for _, m := range f.Template.Mounts {
		c, ok := s.data.Disks[m.Disk]
		w, wok := a.StorageWriters[m.Disk]
		if !ok || !wok || !c.AutoFailover || c.Fleet != a.Fleet || c.Spec.Provider == disk.ProviderLocal || disk.ValidateWriter(c, w) != nil {
			return StorageFailover{}, fmt.Errorf("every mount requires a registered remote catalog and exact writer")
		}
		intent.Catalogs[m.Disk] = c
	}
	if len(intent.Catalogs) == 0 {
		return StorageFailover{}, fmt.Errorf("no remote Disk to fence")
	}
	if s.data.StorageFailovers == nil {
		s.data.StorageFailovers = map[string]StorageFailover{}
	}
	if len(s.data.StorageFailovers) >= 128 {
		return StorageFailover{}, fmt.Errorf("retained failover limit reached; archive intents before more handoffs")
	}
	s.data.StorageFailovers[id] = intent
	// Quarantine is committed with the intent. Pulse/re-registration cannot
	// turn the node into a scheduling candidate while its old mounts remain.
	n.StorageQuarantined = true
	s.data.Nodes[n.ID] = n
	if err = s.commitLocked(); err != nil {
		return StorageFailover{}, err
	}
	return intent, nil
}

func (s *Store) CompleteStorageFailover(intent StorageFailover) error {
	s.lock()
	defer s.unlock()
	stored, ok := s.data.StorageFailovers[intent.ID]
	if !ok || !reflect.DeepEqual(stored, intent) {
		return fmt.Errorf("failover intent changed")
	}
	a, ok := s.data.Assignments[intent.Assignment.ID]
	if !ok || failoverID(a) != intent.ID {
		return fmt.Errorf("assignment changed while fencing")
	}
	stored.Completed = true
	stored.CompletedAt = time.Now().UTC()
	s.data.StorageFailovers[intent.ID] = stored
	a.State = AssignmentStopped
	s.data.Assignments[a.ID] = a
	return s.commitLocked()
}

func validateCatalogFleet(state State, f Fleet) error {
	for _, m := range f.Template.Mounts {
		if c, ok := state.Disks[m.Disk]; ok && (c.Fleet != f.Name || f.Instances > 1) {
			return fmt.Errorf("catalog Disk is pinned to a single-instance owning Fleet")
		}
	}
	return nil
}

// Ownership follows the immutable managed Disk set across rollout generations.
func StorageMappingIdentity(state State, a Assignment) (string, error) {
	f, ok := state.Fleets[a.Fleet]
	if !ok {
		return "", fmt.Errorf("unknown Fleet")
	}
	f, err := f.ForGeneration(a.Generation)
	if err != nil {
		return "", err
	}
	ids := []string{}
	for _, m := range f.Template.Mounts {
		if c, ok := state.Disks[m.Disk]; ok && c.Spec.Provider != disk.ProviderLocal {
			ids = append(ids, c.ID())
		}
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("no managed remote Disk identity")
	}
	sort.Strings(ids)
	b, _ := json.Marshal(ids)
	h := sha256.Sum256(b)
	return "storage-" + hex.EncodeToString(h[:24]), nil
}
