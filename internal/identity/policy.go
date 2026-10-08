package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Policy is public, replicated PKI state. Signing keys are privately provisioned
// on eligible controllers and are never included in Realm/API state.
type Policy struct {
	CA     string `json:"ca_sha256"`
	CRL    []byte `json:"crl"`
	Issued uint64 `json:"issued"`
}

func (a Authority) Fingerprint() (string, error) {
	ca, _, err := a.load()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(ca.Raw)
	return hex.EncodeToString(sum[:]), nil
}

func (a Authority) InitialPolicy() (Policy, error) {
	fingerprint, err := a.Fingerprint()
	if err != nil {
		return Policy{}, err
	}
	data, err := os.ReadFile(filepath.Join(a.Dir, "ca.crl"))
	if err != nil {
		return Policy{}, err
	}
	if _, err = a.CheckPolicy(Policy{CA: fingerprint, CRL: data}); err != nil {
		return Policy{}, err
	}
	return Policy{CA: fingerprint, CRL: data}, nil
}

func (a Authority) CheckPolicy(p Policy) (*x509.RevocationList, error) {
	fingerprint, err := a.Fingerprint()
	if err != nil {
		return nil, err
	}
	if p.CA != fingerprint {
		return nil, fmt.Errorf("signing authority differs from committed Realm CA")
	}
	ca, _, err := a.load()
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(p.CRL)
	if block == nil {
		return nil, fmt.Errorf("missing committed CRL")
	}
	list, err := x509.ParseRevocationList(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err = list.CheckSignatureFrom(ca); err != nil {
		return nil, err
	}
	if list.Number == nil {
		return nil, fmt.Errorf("CRL sequence required")
	}
	return list, nil
}

// SignPolicy is pure: only the quorum transaction can publish this result.
// The last committed cumulative CRL, never a controller's local copy, is input.
func (a Authority) SignPolicy(p Policy, serial *big.Int) (Policy, error) {
	old, err := a.CheckPolicy(p)
	if err != nil {
		return Policy{}, err
	}
	ca, key, err := a.load()
	if err != nil {
		return Policy{}, err
	}
	entries := append([]x509.RevocationListEntry(nil), old.RevokedCertificateEntries...)
	if serial != nil {
		if serial.Sign() <= 0 {
			return Policy{}, fmt.Errorf("invalid certificate serial")
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
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{Number: new(big.Int).Add(old.Number, big.NewInt(1)), ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(7 * 24 * time.Hour), RevokedCertificateEntries: entries}, ca, key)
	if err != nil {
		return Policy{}, err
	}
	p.CRL = pem.EncodeToMemory(&pem.Block{Type: "X509 CRL", Bytes: der})
	return p, nil
}

func (p Policy) NextSerial() (*big.Int, error) {
	if p.Issued == ^uint64(0) {
		return nil, fmt.Errorf("issuance sequence exhausted")
	}
	ca, err := hex.DecodeString(p.CA)
	if err != nil || len(ca) != 32 {
		return nil, fmt.Errorf("invalid CA identity")
	}
	serial := make([]byte, 20)
	copy(serial, ca[:12])
	serial[0] &= 0x7f
	binary.BigEndian.PutUint64(serial[12:], p.Issued+1)
	return new(big.Int).SetBytes(serial), nil
}

func (p Policy) Revoked(serial *big.Int) bool {
	block, _ := pem.Decode(p.CRL)
	if block == nil {
		return true
	}
	list, err := x509.ParseRevocationList(block.Bytes)
	if err != nil || time.Now().Before(list.ThisUpdate) || !time.Now().Before(list.NextUpdate) {
		return true
	}
	for _, entry := range list.RevokedCertificateEntries {
		if serial.Cmp(entry.SerialNumber) == 0 {
			return true
		}
	}
	return false
}
