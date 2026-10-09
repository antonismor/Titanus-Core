package realm

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"time"
)

type SecretRotation struct {
	ID          string    `json:"id"`
	Target      string    `json:"target"`
	Fingerprint string    `json:"fingerprint"`
	Revision    uint64    `json:"revision"`
	CommittedAt time.Time `json:"committed_at"`
}

// Key provisioning/readiness is verified by the authenticated API before this
// revision-checked transaction. References and logical versions never change.
func (s *Store) RotateSecrets(id, target string, expected uint64, keys *secrets.Keyring) (SecretRotation, error) {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	s.lock()
	defer s.unlock()
	for _, r := range s.data.SecretRotations {
		if r.ID == id {
			if r.Target != target {
				return SecretRotation{}, fmt.Errorf("rotation ID reused")
			}
			return r, nil
		}
	}
	if s.data.SchemaVersion < 2 || s.data.Revision != expected || !migrationID.MatchString(id) || keys == nil || keys.Active != target || len(s.data.SecretRotations) >= 16 {
		return SecretRotation{}, fmt.Errorf("schema-two revision-bound rotation required")
	}
	digest := keys.Fingerprints()[target]
	if digest == "" {
		return SecretRotation{}, fmt.Errorf("target encryption key unavailable")
	}
	candidate := map[string][]secrets.Record{}
	for name, versions := range s.data.Secrets {
		for _, v := range versions {
			if len(v.Ciphertext) == 0 {
				candidate[name] = append(candidate[name], v)
				continue
			}
			plain, e := keys.Decrypt(v)
			if e != nil {
				return SecretRotation{}, e
			}
			encrypted, e := keys.Encrypt(v.Realm, v.Name, v.Version, plain)
			clear(plain)
			if e != nil {
				return SecretRotation{}, e
			}
			candidate[name] = append(candidate[name], encrypted)
		}
	}
	marker := SecretRotation{id, target, digest, s.data.Revision + 1, time.Now().UTC()}
	s.data.Secrets = candidate
	s.data.SecretRotations = append(s.data.SecretRotations, marker)
	if e := s.commitLocked(); e != nil {
		return SecretRotation{}, e
	}
	return marker, nil
}
