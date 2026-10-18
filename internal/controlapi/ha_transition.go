package controlapi

import (
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/antonismor/Titanus-Core/internal/fencing"
	"github.com/antonismor/Titanus-Core/internal/haprofile"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/offline"

	"github.com/antonismor/Titanus-Core/internal/version"
)

type HAReadiness struct {
	ID               string        `json:"id"`
	Node             string        `json:"node"`
	Realm            string        `json:"realm"`
	BackupSetID      string        `json:"backup_set_id"`
	Capabilities     string        `json:"capabilities"`
	Build            version.Build `json:"build"`
	FloorSchema      int           `json:"floor_schema"`
	FloorMigrationID string        `json:"floor_migration_id"`
}
type HAActivateRequest struct {
	Prepared         haprofile.Prepared `json:"prepared"`
	ExpectedRevision uint64             `json:"expected_revision"`
}
type HATransitionClient interface {
	HAReadiness(string, string) (HAReadiness, error)
	Compatibility(string) (CompatibilityInfo, error)
}

func (s *Server) haReady(id string) (HAReadiness, error) {
	r, e := haprofile.ReadReady(s.StateRoot, id)
	if e != nil {
		return HAReadiness{}, e
	}
	if r.Node != s.NodeID || r.Realm != s.Store.Snapshot().Name {
		return HAReadiness{}, fmt.Errorf("staged host identity mismatch")
	}
	// Controllers must have loaded this fencing inventory in the running daemon,
	// not merely written new bytes while the old process remained running.
	if s.Consensus != nil && s.PowerFencer == nil {
		return HAReadiness{}, fmt.Errorf("controller must restart with the staged power fencer")
	}
	if s.Consensus != nil {
		loaded, ok := s.PowerFencer.(*fencing.Power)
		if !ok {
			return HAReadiness{}, fmt.Errorf("loaded native power fencer required")
		}
		path := ""
		for name := range r.Files {
			if strings.HasSuffix(name, "/power-fencing.json") {
				path = filepath.Join(r.ConfigRoot, name)
			}
		}
		expected, e := fencing.Load(path)
		if e != nil || !reflect.DeepEqual(expected.Targets, loaded.Targets) {
			return HAReadiness{}, fmt.Errorf("running fencing configuration differs from staged inventory")
		}
	}
	floor, e := offline.ReadSchemaFloor(s.StateRoot)
	if e != nil || floor.Schema != 4 || floor.Realm != r.Realm {
		return HAReadiness{}, fmt.Errorf("host has not durably prepared schema four")
	}
	if e = offline.CheckSchemaFloor(s.StateRoot); e != nil {
		return HAReadiness{}, e
	}
	return HAReadiness{ID: r.ID, Node: r.Node, Realm: r.Realm, BackupSetID: r.BackupSetID, Capabilities: r.TargetCapabilities, Build: version.Info(), FloorSchema: floor.Schema, FloorMigrationID: floor.MigrationID}, nil
}
func (s *Server) haReadiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if e := decodeJSON(r, &req); e != nil {
		writeError(w, 400, e)
		return
	}
	out, e := s.haReady(req.ID)
	if e != nil {
		writeError(w, 409, e)
		return
	}
	writeJSON(w, 200, out)
}
func (s *Server) haActivate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	actor, ok := identity.RequestPrincipal(r)
	if !ok || actor.Role != identity.RoleAdmin {
		writeError(w, 403, fmt.Errorf("HA activation requires admin"))
		return
	}
	if s.Consensus == nil || s.HATransitionClient == nil || (len(s.SchemaPeers) != 3 && len(s.SchemaPeers) != 5) {
		writeError(w, 409, fmt.Errorf("existing three/five-voter quorum required; standalone or membership conversion is not supported"))
		return
	}
	var req HAActivateRequest
	if e := decodeJSON(r, &req); e != nil {
		writeError(w, 400, e)
		return
	}
	p := req.Prepared
	if e := p.Validate(); e != nil {
		writeError(w, 400, e)
		return
	}
	state := s.Store.Snapshot()
	if state.HAActivation != nil {
		if state.HAActivation.ID == p.ID {
			writeJSON(w, 200, state.HAActivation)
			return
		}
		writeError(w, 409, fmt.Errorf("different activation already committed"))
		return
	}
	if e := haprofile.ValidateTarget(state, p.Plan); e != nil {
		writeError(w, 409, e)
		return
	}
	if p.Plan.Installation.Revision != version.Revision || p.Plan.Installation.ReleaseVersion != version.Version {
		writeError(w, 409, fmt.Errorf("transition must target this exact upgraded build"))
		return
	}
	peers := map[string]bool{}
	for _, peer := range s.SchemaPeers {
		peers[peer.ID] = true
	}
	for _, n := range p.Plan.ControlNodes() {
		if !peers[n.Name] {
			writeError(w, 409, fmt.Errorf("original voter membership differs"))
			return
		}
	}
	if len(p.Plan.ControlNodes()) != len(peers) {
		writeError(w, 409, fmt.Errorf("controller membership cannot change"))
		return
	}
	target := map[string][]model.Capability{}
	for _, n := range p.Plan.Nodes {
		node := state.Nodes[n.Name]
		var ready HAReadiness
		var e error
		if n.Name == s.NodeID {
			ready, e = s.haReady(p.ID)
		} else {
			ready, e = s.HATransitionClient.HAReadiness(node.Address, p.ID)
		}
		if e != nil || ready.ID != p.ID || ready.Node != n.Name || ready.Realm != state.Name || ready.BackupSetID != p.BackupSetID || ready.Capabilities != haprofile.Capabilities(n.Capabilities) || !compatibleHABuild(ready.Build) || ready.FloorSchema != 4 || len(state.SchemaMigrations) != 4 || ready.FloorMigrationID != state.SchemaMigrations[3].ID {
			writeError(w, 409, fmt.Errorf("host %s has not acknowledged the exact staged transition/build", n.Name))
			return
		}
		info, e := s.HATransitionClient.Compatibility(node.Address)
		if n.Name == s.NodeID {
			info = CompatibilityInfo{s.NodeID, state.Name, state.SchemaVersion, version.Compatible()}
			e = nil
		}
		if e != nil || info.NodeID != n.Name || info.Realm != state.Name || !info.Capabilities.Supports(4) {
			writeError(w, 409, fmt.Errorf("host %s schema readiness failed", n.Name))
			return
		}
		target[n.Name] = n.Capabilities
	}
	if p.Plan.NativeHAStorage() {
		if s.HACephProbe == nil {
			writeError(w, 409, fmt.Errorf("read-only Ceph HA verification unavailable"))
			return
		}
		if e := s.HACephProbe(p.Plan); e != nil {
			writeError(w, 409, e)
			return
		}
	}
	receipt, e := s.Store.ActivateNativeHA(p.ID, p.BackupSetID, req.ExpectedRevision, target)
	if e != nil {
		writeError(w, 409, e)
		return
	}
	writeJSON(w, 200, receipt)
}

func compatibleHABuild(b version.Build) bool {
	current := version.Info()
	return b.Version == current.Version && b.Revision == current.Revision && b.OS == current.OS && b.StateProfile == current.StateProfile && (b.Arch == "amd64" || b.Arch == "arm64")
}
