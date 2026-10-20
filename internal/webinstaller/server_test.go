package webinstaller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func requestJSON(t *testing.T, s *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	req.Header.Set("X-Titanus-Installer-Token", s.accessToken)
	res := httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	return res
}

func TestMutationsCannotReplaceActiveDeployment(t *testing.T) {
	s := testServer(t)
	if res := requestJSON(t, s, http.MethodPost, "/api/v1/validate", validPlan()); res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	s.mu.Lock()
	s.running = true
	s.state.Running = true
	s.state.Progress = 63
	s.mu.Unlock()
	for _, path := range []string{"/api/v1/validate", "/api/v1/reset", "/api/v1/discover", "/api/v1/start", "/api/v1/quiesce"} {
		var body any = map[string]bool{"apply": false}
		if strings.HasSuffix(path, "validate") {
			body = validPlan()
		}
		if res := requestJSON(t, s, http.MethodPost, path, body); res.Code != http.StatusConflict {
			t.Fatalf("%s: %d %s", path, res.Code, res.Body.String())
		}
	}
	if s.state.Progress != 63 || !s.running {
		t.Fatal("active state was replaced")
	}
	if res := requestJSON(t, s, http.MethodGet, "/api/v1/status", nil); res.Code != 200 {
		t.Fatal("monitoring must remain available")
	}
}

func TestQuiesceClosesAdmissionBeforeTerminalSwitch(t *testing.T) {
	s := testServer(t)
	if res := requestJSON(t, s, http.MethodPost, "/api/v1/quiesce", nil); res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	for _, path := range []string{"/api/v1/validate", "/api/v1/discover", "/api/v1/start", "/api/v1/reset"} {
		if res := requestJSON(t, s, http.MethodPost, path, validPlan()); res.Code != http.StatusConflict {
			t.Fatalf("%s: %d", path, res.Code)
		}
	}
	if res := requestJSON(t, s, http.MethodGet, "/api/v1/status", nil); res.Code != 200 {
		t.Fatal("quiesce hid persisted status")
	}
}

func TestStartRequiresDurableLaunchStatus(t *testing.T) {
	s := testServer(t)
	if res := requestJSON(t, s, http.MethodPost, "/api/v1/validate", validPlan()); res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	path := filepath.Join(s.cfg.StateDir, "state.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	res := requestJSON(t, s, http.MethodPost, "/api/v1/start", map[string]bool{"apply": false})
	if res.Code != 500 || s.running || s.state.Running {
		t.Fatalf("launch not blocked: %d %+v", res.Code, s.state)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.StateDir, "deployments")); !os.IsNotExist(err) {
		t.Fatal("deployment started despite persistence failure")
	}
}

func TestValidationDoesNotPublishUnpersistedPlan(t *testing.T) {
	s := testServer(t)
	if err := os.Mkdir(filepath.Join(s.cfg.StateDir, "state.json"), 0700); err != nil {
		t.Fatal(err)
	}
	if res := requestJSON(t, s, http.MethodPost, "/api/v1/validate", validPlan()); res.Code != 500 {
		t.Fatal(res.Body.String())
	}
	if _, ok := s.currentPlan(); ok {
		t.Fatal("unpersisted validation accepted")
	}
	if res := requestJSON(t, s, http.MethodPost, "/api/v1/start", map[string]bool{"apply": false}); res.Code != 400 {
		t.Fatal(res.Body.String())
	}
}

func TestLogTailIsBoundedAndProtected(t *testing.T) {
	s := testServer(t)
	data := strings.Repeat("old log entry\n", maxLogBytes/5) + "LATEST-EVENT\n"
	if err := os.WriteFile(s.cfg.LogFile, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	tail, err := tailLog(s.cfg.LogFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) > maxLogBytes || !bytes.Contains(tail, []byte("LATEST-EVENT")) {
		t.Fatal("invalid log tail")
	}
	res := requestJSON(t, s, http.MethodGet, "/api/v1/logs", nil)
	var payload struct {
		Lines []string `json:"lines"`
	}
	json.Unmarshal(res.Body.Bytes(), &payload)
	if res.Code != 200 || len(payload.Lines) > 600 || payload.Lines[len(payload.Lines)-1] != "LATEST-EVENT" {
		t.Fatal("latest bounded events missing")
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs", nil)
	res = httptest.NewRecorder()
	s.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatal("verbose log exposed without session")
	}
}

func TestConcurrentQuiesceAndValidationCannotReopenAdmission(t *testing.T) {
	s := testServer(t)
	var group sync.WaitGroup
	for i := 0; i < 20; i++ {
		group.Add(1)
		go func() { defer group.Done(); requestJSON(t, s, http.MethodPost, "/api/v1/validate", validPlan()) }()
	}
	group.Add(1)
	go func() { defer group.Done(); requestJSON(t, s, http.MethodPost, "/api/v1/quiesce", nil) }()
	group.Wait()
	if res := requestJSON(t, s, http.MethodPost, "/api/v1/start", nil); res.Code != 409 {
		t.Fatalf("admission reopened: %d", res.Code)
	}
}

func TestCompletedOrInterruptedDeploymentRequiresRevalidation(t *testing.T) {
	for _, status := range []string{"completed", "prepared", "failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			s := testServer(t)
			requestJSON(t, s, http.MethodPost, "/api/v1/validate", validPlan())
			s.state.Status = status
			if res := requestJSON(t, s, http.MethodPost, "/api/v1/start", map[string]bool{"apply": false}); res.Code != 409 {
				t.Fatalf("replayed deployment: %d", res.Code)
			}
		})
	}
}

func TestFailureKeepsRunReservedUntilCleanupFinishes(t *testing.T) {
	s := testServer(t)
	s.running = true
	s.state.Running = true
	s.fail("injected deployment error")
	if !s.running || !s.state.Running {
		t.Fatal("failure released run before password cleanup")
	}
	s.finishRun(true)
	if s.running || s.state.Running {
		t.Fatal("completed cleanup retained running flag")
	}
}
