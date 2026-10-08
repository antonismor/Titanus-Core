package realm

import (
	"encoding/hex"
	"fmt"
	"regexp"
)

type SourceRecord struct {
	Name   string `json:"name"`
	Digest string `json:"sha256"`
}

func (s *Store) PutSourceRecord(ref SourceRecord) error {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`).MatchString(ref.Name) {
		return fmt.Errorf("invalid Source name")
	}
	digest, err := hex.DecodeString(ref.Digest)
	if err != nil || len(digest) != 32 {
		return fmt.Errorf("invalid Source identity")
	}
	s.lock()
	defer s.unlock()
	if s.data.Sources == nil {
		s.data.Sources = map[string]SourceRecord{}
	}
	if old, ok := s.data.Sources[ref.Name]; ok && old != ref {
		return fmt.Errorf("published Source names are immutable; use a new versioned name")
	}
	s.data.Sources[ref.Name] = ref
	return s.commitLocked()
}
