package controlapi

import (
	"bytes"
	"encoding/json"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/observe"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"github.com/antonismor/Titanus-Core/internal/version"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

type rotationClient struct {
	fingerprints map[string]string
	bad          bool
}

func (c *rotationClient) KeyReadiness(address string) (KeyReadiness, error) {
	keys := map[string]string{}
	for k, v := range c.fingerprints {
		keys[k] = v
	}
	if c.bad && address == "c2" {
		keys["two"] = "different-key"
	}
	return KeyReadiness{address, "LAB", keys}, nil
}
func (*rotationClient) NodeObservation(string) (observe.NodeObservation, error) {
	return observe.NodeObservation{}, nil
}
func TestRotationRequiresEveryOriginalHostKeyReadiness(t *testing.T) {
	root := t.TempDir()
	store, e := realm.Open(root, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	for target := 1; target <= 2; target++ {
		if _, e = store.TransitionSchemaTo("api-m6-transition-00"+string(rune('0'+target)), store.Snapshot().Revision, target, map[string]version.Capabilities{"c0": version.Compatible()}, []string{"c0"}); e != nil {
			t.Fatal(e)
		}
	}
	keys := &secrets.Keyring{Active: "one", Keys: map[string][]byte{"one": bytes.Repeat([]byte{1}, 32), "two": bytes.Repeat([]byte{2}, 32)}}
	record, e := keys.Encrypt("LAB", "password", 1, []byte("DO_NOT_EXPOSE_SECRET"))
	if e != nil {
		t.Fatal(e)
	}
	if e = store.PutSecret(record); e != nil {
		t.Fatal(e)
	}
	keys.Active = "two"
	path := filepath.Join(root, "private.json")
	if e = durable.WriteJSON(path, keys, 0600); e != nil {
		t.Fatal(e)
	}
	api := New(store, nil, nil, nil)
	api.NodeID = "c0"
	api.SecretKeyring = path
	api.SchemaPeers = []SchemaPeer{{"c0", "c0"}, {"c1", "c1"}, {"c2", "c2"}}
	api.Consensus = schemaGate{}
	client := &rotationClient{fingerprints: keys.Fingerprints(), bad: true}
	api.M6Client = client
	mux := http.NewServeMux()
	api.Register(mux)
	h := identity.LocalManagement(mux)
	revision := store.Snapshot().Revision
	request := map[string]any{"id": "api-rotation-proof-001", "target": "two", "expected_revision": revision}
	call := func() *httptest.ResponseRecorder {
		raw, _ := json.Marshal(request)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/realm/secret-rotation", bytes.NewReader(raw)))
		return w
	}
	if w := call(); w.Code != 409 {
		t.Fatal("mismatched node key admitted", w.Code, w.Body.String())
	}
	if store.Snapshot().Revision != revision {
		t.Fatal("failed readiness partially committed vault")
	}
	client.bad = false
	if w := call(); w.Code != 200 || bytes.Contains(w.Body.Bytes(), []byte("DO_NOT_EXPOSE_SECRET")) {
		t.Fatal("rotation failed or leaked", w.Code, w.Body.String())
	}
	committed := store.Snapshot().Revision
	if w := call(); w.Code != 200 || store.Snapshot().Revision != committed {
		t.Fatal("uncertain response repeated vault transaction")
	}
	for _, path := range []string{"/v1/node/secret-keyring", "/v1/node/observation", "/v1/realm/task-schedules", "/v1/realm/observations", "/v1/realm/alerts"} {
		if identity.Allowed(identity.Principal{ID: "worker", Role: identity.RoleNode}, "GET", path) {
			t.Fatal("node admitted protected M6 data", path)
		}
	}
}
