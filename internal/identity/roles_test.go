package identity

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRolesDenyPrivilegeEscalation(t *testing.T) {
	for _, role := range []Role{RoleNode, RoleController, RoleAdmin} {
		p := Principal{ID: "node", Role: role}
		if !Allowed(p, "GET", "/v1/realm/state") {
			t.Fatal("Fabric state unavailable")
		}
		if Allowed(p, "POST", "/v1/realm/fleets") != (role == RoleAdmin) {
			t.Fatal("desired-state administration rights incorrect")
		}
		if Allowed(p, "POST", "/v1/node/units/unit/start") != (role != RoleNode) {
			t.Fatal("Node role can control workloads")
		}
	}
	for _, ous := range [][]string{nil, {"root"}, {"node", "admin"}} {
		if _, err := CertificatePrincipal(&x509.Certificate{Subject: pkix.Name{CommonName: "node", OrganizationalUnit: ous}}); err == nil {
			t.Fatal("ambiguous or unknown role accepted")
		}
	}
}

func TestCertificateRotationAndRevocationOnExistingConnection(t *testing.T) {
	auth, err := InitAuthority(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	serverCert, serverKey, err := auth.Issue("controller", []string{"127.0.0.1"}, RoleController, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	clientCert, clientKey, err := auth.IssueNode("worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	old, err := loadCertificate(clientCert)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := TLSConfig(auth.CertPath, serverCert, serverKey, true)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tls.Listen("tcp", "127.0.0.1:0", serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }), auth.CertPath)}
	go server.Serve(listener)
	defer server.Close()
	clientTLS, err := TLSConfig(auth.CertPath, clientCert, clientKey, false)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: clientTLS}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	url := "https://" + listener.Addr().String() + "/v1/realm/state"
	get := func(want int) {
		t.Helper()
		response, err := client.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		io.Copy(io.Discard, response.Body)
		if response.StatusCode != want {
			t.Fatalf("response %d, want %d", response.StatusCode, want)
		}
	}
	get(http.StatusOK)
	if _, err := auth.Revoke(old.SerialNumber); err != nil {
		t.Fatal(err)
	}
	get(http.StatusUnauthorized) // keepalive must not bypass current revocations
	if _, _, err := auth.IssueNode("worker", nil); err != nil {
		t.Fatal(err)
	}
	transport.CloseIdleConnections()
	get(http.StatusOK) // reload new certificate/key at the same configured paths
	newLeaf, err := loadCertificate(clientCert)
	if err != nil || newLeaf.SerialNumber.Cmp(old.SerialNumber) == 0 {
		t.Fatal("renewal did not rotate serial")
	}
	if info, err := os.Stat(clientKey); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private key permissions incorrect")
	}
}

func TestSignedCRLRejectsRollbackAndForeignAuthority(t *testing.T) {
	auth, err := InitAuthority(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(auth.Dir, "ca.crl")
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Revoke(nil); err != nil {
		t.Fatal(err)
	}
	if err := InstallCRL(auth.CertPath, old); err == nil {
		t.Fatal("CRL rollback accepted")
	}
	foreign, err := InitAuthority(t.TempDir(), "other")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(foreign.Dir, "ca.crl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := InstallCRL(auth.CertPath, data); err == nil {
		t.Fatal("foreign CRL accepted")
	}
}

func TestPlainTCPDoesNotBecomeLocalAdmin(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unauthenticated handler reached") }), "missing")}
	go server.Serve(listener)
	defer server.Close()
	response, err := http.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatal("plain TCP was trusted")
	}
}
