package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type Authority struct {
	Dir      string
	Realm    string
	CertPath string
	KeyPath  string
}

func InitAuthority(dir, realmName string) (Authority, error) {
	if realmName == "" {
		return Authority{}, fmt.Errorf("Realm name is required")
	}
	if dir == "" {
		return Authority{}, fmt.Errorf("identity directory is required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return Authority{}, err
	}
	bootstrap, err := os.OpenFile(filepath.Join(dir, "bootstrap.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return Authority{}, err
	}
	defer bootstrap.Close()
	if err := syscall.Flock(int(bootstrap.Fd()), syscall.LOCK_EX); err != nil {
		return Authority{}, err
	}
	defer syscall.Flock(int(bootstrap.Fd()), syscall.LOCK_UN)
	auth := Authority{
		Dir: dir, Realm: realmName,
		CertPath: filepath.Join(dir, "ca.crt"),
		KeyPath:  filepath.Join(dir, "ca.key"),
	}
	if _, err := os.Stat(auth.CertPath); err == nil {
		if _, keyErr := os.Stat(auth.KeyPath); keyErr != nil {
			return Authority{}, fmt.Errorf("existing CA has no key: %w", keyErr)
		}
		cert, _, err := auth.load()
		if err != nil {
			return Authority{}, err
		}
		if cert.Subject.CommonName != "Titanus Realm CA: "+realmName {
			return Authority{}, fmt.Errorf("existing CA belongs to a different Realm")
		}
		if _, err := os.Stat(filepath.Join(dir, "ca.crl")); os.IsNotExist(err) {
			if _, err := auth.Revoke(nil); err != nil {
				return Authority{}, err
			}
		}
		return auth, nil
	} else if !os.IsNotExist(err) {
		return Authority{}, err
	}

	if _, err := os.Stat(auth.KeyPath); err == nil {
		return Authority{}, fmt.Errorf("CA key exists without its certificate; refusing replacement")
	} else if !os.IsNotExist(err) {
		return Authority{}, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Authority{}, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "Titanus Realm CA: " + realmName, Organization: []string{"Titanus"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		return Authority{}, err
	}
	if err := writePEM(auth.CertPath, "CERTIFICATE", der, 0644); err != nil {
		return Authority{}, err
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return Authority{}, err
	}
	if err := writePEM(auth.KeyPath, "PRIVATE KEY", keyBytes, 0600); err != nil {
		return Authority{}, err
	}
	if _, err := auth.Revoke(nil); err != nil {
		return Authority{}, err
	}
	return auth, nil
}

func (a Authority) IssueNode(nodeID string, addresses []string) (certPath, keyPath string, err error) {
	return a.Issue(nodeID, addresses, RoleNode, 24*time.Hour)
}

func (a Authority) Issue(nodeID string, addresses []string, role Role, ttl time.Duration) (certPath, keyPath string, err error) {
	if !identityName.MatchString(nodeID) {
		return "", "", fmt.Errorf("invalid identity ID")
	}
	if role != RoleNode && role != RoleController && role != RoleAdmin {
		return "", "", fmt.Errorf("invalid identity role")
	}
	if ttl < time.Hour || ttl > 30*24*time.Hour {
		return "", "", fmt.Errorf("certificate TTL must be 1h to 30d")
	}
	caCert, caKey, err := a.load()
	if err != nil {
		return "", "", err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject: pkix.Name{
			CommonName:         nodeID,
			Organization:       []string{"Titanus", a.Realm},
			OrganizationalUnit: []string{string(role)},
		},
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.Add(ttl),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{nodeID},
	}
	if template.NotAfter.After(caCert.NotAfter) {
		template.NotAfter = caCert.NotAfter
	}
	if role == RoleAdmin {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	for _, address := range addresses {
		if ip := net.ParseIP(address); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else if address != "" {
			template.DNSNames = append(template.DNSNames, address)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, pub, caKey)
	if err != nil {
		return "", "", err
	}
	nodeDir := filepath.Join(a.Dir, "identities", string(role)+"-"+nodeID)
	versionDir := filepath.Join(nodeDir, template.SerialNumber.Text(16))
	if err := os.MkdirAll(versionDir, 0700); err != nil {
		return "", "", err
	}
	certPath = filepath.Join(versionDir, "node.crt")
	keyPath = filepath.Join(versionDir, "node.key")
	if err := writePEM(certPath, "CERTIFICATE", der, 0644); err != nil {
		return "", "", err
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return "", "", err
	}
	if err := writePEM(keyPath, "PRIVATE KEY", keyBytes, 0600); err != nil {
		return "", "", err
	}
	link, err := os.CreateTemp(nodeDir, ".current-*")
	if err != nil {
		return "", "", err
	}
	linkPath := link.Name()
	_ = link.Close()
	_ = os.Remove(linkPath)
	defer os.Remove(linkPath)
	if err := os.Symlink(filepath.Base(versionDir), linkPath); err != nil {
		return "", "", err
	}
	if err := os.Rename(linkPath, filepath.Join(nodeDir, "current")); err != nil {
		return "", "", err
	}
	parent, err := os.Open(nodeDir)
	if err != nil {
		return "", "", err
	}
	err = parent.Sync()
	_ = parent.Close()
	if err != nil {
		return "", "", err
	}
	return filepath.Join(nodeDir, "current", "node.crt"), filepath.Join(nodeDir, "current", "node.key"), nil
}

func TLSConfig(caPath, certPath, keyPath string, server bool) (*tls.Config, error) {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("unable to parse Titanus CA %s", caPath)
	}
	cert, err := loadPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	leaf, parseErr := x509.ParseCertificate(cert.Certificate[0])
	if parseErr != nil {
		return nil, parseErr
	}
	if err := ValidatePeer(leaf, caPath); err != nil {
		return nil, err
	}
	reload := func() (*tls.Certificate, error) {
		pair, err := loadPair(certPath, keyPath)
		if err != nil {
			return nil, err
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, err
		}
		if err := ValidatePeer(leaf, caPath); err != nil {
			return nil, err
		}
		return &pair, nil
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("missing peer certificate")
			}
			return ValidatePeer(state.PeerCertificates[0], caPath)
		},
	}
	if server {
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.GetCertificate = func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return reload() }
	} else {
		cfg.RootCAs = pool
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return reload() }
	}
	return cfg, nil
}

func (a Authority) load() (*x509.Certificate, ed25519.PrivateKey, error) {
	certPEM, err := os.ReadFile(a.CertPath)
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("invalid CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(a.KeyPath)
	if err != nil {
		return nil, nil, err
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("invalid CA key PEM")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := keyAny.(ed25519.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("Titanus CA key is not Ed25519")
	}
	if !cert.IsCA || !key.Public().(ed25519.PublicKey).Equal(cert.PublicKey) {
		return nil, nil, fmt.Errorf("CA certificate and key do not match")
	}
	now := time.Now()
	if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return nil, nil, fmt.Errorf("CA certificate outside validity window")
	}
	return cert, key, nil
}

func writePEM(path, typ string, bytes []byte, mode os.FileMode) error {
	return durable.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: bytes}), mode)
}

func randomSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 120)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}
