package controlapi

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/lease"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
)

func TestAPIRolesAndNodeIdentityScoping(t *testing.T) {
	auth, err := identity.InitAuthority(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	store, err := realm.Open(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	New(store, nil, nil, nil).Register(mux)
	handler := identity.Authenticate(mux, auth.CertPath)
	for _, role := range []identity.Role{identity.RoleNode, identity.RoleController, identity.RoleAdmin} {
		certPath, _, err := auth.Issue("worker", nil, role, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(certPath)
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(data)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			method, path, body string
			allowed            bool
		}{
			{"GET", "/v1/realm/state", "", true},
			{"GET", "/v1/realm/fleets", "", role != identity.RoleNode},
			{"POST", "/v1/realm/fleets", "{}", role == identity.RoleAdmin},
 {"POST", "/v1/realm/fleets/web/rollback", "{}", role == identity.RoleAdmin},
			{"POST", "/v1/realm/pulse", `{"node_id":"other"}`, role == identity.RoleAdmin},
			{"POST", "/v1/realm/nodes", `{"id":"worker","capabilities":["CONTROL"],"address":"127.0.0.1"}`, role != identity.RoleNode},
		} {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if (response.Code != http.StatusForbidden) != tc.allowed {
				t.Fatalf("role=%s %s %s: code=%d body=%s", role, tc.method, tc.path, response.Code, response.Body.String())
			}
		}
	}
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("GET", "/v1/realm/state", nil))
	if response.Code != http.StatusForbidden {
		t.Fatal("unwrapped/nil-TLS request became administrator")
	}
	response = httptest.NewRecorder()
	identity.LocalManagement(mux).ServeHTTP(response, httptest.NewRequest("GET", "/v1/realm/state", nil))
	if response.Code != http.StatusOK {
		t.Fatal("privileged Unix management unavailable")
	}
}

func TestControllerStartRequiresLiveLease(t *testing.T) {
	auth, err := identity.InitAuthority(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	path, _, err := auth.Issue("control", nil, identity.RoleController, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	leases := lease.NewManager(nil)
	defer leases.Close()
	mux := http.NewServeMux()
	New(nil, unitruntime.NewManager(unitruntime.Config{StateRoot: t.TempDir()}), nil, leases).Register(mux)
	handler := identity.Authenticate(mux, auth.CertPath)
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/node/units/missing/start", nil)
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	if response := request(); !strings.Contains(response.Body.String(), "live lease required") {
		t.Fatal("controller start bypassed lease fencing")
	}
	if _, err := leases.Renew("missing", "token", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if response := request(); strings.Contains(response.Body.String(), "live lease required") {
		t.Fatal("valid lease was rejected")
	}
}
