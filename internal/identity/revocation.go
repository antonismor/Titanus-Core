package identity

import (
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func loadCertificate(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("invalid certificate PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

func loadCRL(path string, ca *x509.Certificate) (*x509.RevocationList, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("invalid CRL PEM")
	}
	list, err := x509.ParseRevocationList(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := list.CheckSignatureFrom(ca); err != nil {
		return nil, err
	}
	return list, nil
}

func ValidatePeer(cert *x509.Certificate, caPath string) error {
	if cert == nil {
		return fmt.Errorf("missing certificate")
	}
	ca, err := loadCertificate(caPath)
	if err != nil {
		return err
	}
	now := time.Now()
	if now.Before(ca.NotBefore) || !now.Before(ca.NotAfter) {
		return fmt.Errorf("CA outside validity window")
	}
	if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return fmt.Errorf("certificate expired or not yet valid")
	}
	if err := cert.CheckSignatureFrom(ca); err != nil {
		return err
	}
	list, err := loadCRL(filepath.Join(filepath.Dir(caPath), "ca.crl"), ca)
	if err != nil {
		return err
	}
	if now.Before(list.ThisUpdate) || !now.Before(list.NextUpdate) {
		return fmt.Errorf("CRL expired or not yet valid")
	}
	for _, entry := range list.RevokedCertificateEntries {
		if cert.SerialNumber.Cmp(entry.SerialNumber) == 0 {
			return fmt.Errorf("certificate revoked")
		}
	}
	return nil
}

// Revoke issues a cumulative CA-signed CRL. Empty serial refreshes the CRL;
// reissuing a leaf creates a new key and serial, without silently revoking the
// previous leaf before operators have deployed its replacement.
func (a Authority) Revoke(serial *big.Int) (string, error) {
	lock, err := os.OpenFile(filepath.Join(a.Dir, "ca.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ca, key, err := a.load()
	if err != nil {
		return "", err
	}
	path := filepath.Join(a.Dir, "ca.crl")
	number := big.NewInt(1)
	entries := []x509.RevocationListEntry{}
	if _, err := os.Stat(path); err == nil {
		old, err := loadCRL(path, ca)
		if err != nil {
			return "", err
		}
		if old.Number == nil {
			return "", fmt.Errorf("CRL has no sequence number")
		}
		number.Add(old.Number, big.NewInt(1))
		entries = append(entries, old.RevokedCertificateEntries...)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if serial != nil {
		if serial.Sign() <= 0 {
			return "", fmt.Errorf("invalid certificate serial")
		}
		found := false
		for _, entry := range entries {
			if entry.SerialNumber.Cmp(serial) == 0 {
				found = true
			}
		}
		if !found {
			entries = append(entries, x509.RevocationListEntry{SerialNumber: serial, RevocationTime: time.Now().UTC()})
		}
	}
	now := time.Now().UTC()
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{Number: number, ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(7 * 24 * time.Hour), RevokedCertificateEntries: entries}, ca, key)
	if err != nil {
		return "", err
	}
	if err := writePEM(path, "X509 CRL", der, 0644); err != nil {
		return "", err
	}
	return path, nil
}
