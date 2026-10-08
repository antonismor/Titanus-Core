package identity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/antonismor/Titanus-Core/internal/durable"
)

// RenewCSR preserves the authenticated certificate's role, identity and SANs.
// The client owns the replacement private key; no CA private key leaves the
// controller. The CSR cannot expand privileges or impersonate another server.
func (a Authority) RenewCSR(current *x509.Certificate, request []byte) ([]byte, error) {
	return a.RenewCSRWithSerial(current, request, randomSerial())
}

// RenewCSRWithSerial is used with a quorum-committed issuance sequence.
func (a Authority) RenewCSRWithSerial(current *x509.Certificate, request []byte, serial *big.Int) ([]byte, error) {
	if serial == nil || serial.Sign() <= 0 {
		return nil, fmt.Errorf("positive certificate serial required")
	}
	principal, err := CertificatePrincipal(current)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(request)
	if block == nil {
		return nil, fmt.Errorf("invalid CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, err
	}
	if csr.Subject.CommonName != principal.ID {
		return nil, fmt.Errorf("CSR identity does not match client")
	}
	ca, key, err := a.load()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: principal.ID, Organization: []string{"Titanus", a.Realm}, OrganizationalUnit: []string{string(principal.Role)}}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: append([]x509.ExtKeyUsage(nil), current.ExtKeyUsage...), DNSNames: append([]string(nil), current.DNSNames...), IPAddresses: current.IPAddresses}
	if template.NotAfter.After(ca.NotAfter) {
		template.NotAfter = ca.NotAfter
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, csr.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

func loadPair(certPath, keyPath string) (tls.Certificate, error) {
	if filepath.Dir(certPath) != filepath.Dir(keyPath) {
		return tls.Certificate{}, fmt.Errorf("certificate and key must use the same bundle directory")
	}
	cert, err := filepath.EvalSymlinks(certPath)
	if err != nil {
		return tls.Certificate{}, err
	}
	// Pin the immutable generation once, so a simultaneous current-pointer
	// swap cannot mix one generation's certificate with another's private key.
	return tls.LoadX509KeyPair(cert, filepath.Join(filepath.Dir(cert), filepath.Base(keyPath)))
}

// RenewIfNeeded rotates the leaf/key when less than eight hours remain. Flat
// installations migrate once to a versioned bundle; subsequent rotation swaps
// only its shared current pointer. In-flight authenticated requests complete.
func RenewIfNeeded(client *http.Client, endpoint, caPath, certPath, keyPath string) error {
	current, err := loadCertificate(certPath)
	if err != nil {
		return err
	}
	if time.Until(current.NotAfter) > 8*time.Hour {
		return nil
	}
	if discovery, ok := client.Transport.(interface {
		Discover(context.Context, string) error
	}); ok {
		ctx, cancel := context.WithTimeout(context.Background(), client.Timeout)
		if client.Timeout <= 0 {
			cancel()
			ctx, cancel = context.WithCancel(context.Background())
		}
		defer cancel()
		if err := discovery.Discover(ctx, endpoint); err != nil {
			return err
		}
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: current.Subject.CommonName}}, private)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(struct {
		CSR string `json:"csr"`
	}{string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))})
	if err != nil {
		return err
	}
	response, err := client.Post(endpoint+"/v1/identity/renew", "application/json", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("certificate renewal: %s", response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return fmt.Errorf("invalid renewed certificate")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	if err := ValidatePeer(leaf, caPath); err != nil {
		return err
	}
	before, err := CertificatePrincipal(current)
	if err != nil {
		return err
	}
	after, err := CertificatePrincipal(leaf)
	if err != nil || before != after {
		return fmt.Errorf("renewal changed identity or role")
	}
	issued, ok := leaf.PublicKey.(ed25519.PublicKey)
	if !ok || !bytes.Equal(issued, public) {
		return fmt.Errorf("renewal does not match replacement key")
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return err
	}
	return installBundle(certPath, keyPath, leaf.SerialNumber.Text(16), data, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
}

func installBundle(certPath, keyPath, version string, cert, key []byte) error {
	if filepath.Dir(certPath) != filepath.Dir(keyPath) {
		return fmt.Errorf("certificate and key must share a directory")
	}
	parent := filepath.Dir(certPath)
	// Resolve an existing current directory before creating the rotation store;
	// otherwise a previously configured current path would nest generations.
	if filepath.Base(parent) == "current" {
		parent = filepath.Dir(parent)
	}
	root := filepath.Join(parent, "rotations")
	generation := filepath.Join(root, version)
	if err := os.MkdirAll(generation, 0700); err != nil {
		return err
	}
	if err := durable.WriteFile(filepath.Join(generation, filepath.Base(certPath)), cert, 0644); err != nil {
		return err
	}
	if err := durable.WriteFile(filepath.Join(generation, filepath.Base(keyPath)), key, 0600); err != nil {
		return err
	}
	if err := swapLink(filepath.Join(root, "current"), version); err != nil {
		return err
	}
	// Fixed configured paths each reference the common pointer, never separate
	// mutable certificate/key contents. For current-based issuance paths, replace
	// their current pointer directly with the rotation generation.
	if filepath.Base(filepath.Dir(certPath)) == "current" {
		return swapLink(filepath.Dir(certPath), filepath.Join("rotations", version))
	}
	for _, path := range []string{certPath, keyPath} {
		if err := swapLink(path, filepath.Join("rotations", "current", filepath.Base(path))); err != nil {
			return err
		}
	}
	return nil
}

func swapLink(path, target string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".link-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_ = f.Close()
	_ = os.Remove(tmp)
	defer os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
