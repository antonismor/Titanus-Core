package controlapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/version"
)

type schemaGate struct{}

func (schemaGate) CheckLeader() error        { return nil }
func (schemaGate) LeaderAPI() string         { return "" }
func (schemaGate) Status() map[string]string { return map[string]string{"state": "Leader"} }

type schemaClient struct {
	legacy   bool
	fail     bool
	prepared map[string]bool
}

func (c *schemaClient) Compatibility(address string) (CompatibilityInfo, error) {
	caps := version.Compatible()
	if c.legacy && address == "c2" {
		caps.WriteSchemaMax = 0
		caps.ReadSchemaMax = 0
	}
	return CompatibilityInfo{address, "LAB", 0, caps}, nil
}
func (c *schemaClient) PrepareSchemaFloor(address string, f offline.SchemaFloor) error {
	if c.fail && address == "c2" {
		return fmt.Errorf("injected unavailable original voter")
	}
	c.prepared[address] = true
	return nil
}
func TestSchemaAPIRequiresEveryOriginalVoterAndResumesInterruptedPrepare(t *testing.T) {
	root := t.TempDir()
	store, e := realm.Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	client := &schemaClient{legacy: true, prepared: map[string]bool{}}
	api := New(store, nil, nil, nil)
	api.StateRoot = root
	api.NodeID = "c0"
	api.Consensus = schemaGate{}
	api.TransitionClient = client
	api.SchemaPeers = []SchemaPeer{{"c0", "c0"}, {"c1", "c1"}, {"c2", "c2"}}
	mux := http.NewServeMux()
	api.Register(mux)
	h := identity.LocalManagement(mux)
	call := func() *httptest.ResponseRecorder {
		b, _ := json.Marshal(SchemaRequest{ID: "schema-api-test-001", ExpectedRevision: store.Snapshot().Revision})
		r := httptest.NewRequest(http.MethodPost, "/v1/realm/schema", bytes.NewReader(b))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call(); w.Code != http.StatusConflict {
		t.Fatal("legacy voter admitted", w.Code, w.Body.String())
	}
	if len(client.prepared) != 0 {
		t.Fatal("floor prepared before all capabilities verified")
	}
	client.legacy = false
	client.fail = true
	if w := call(); w.Code != http.StatusConflict {
		t.Fatal("absent voter admitted", w.Code, w.Body.String())
	}
	if store.Snapshot().SchemaVersion != 0 {
		t.Fatal("partial prepare migrated data")
	}
	if e := offline.CheckSchemaFloor(root); e != nil {
		t.Fatal("partial preparation not durable", e)
	}
	client.fail = false
	if w := call(); w.Code != http.StatusOK {
		t.Fatal("same-ID resume failed", w.Code, w.Body.String())
	}
	if store.Snapshot().SchemaVersion != 1 || len(client.prepared) != 2 {
		t.Fatal("migration omitted original voter")
	}
}
