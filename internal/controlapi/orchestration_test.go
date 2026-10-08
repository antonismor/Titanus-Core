package controlapi

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/observe"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOrchestrationAuthorizationAuditAndNoSecretExport(t *testing.T) {
	root := t.TempDir()
	store, _ := realm.Open(root, "LAB")
	rec, e := observe.New(root)
	if e != nil {
		t.Fatal(e)
	}
	keys := secrets.Keyring{Active: "one", Keys: map[string][]byte{"one": bytes.Repeat([]byte{1}, 32)}}
	keyPath := filepath.Join(root, "keys")
	raw, _ := json.Marshal(keys)
	os.WriteFile(keyPath, raw, 0600)
	api := New(store, nil, nil, nil)
	api.Observations = rec
	api.SecretKeyring = keyPath
	mux := http.NewServeMux()
	api.Register(mux)
	local := identity.LocalManagement(mux)
	req := httptest.NewRequest("PUT", "/v1/realm/secrets/db", strings.NewReader(`{"value":"QVBJX1NFQ1JFVF9DQU5BUlk="}`))
	w := httptest.NewRecorder()
	local.ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatal(w.Body.String())
	}
	for _, path := range []string{"/v1/realm/secrets", "/v1/realm/state", "/v1/events"} {
		w = httptest.NewRecorder()
		local.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || strings.Contains(w.Body.String(), "API_SECRET_CANARY") {
			t.Fatalf("leak at %s: %s", path, w.Body.String())
		}
	}
	auth, e := identity.InitAuthority(t.TempDir(), "LAB")
	if e != nil {
		t.Fatal(e)
	}
	handler := identity.Authenticate(mux, auth.CertPath)
	for _, role := range []identity.Role{identity.RoleNode, identity.RoleController, identity.RoleAdmin} {
		cp, _, e := auth.Issue("actor", nil, role, time.Hour)
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := os.ReadFile(cp)
		block, _ := pem.Decode(raw)
		cert, _ := x509.ParseCertificate(block.Bytes)
		for _, path := range []string{"/command-center", "/command-center/app.js", "/v1/realm/tasks", "/v1/realm/secrets", "/v1/realm/autoscalers"} {
			req := httptest.NewRequest("GET", path, nil)
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			allowed := role == identity.RoleAdmin || role == identity.RoleController && strings.HasPrefix(path, "/v1/realm/")
			if (w.Code == 200) != allowed {
				t.Fatalf("role %s path %s code %d", role, path, w.Code)
			}
			if path == "/command-center" && allowed {
				if w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("missing browser policy")
				}
			}
		}
		req = httptest.NewRequest("POST", "/v1/realm/tasks", strings.NewReader(`{"name":"one","template":{"source":"app","command":["app"]}}`))
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		w = httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if (w.Code == 201) != (role == identity.RoleAdmin) {
			t.Fatal("Task role mutation bypass")
		}
		if role == identity.RoleNode {
			w = httptest.NewRecorder()
			req = httptest.NewRequest("GET", "/v1/realm/state", nil)
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
			handler.ServeHTTP(w, req)
			if strings.Contains(w.Body.String(), "ciphertext") {
				t.Fatal("node received vault ciphertext")
			}
		}
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/command-center", nil))
	if w.Code != 403 {
		t.Fatal("anonymous Command Center")
	}
	// API mutation must not enter the handler when durable intent fails.
	api.Observations = &observe.Recorder{Dir: filepath.Join(root, "missing")}
	w = httptest.NewRecorder()
	local.ServeHTTP(w, httptest.NewRequest("POST", "/v1/realm/tasks", strings.NewReader(`{"name":"unaudited","template":{"source":"app","command":["app"]}}`)))
	if w.Code != 503 {
		t.Fatal("audit failure admitted mutation")
	}
	if _, ok := store.Snapshot().Tasks["unaudited"]; ok {
		t.Fatal("unaudited Task created")
	}
}
