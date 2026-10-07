package identity

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthorityAndNodeCertificate(t *testing.T) {
	dir := t.TempDir()
	auth, err := InitAuthority(dir, "LAB")
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := auth.IssueNode("node01", []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{auth.CertPath, auth.KeyPath, cert, key} {
		if _, err := os.Stat(filepath.Clean(path)); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := TLSConfig(auth.CertPath, cert, key, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatal("expected TLS 1.3")
	}
}
