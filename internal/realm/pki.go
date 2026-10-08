package realm

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/identity"
)

// MutatePKI serializes signing with every other Realm update and publishes
// only after durable quorum commit. A signed but rejected result is not served.
func (s *Store) MutatePKI(update func(identity.Policy) (identity.Policy, error)) (identity.Policy, error) {
	s.lock()
	defer s.unlock()
	if s.consensus != nil {
		if err := s.consensus.CheckLeader(); err != nil {
			return identity.Policy{}, err
		}
	} else if s.haManaged {
		return identity.Policy{}, fmt.Errorf("PKI requires live HA consensus")
	}
	next, err := update(s.data.PKI)
	if err != nil {
		return identity.Policy{}, err
	}
	if s.data.PKI.CA != "" && (next.CA != s.data.PKI.CA || next.Issued < s.data.PKI.Issued) {
		return identity.Policy{}, fmt.Errorf("PKI identity/sequence rollback denied")
	}
	s.data.PKI = next
	if err = s.commitLocked(); err != nil {
		return identity.Policy{}, err
	}
	return next, nil
}
