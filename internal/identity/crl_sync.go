package identity

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/antonismor/Titanus-Core/internal/durable"
)

// InstallCRL verifies the CA signature, validity window and monotonic sequence
// before atomically replacing a node's local revocation policy.
func InstallCRL(caPath string, data []byte) error {
	if len(data) > 1<<20 {
		return fmt.Errorf("CRL too large")
	}
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(caPath), "ca.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	ca, err := loadCertificate(caPath)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return fmt.Errorf("invalid CRL")
	}
	list, err := x509.ParseRevocationList(block.Bytes)
	if err != nil {
		return err
	}
	if err := list.CheckSignatureFrom(ca); err != nil {
		return err
	}
	now := time.Now()
	if list.Number == nil || now.Before(list.ThisUpdate) || !now.Before(list.NextUpdate) {
		return fmt.Errorf("invalid CRL window or sequence")
	}
	path := filepath.Join(filepath.Dir(caPath), "ca.crl")
	if old, err := loadCRL(path, ca); err == nil {
		if list.Number.Cmp(old.Number) < 0 {
			return fmt.Errorf("CRL rollback rejected")
		}
		if list.Number.Cmp(old.Number) == 0 {
			if !bytes.Equal(list.Raw, old.Raw) {
				return fmt.Errorf("conflicting CRL sequence")
			}
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return durable.WriteFile(path, data, 0644)
}
