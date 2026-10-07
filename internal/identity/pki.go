package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
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
	auth := Authority{
		Dir: dir, Realm: realmName,
		CertPath: filepath.Join(dir, "ca.crt"),
		KeyPath: filepath.Join(dir, "ca.key"),
	}
	if _, err := os.Stat(auth.CertPath); err == nil {
		if _, keyErr := os.Stat(auth.KeyPath); keyErr == nil {
			return auth, nil
		}
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
	return auth, nil
}

func (a Authority) IssueNode(nodeID string, addresses []string) (certPath, keyPath string, err error) {
	if nodeID == "" {
		return "", "", fmt.Errorf("node ID is required")
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
			CommonName:   nodeID,
			Organization: []string{"Titanus", a.Realm},
		},
		NotBefore:   now.Add(-5 * time.Minute),
		NotAfter:    now.AddDate(2, 0, 0),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{nodeID},
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
	nodeDir := filepath.Join(a.Dir, "nodes", nodeID)
	if err := os.MkdirAll(nodeDir, 0700); err != nil {
		return "", "", err
	}
	certPath = filepath.Join(nodeDir, "node.crt")
	keyPath = filepath.Join(nodeDir, "node.key")
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
	return certPath, keyPath, nil
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
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	}
	if server {
		cfg.ClientCAs = pool
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
	} else {
		cfg.RootCAs = pool
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
	return cert, key, nil
}

func writePEM(path, typ string, bytes []byte, mode os.FileMode) error {
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: bytes}), mode)
}

func randomSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 120)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}
