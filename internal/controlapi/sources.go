package controlapi

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/source"
	"net/http"
	"strings"
)

type SourceReplicator interface {
	EnsureSource(string, string, *source.Manager) error
}

func (s *Server) publishSource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	principal, ok := identity.RequestPrincipal(r)
	if !ok || principal.Role != identity.RoleAdmin {
		writeError(w, 403, fmt.Errorf("Source publication requires admin"))
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/v1/realm/sources/")
	ref, err := s.PublishSourceRecord(name)
	if err != nil {
		writeError(w, 503, err)
		return
	}
	writeJSON(w, 201, ref)
}

func (s *Server) PublishSourceRecord(name string) (realm.SourceRecord, error) {
	if s.Sources == nil {
		return realm.SourceRecord{}, fmt.Errorf("local Source manager unavailable")
	}
	if err := s.Store.CheckLeader(); err != nil {
		return realm.SourceRecord{}, err
	}
	digest, err := s.Sources.Identity(name)
	if err != nil {
		return realm.SourceRecord{}, err
	}
	for _, peer := range s.SourcePeers {
		if err = s.Store.CheckLeader(); err != nil {
			return realm.SourceRecord{}, err
		}
		if s.SourceReplicator == nil {
			return realm.SourceRecord{}, fmt.Errorf("Source replication unavailable")
		}
		if err = s.SourceReplicator.EnsureSource(peer, name, s.Sources); err != nil {
			return realm.SourceRecord{}, err
		}
	}
	if actual, e := s.Sources.Identity(name); e != nil || actual != digest {
		return realm.SourceRecord{}, fmt.Errorf("Source changed during replication")
	}
	ref := realm.SourceRecord{Name: name, Digest: digest}
	return ref, s.Store.PutSourceRecord(ref)
}
