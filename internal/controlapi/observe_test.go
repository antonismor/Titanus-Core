package controlapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/observe"
)

func TestAuditIntentOutcomeAndNoSecretPayload(t *testing.T) {
	r, err := observe.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, nil, nil, nil)
	s.Observations = r
	called := 0
	handler := identity.LocalManagement(s.authorize(func(w http.ResponseWriter, r *http.Request) { called++; w.WriteHeader(201) }))
	request := httptest.NewRequest("POST", "/v1/node/leases/object?token=TOP_SECRET", strings.NewReader(`{"password":"PAYLOAD_SECRET"}`))
	request.Header.Set("Authorization", "AUTH_SECRET")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if w.Code != 201 || called != 1 {
		t.Fatal(w.Code, called)
	}
	events, err := r.Tail()
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(events)
	for _, secret := range []string{"TOP_SECRET", "PAYLOAD_SECRET", "AUTH_SECRET", "?token"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("audit leaked secret", secret)
		}
	}
	if len(events) != 3 || events[1].Kind != "api.intent" || events[2].Kind != "api.result" || events[1].Request != events[2].Request || events[2].Status != 201 {
		t.Fatal("audit correlation", events)
	}
	// Logging failure must stop the mutation before its handler is invoked.
	os.Remove(filepath.Join(r.Dir, "events.ndjson"))
	os.Mkdir(filepath.Join(r.Dir, "events.ndjson"), 0700)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("DELETE", "/v1/node/units/anything", nil))
	if w.Code != 503 || called != 1 || r.Failures() == 0 {
		t.Fatal("unaudited mutation executed", w.Code, called)
	}
}
func TestObservationAuthorizationAndPartialDiagnostics(t *testing.T) {
	for _, path := range []string{"/v1/metrics", "/v1/diagnostics", "/v1/events"} {
		for _, role := range []identity.Role{identity.RoleNode, identity.RoleController, identity.RoleAdmin} {
			if identity.Allowed(identity.Principal{Role: role}, "GET", path) != (role != identity.RoleNode) {
				t.Fatal("observation role leak", role, path)
			}
		}
	}
	s := New(nil, nil, nil, nil)
	s.Observations, _ = observe.New(t.TempDir())
	mux := http.NewServeMux()
	s.Register(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/v1/diagnostics", nil))
	if w.Code != 403 {
		t.Fatal("unauthenticated diagnostics", w.Code)
	}
	w = httptest.NewRecorder()
	identity.LocalManagement(mux).ServeHTTP(w, httptest.NewRequest("GET", "/v1/diagnostics", nil))
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"incomplete"`) {
		t.Fatal("missing components reported healthy", w.Code, w.Body.String())
	}
}

func TestPanickingHandlerCannotPublishSuccessfulAudit(t *testing.T) {
	r, err := observe.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := New(nil, nil, nil, nil)
	s.Observations = r
	handler := identity.LocalManagement(s.authorize(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200); panic("SECRET_PANIC") }))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was swallowed")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/node/units/object/start", nil))
	}()
	events, err := r.Tail()
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Kind != "api.aborted" || last.Status != 500 {
		t.Fatal("panic claimed success", last)
	}
	data, _ := json.Marshal(events)
	if strings.Contains(string(data), "SECRET_PANIC") {
		t.Fatal("panic payload leaked")
	}
}
