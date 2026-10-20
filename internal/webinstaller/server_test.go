package webinstaller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/model"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	s, err := New(Config{Listen: "127.0.0.1:0", StateDir: filepath.Join(dir, "state"), LogFile: filepath.Join(dir, "installer.log")})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validPlan() model.RealmPlan {
	return model.RealmPlan{
		Version:     "titanus-plan/v2",
		RealmName:   "TEST-REALM",
		FabricCIDR:  "10.210.0.0/16",
		ServiceCIDR: "10.220.0.0/16",
		CreatedAt:   time.Now().UTC(),
		Nodes: []model.NodeSpec{{
			Name: "node01", ManagementIP: "192.0.2.10", SSHUser: "root", SSHPort: 22,
			Capabilities: []model.Capability{model.CapabilityControl, model.CapabilityExecution},
		}},
		Installation: &model.Installation{
			Profile: model.ProfileStandard, Operation: "install", ReleaseVersion: "0.4.0-rc.17",
			Revision: strings.Repeat("a", 40),
			Archives: map[string]model.Archive{
				"amd64": {Path: "/tmp/amd64.tar.gz", SHA256: strings.Repeat("b", 64)},
				"arm64": {Path: "/tmp/arm64.tar.gz", SHA256: strings.Repeat("c", 64)},
			},
			PKIDir: "/tmp/titanus-pki", APIPort: 9443, RaftPort: 9444, NodePrefix: 24, VXLANID: 4242, Start: true,
		},
	}
}

func TestValidatePersistsStatusAcrossBrowserReconnect(t *testing.T) {
	s := testServer(t)
	body, _ := json.Marshal(validPlan())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/validate", bytes.NewReader(body))
	req.Header.Set("X-Titanus-Installer-Token", s.accessToken)
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("validate status=%d body=%s", res.Code, res.Body.String())
	}
	var state InstallerState
	if err := json.Unmarshal(res.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Status != "validated" || state.Progress != 8 || state.PlanPath == "" {
		t.Fatalf("unexpected state: %+v", state)
	}

	second, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/status", nil)
	req.Header.Set("X-Titanus-Installer-Token", second.accessToken)
	res = httptest.NewRecorder()
	second.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"status":"validated"`) {
		t.Fatalf("reconnect status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestInterruptedRunIsVisibleAfterServiceRestart(t *testing.T) {
	s := testServer(t)
	s.mu.Lock()
	s.state.Running = true
	s.state.Status = "running"
	s.state.Progress = 63
	s.state.Message = "Installing"
	if err := s.persistStateLocked(); err != nil {
		t.Fatal(err)
	}
	s.mu.Unlock()

	second, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if second.state.Status != "interrupted" || second.state.Progress != 63 || second.state.Running {
		t.Fatalf("unexpected recovered state: %+v", second.state)
	}
}

func TestWebAssetsUseSecurityHeaders(t *testing.T) {
	s := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("index status=%d", res.Code)
	}
	if res.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(res.Header().Get("Content-Security-Policy"), "default-src 'self'") {
		t.Fatalf("missing security headers: %#v", res.Header())
	}
}

func TestAccessFragmentCanEstablishReconnectCookie(t *testing.T) {
	s := testServer(t)
	body, _ := json.Marshal(sessionRequest{Token: s.accessToken})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/session", bytes.NewReader(body))
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("session status=%d body=%s", res.Code, res.Body.String())
	}
	cookies := res.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookie || !cookies[0].HttpOnly || !cookies[0].Secure {
		t.Fatalf("unexpected session cookie: %#v", cookies)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/bootstrap", nil)
	req.AddCookie(cookies[0])
	res = httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("cookie reconnect status=%d body=%s", res.Code, res.Body.String())
	}
}

func TestBootstrapIncludesPersistedPlanForReconnect(t *testing.T) {
	s := testServer(t)
	plan := validPlan()
	body, _ := json.Marshal(plan)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/validate", bytes.NewReader(body))
	req.Header.Set("X-Titanus-Installer-Token", s.accessToken)
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatal(res.Body.String())
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/bootstrap", nil)
	req.Header.Set("X-Titanus-Installer-Token", s.accessToken)
	res = httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `"realm_name":"TEST-REALM"`) {
		t.Fatalf("bootstrap did not restore plan: %s", res.Body.String())
	}
}


func TestValidatedPlanCanBeDownloaded(t *testing.T) {
	s := testServer(t)
	body, _ := json.Marshal(validPlan())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/validate", bytes.NewReader(body))
	req.Header.Set("X-Titanus-Installer-Token", s.accessToken)
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("validate status=%d body=%s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/v1/plan", nil)
	req.Header.Set("X-Titanus-Installer-Token", s.accessToken)
	res = httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("plan status=%d body=%s", res.Code, res.Body.String())
	}
	if disposition := res.Header().Get("Content-Disposition"); disposition != `attachment; filename="titanus-plan.json"` {
		t.Fatalf("unexpected content disposition: %q", disposition)
	}
	var plan model.RealmPlan
	if err := json.Unmarshal(res.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.RealmName != "TEST-REALM" || len(plan.Nodes) != 1 {
		t.Fatalf("unexpected downloaded plan: %+v", plan)
	}
}
