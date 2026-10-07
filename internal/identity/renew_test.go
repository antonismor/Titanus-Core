package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentRenewalRotatesKeyAndPreservesPermissions(t *testing.T) {
	auth, err := InitAuthority(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	serverCert, serverKey, err := auth.Issue("controller", []string{"127.0.0.1"}, RoleController, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath, err := auth.Issue("worker", nil, RoleNode, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise migration of installed flat certificate/key paths as well.
	flat := t.TempDir()
	for name, path := range map[string]string{"node.crt": certPath, "node.key": keyPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(flat, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	certPath = filepath.Join(flat, "node.crt")
	keyPath = filepath.Join(flat, "node.key")
	old, err := loadCertificate(certPath)
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
	server := &http.Server{Handler: Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			CSR string `json:"csr"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		data, err := auth.RenewCSR(r.TLS.PeerCertificates[0], []byte(req.CSR))
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		w.Write(data)
	}), auth.CertPath)}
	go server.Serve(listener)
	defer server.Close()
	clientTLS, err := TLSConfig(auth.CertPath, certPath, keyPath, false)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: clientTLS, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	if err := RenewIfNeeded(client, "https://"+listener.Addr().String(), auth.CertPath, certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	leaf, err := loadCertificate(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.SerialNumber.Cmp(old.SerialNumber) == 0 || leaf.PublicKey.(ed25519.PublicKey).Equal(old.PublicKey) {
		t.Fatal("automatic renewal did not rotate key/serial")
	}
	if principal, err := CertificatePrincipal(leaf); err != nil || principal.Role != RoleNode || principal.ID != "worker" {
		t.Fatal("renewal expanded identity")
	}
	if time.Until(leaf.NotAfter) < 23*time.Hour {
		t.Fatal("renewal did not extend lifetime")
	}
	if _, err := loadPair(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	// A second call has nothing to renew and performs no request.
	if err := RenewIfNeeded(client, "invalid", auth.CertPath, certPath, keyPath); err != nil {
		t.Fatal(err)
	}
}

func TestCSRDoesNotEscalateRoleIdentityOrSAN(t *testing.T) {
	auth, err := InitAuthority(t.TempDir(), "test")
	if err != nil {
		t.Fatal(err)
	}
	certPath, _, err := auth.Issue("worker", []string{"worker.example"}, RoleNode, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := loadCertificate(certPath)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"worker", "another-node"} {
		csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: id, OrganizationalUnit: []string{"admin"}}, DNSNames: []string{"controller.example"}}, key)
		if err != nil {
			t.Fatal(err)
		}
		data, err := auth.RenewCSR(leaf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))
		if id != "worker" {
			if err == nil {
				t.Fatal("CSR changed identity")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(data)
		renewed, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if renewed.Subject.OrganizationalUnit[0] != "node" || len(renewed.DNSNames) != 2 || renewed.DNSNames[1] != "worker.example" {
			t.Fatalf("CSR expanded authority: %+v", renewed)
		}
	}
}
