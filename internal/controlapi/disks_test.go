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

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/identity"
)

func TestDiskAPIAdminAndMaintenanceOwnership(t *testing.T) {
	auth, err := identity.InitAuthority(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	d := disk.NewManager(t.TempDir())
	if _, err = d.Create(disk.Spec{Name: "data", SizeBytes: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	s := New(nil, nil, nil, nil)
	s.Disks = d
	mux := http.NewServeMux()
	s.Register(mux)
	handler := identity.Authenticate(mux, auth.CertPath)
	request := func(role identity.Role, method, path, body string) int {
		certPath, _, err := auth.Issue("identity-"+string(role), nil, role, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(certPath)
		block, _ := pem.Decode(raw)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response.Code
	}
	for _, role := range []identity.Role{identity.RoleNode, identity.RoleController} {
		if status := request(role, "POST", "/v1/node/disks/data/snapshot", `{"name":"denied"}`); status != 403 {
			t.Fatal("storage privilege escalation", role, status)
		}
	}
	if status := request(identity.RoleController, "GET", "/v1/node/disks/data", ""); status != 200 {
		t.Fatal("controller cannot inspect", status)
	}
	a, err := d.Acquire("data", "writer", "run")
	if err != nil {
		t.Fatal(err)
	}
	if status := request(identity.RoleAdmin, "POST", "/v1/node/disks/data/snapshot", `{"name":"blocked"}`); status != 409 {
		t.Fatal("admin bypassed live ownership", status)
	}
	a.Close()
	if status := request(identity.RoleAdmin, "POST", "/v1/node/disks/data/snapshot", `{"name":"offline"}`); status != 201 {
		t.Fatal("offline snapshot failed", status)
	}
	if status := request(identity.RoleAdmin, "POST", "/v1/node/disks/data/restore", `{"snapshot":"offline","target":"restored"}`); status != 201 {
		t.Fatal("restore failed", status)
	}
	if status := request(identity.RoleAdmin, "POST", "/v1/node/disks/data/restore", `{"snapshot":"offline","target":"restored"}`); status != 409 {
		t.Fatal("restore overwrote data", status)
	}
}
