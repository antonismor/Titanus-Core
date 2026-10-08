package realm

import "fmt"

// Consensus owns committed state. Apply must durably commit through a quorum
// before returning success. Its FSM must never call back into this Store.
type Consensus interface {
	Snapshot() State
	Apply(State) error
	CheckLeader() error
}

func (s *Store) EnableConsensus(c Consensus) error {
	if c == nil {
		return fmt.Errorf("nil Realm consensus")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consensus != nil {
		return fmt.Errorf("Realm consensus already enabled")
	}
	s.consensus = c
	s.data = c.Snapshot()
	return nil
}

// CheckLeader is also required before physical orchestration operations, which
// are outside the replicated state machine. Standalone behavior is unchanged.
func (s *Store) CheckLeader() error {
	s.mu.Lock()
	c := s.consensus
	managed := s.haManaged
	s.mu.Unlock()
	if c != nil {
		return c.CheckLeader()
	}
	if managed {
		return fmt.Errorf("HA-managed Realm requires daemon consensus")
	}
	return nil
}

func (s *Store) lock() {
	s.mu.Lock()
	if s.consensus != nil {
		s.data = s.consensus.Snapshot()
	} else {
		before := cloneState(s.data)
		s.transactionBefore = &before
	}
}

func (s *Store) unlock() {
	// Validation failures, rejected proposals and uncertain writes cannot leak
	// a locally mutated candidate as committed state, including on followers.
	if s.consensus != nil {
		s.data = s.consensus.Snapshot()
	} else if s.transactionBefore != nil {
		s.data = *s.transactionBefore
	}
	s.transactionBefore = nil
	s.mu.Unlock()
}
