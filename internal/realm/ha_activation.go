package realm

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/model"
	"reflect"
	"regexp"
	"time"
)

type HAActivation struct {
	ID             string    `json:"id"`
	BackupSetID    string    `json:"backup_set_id"`
	BeforeRevision uint64    `json:"before_revision"`
	CommittedAt    time.Time `json:"committed_at"`
}

func (s *Store) ActivateNativeHA(id, backupID string, expected uint64, target map[string][]model.Capability) (HAActivation, error) {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	if e := s.CheckLeader(); e != nil {
		return HAActivation{}, e
	}
	s.lock()
	defer s.unlock()
	if s.data.HAActivation != nil {
		if s.data.HAActivation.ID == id && s.data.HAActivation.BackupSetID == backupID {
			return *s.data.HAActivation, nil
		}
		return HAActivation{}, fmt.Errorf("different HA activation is already committed")
	}
	hash := regexp.MustCompile(`^[a-f0-9]{64}$`)
	if !hash.MatchString(id) || !hash.MatchString(backupID) || s.data.NativeHA || s.data.SchemaVersion != 4 || s.data.Revision != expected || len(s.data.Nodes) != len(target) {
		return HAActivation{}, fmt.Errorf("HA activation identity/schema/revision changed")
	}
	if len(s.data.Assignments) > 0 {
		return HAActivation{}, fmt.Errorf("HA activation requires no assignments")
	}
	for _, f := range s.data.Fleets {
		if f.Instances != 0 {
			return HAActivation{}, fmt.Errorf("HA activation requires quiescent Fleets")
		}
	}
	for _, t := range s.data.Tasks {
		if !t.Terminal() {
			return HAActivation{}, fmt.Errorf("nonterminal Task prevents HA activation")
		}
	}
	for _, f := range s.data.GatewayTransfers {
		if !f.Complete {
			return HAActivation{}, fmt.Errorf("pending Gateway transfer")
		}
	}
	for _, f := range s.data.StorageFailovers {
		if !f.Completed {
			return HAActivation{}, fmt.Errorf("pending storage failover")
		}
	}
	for _, d := range s.data.Disks {
		if d.Spec.Provider == disk.ProviderLocal || !d.AutoFailover {
			return HAActivation{}, fmt.Errorf("non-HA Disk catalog")
		}
	}
	controls, workers := 0, 0
	for id, caps := range target {
		n, ok := s.data.Nodes[id]
		if !ok || n.State != NodeReady || n.PowerQuarantined || n.StorageQuarantined || time.Since(n.LastPulse) > 15*time.Second || !reflect.DeepEqual(n.Capabilities, caps) {
			return HAActivation{}, fmt.Errorf("host %s is not freshly ready with target roles", id)
		}
		control, worker := hasCapability(caps, model.CapabilityControl), hasCapability(caps, model.CapabilityExecution)
		if control {
			controls++
		}
		if worker {
			workers++
		}
		if control && worker {
			return HAActivation{}, fmt.Errorf("CONTROL and EXECUTION must be separate")
		}
	}
	if (controls != 3 && controls != 5) || workers < 2 || len(target) > model.CommunityNodeLimit {
		return HAActivation{}, fmt.Errorf("invalid HA voting/Worker topology")
	}
	receipt := HAActivation{id, backupID, s.data.Revision, time.Now().UTC()}
	s.data.NativeHA = true
	s.data.HAActivation = &receipt
	return receipt, s.commitLocked()
}
