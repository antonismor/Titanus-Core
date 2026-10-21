package webinstaller

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTLSChangesSubnetAndRenewsWithoutChangingTrust(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	for _, address := range []string{"192.168.9.20", "172.28.1.42", "10.77.5.6"} {
		crt, key, err := ensureLocalTLS(dir, []string{"localhost", "installer.example.internal"}, []net.IP{net.ParseIP(address), net.ParseIP("127.0.0.1"), net.ParseIP("::1")}, now)
		if err != nil {
			t.Fatal(err)
		}
		rootData, _ := os.ReadFile(filepath.Join(dir, "https-ca.crt"))
		rootKey, _ := os.ReadFile(filepath.Join(dir, "https-ca.key"))
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(rootData) {
			t.Fatal("missing CA")
		}
		pair, err := tls.LoadX509KeyPair(crt, key)
		if err != nil {
			t.Fatal(err)
		}
		cert, _ := x509.ParseCertificate(pair.Certificate[0])
		for _, name := range []string{address, "localhost", "127.0.0.1", "::1", "installer.example.internal"} {
			if _, err = cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: name, CurrentTime: now}); err != nil {
				t.Fatal(name, err)
			}
		}
		if _, err = cert.Verify(x509.VerifyOptions{Roots: x509.NewCertPool(), DNSName: address, CurrentTime: now}); err == nil {
			t.Fatal("unenrolled client unexpectedly trusted local CA")
		}
		// A real HTTPS client must validate both the certificate chain and localhost.
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("verified")) }))
		server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
		server.StartTLS()
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS12}}}
		res, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		client.CloseIdleConnections()
		server.Close()
		later := now.Add(85 * 24 * time.Hour)
		_, _, err = ensureLocalTLS(dir, []string{"localhost", "installer.example.internal"}, []net.IP{net.ParseIP(address)}, later)
		if err != nil {
			t.Fatal(err)
		}
		renewedPair, e := tls.LoadX509KeyPair(crt, key)
		if e != nil {
			t.Fatal(e)
		}
		renewed, e := x509.ParseCertificate(renewedPair.Certificate[0])
		if e != nil {
			t.Fatal(e)
		}
		if !renewed.NotAfter.After(cert.NotAfter) {
			t.Fatal("expiring leaf did not renew")
		}
		caAfter, _ := os.ReadFile(filepath.Join(dir, "https-ca.crt"))
		keyAfter, _ := os.ReadFile(filepath.Join(dir, "https-ca.key"))
		if !bytes.Equal(rootData, caAfter) || !bytes.Equal(rootKey, keyAfter) {
			t.Fatal("leaf renewal rotated client trust")
		}
	}
}

func TestTLSRejectsBrokenAuthorityAndRedirectedKeys(t *testing.T) {
	for _, mode := range []string{"bad-ca", "missing-ca-key", "symlink-leaf", "public-key"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			_, key, err := ensureTLSIdentity(dir)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "bad-ca":
				os.WriteFile(filepath.Join(dir, "https-ca.crt"), []byte("invalid"), 0644)
			case "missing-ca-key":
				os.Remove(filepath.Join(dir, "https-ca.key"))
			case "symlink-leaf":
				os.Remove(key)
				os.Symlink(filepath.Join(dir, "https-ca.key"), key)
			case "public-key":
				os.Chmod(key, 0644)
			}
			if _, _, err = ensureTLSIdentity(dir); err == nil {
				t.Fatal("unsafe TLS state silently accepted or replaced")
			}
		})
	}
}

func TestCustomTLSRequiresCompleteConfigurationAndCorrectSANs(t *testing.T) {
	dir := t.TempDir()
	crt, key, err := ensureTLSIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{StateDir: dir, TLSCert: crt, TLSKey: key, TLSCA: filepath.Join(dir, "https-ca.crt")}
	if _, _, _, err = tlsIdentity(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.TLSCA = ""
	if _, _, _, err = tlsIdentity(cfg); err == nil {
		t.Fatal("custom TLS without pinned issuer accepted")
	}
	bad := t.TempDir()
	crt, key, err = ensureLocalTLS(bad, []string{"wrong.example"}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cfg = Config{StateDir: dir, TLSCert: crt, TLSKey: key, TLSCA: filepath.Join(bad, "https-ca.crt")}
	if _, _, _, err = tlsIdentity(cfg); err == nil {
		t.Fatal("certificate missing local addresses accepted")
	}
}
