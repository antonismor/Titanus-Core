package controlapi

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"github.com/antonismor/Titanus-Core/internal/version"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type recoveryProbe struct {
	info  CompatibilityInfo
	units []unitruntime.State
	err   error
}

func (p *recoveryProbe) Compatibility(string) (CompatibilityInfo, error) { return p.info, p.err }
func (p *recoveryProbe) ListUnits(string) ([]unitruntime.State, error)   { return p.units, p.err }
func TestNativeWorkerRecoveryRequiresIdentityIdleRuntimeAndEvidence(t *testing.T) {
	s, e := realm.Open(t.TempDir(), "LAB")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.SeedNativeHA(); e != nil {
		t.Fatal(e)
	}
	caps := version.Compatible()
	if e = s.UpsertNode(realm.Node{ID: "w", Address: "w", State: realm.NodeUnreachable, Capabilities: []model.Capability{model.CapabilityExecution}, Compatibility: &caps}); e != nil {
		t.Fatal(e)
	}
	a := realm.Assignment{ID: "web", NodeID: "w", Fleet: "app"}
	if e = s.SetAssignments("app", []realm.Assignment{a}); e != nil {
		t.Fatal(e)
	}
	f, e := s.BeginWorkerFailover("w")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.CompleteWorkerFailover(f); e != nil {
		t.Fatal(e)
	}
	if e = s.SetAssignments("app", nil); e != nil {
		t.Fatal(e)
	}
	if e = s.Pulse("w", realm.Resources{}); e != nil {
		t.Fatal(e)
	}
	probe := &recoveryProbe{info: CompatibilityInfo{NodeID: "w", Realm: "LAB", Capabilities: caps}}
	api := New(s, nil, nil, nil)
	api.WorkerRecovery = probe
	mux := http.NewServeMux()
	api.Register(mux)
	evidence := strings.Repeat("Old Units and mounts independently excluded; identity reviewed. ", 2)
	request := func() int {
		r := httptest.NewRequest("POST", "/v1/realm/nodes/recover-power", strings.NewReader(fmt.Sprintf(`{"node":"w","evidence":%q}`, evidence)))
		w := httptest.NewRecorder()
		identity.LocalManagement(mux).ServeHTTP(w, r)
		return w.Code
	}
	probe.info.NodeID = "wrong"
	if request() == 200 {
		t.Fatal("wrong runtime identity admitted")
	}
	probe.info.NodeID = "w"
	probe.units = []unitruntime.State{{}}
	if request() == 200 {
		t.Fatal("nonempty runtime admitted")
	}
	probe.units = nil
	probe.err = fmt.Errorf("unavailable")
	if request() == 200 {
		t.Fatal("unreachable runtime admitted")
	}
	probe.err = nil
	n := s.Snapshot().Nodes["w"]
	n.LastPulse = time.Now().Add(-time.Minute)
	if e = s.UpsertNode(n); e != nil {
		t.Fatal(e)
	}
	if request() == 200 {
		t.Fatal("stale Pulse admitted")
	}
	if e = s.Pulse("w", realm.Resources{}); e != nil {
		t.Fatal(e)
	}
	if status := request(); status != 200 {
		t.Fatal("verified recovery rejected", status)
	}
	if s.Snapshot().Nodes["w"].PowerQuarantined || s.Snapshot().WorkerFailovers[f.ID].RecoveryEvidence != evidence {
		t.Fatal("reviewed recovery not persisted")
	}
}
