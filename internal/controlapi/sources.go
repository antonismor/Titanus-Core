package controlapi

import (
	"context"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/source"
	"log"
	"net/http"
	"strings"
	"time"
)

type SourceReplicator interface {
	EnsureSource(string, string, *source.Manager) error
}

type SourceRecovery interface {
	RecoverSource(realm.SourceRecord, []string, *source.Manager) error
}

// Payload work is separate from lease renewal: one unavailable Source must not
// make healthy unrelated workloads lose their controller dispatch leases.
func (s *Server) MaintainSources(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		state := s.Store.Snapshot()
		if len(s.SourcePeers) > 0 && s.Store.CheckLeader() == nil {
			names := map[string]bool{}
			for _, f := range state.Fleets {
				names[f.Template.Source] = true
				for _, h := range f.History {
					names[h.Template.Source] = true
				}
			}
			for _, task := range state.Tasks {
				if !task.Terminal() {
					names[task.Template.Source] = true
				}
			}
			for name := range names {
				if _, ok := state.Sources[name]; !ok {
					if _, err := s.PublishSourceRecord(name); err != nil {
						log.Printf("Source %s publication pending: %v", name, err)
					}
				}
			}
		}
		if recovery, ok := s.SourceReplicator.(SourceRecovery); ok {
			for _, ref := range s.Store.Snapshot().Sources {
				if err := recovery.RecoverSource(ref, s.SourcePeers, s.Sources); err != nil {
					log.Printf("Source %s recovery pending: %v", ref.Name, err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
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
