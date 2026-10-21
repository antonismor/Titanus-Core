package webinstaller

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"io"
	"syscall"

	"encoding/pem"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/fencing"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const tlsRenewBefore = 7 * 24 * time.Hour

func localTLSNames() ([]string, []net.IP) {
	names := []string{"localhost"}
	if host, err := os.Hostname(); err == nil && host != "" {
		names = append(names, host)
	}
	ips := []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
	if interfaces, err := net.Interfaces(); err == nil {
		for _, iface := range interfaces {
			if iface.Flags&net.FlagUp == 0 {
				continue
			}
			addrs, _ := iface.Addrs()
			for _, addr := range addrs {
				ip, _, err := net.ParseCIDR(addr.String())
				if err == nil && !ip.IsUnspecified() && !ip.IsMulticast() {
					ips = append(ips, ip)
				}
			}
		}
	}
	return uniqueStrings(names), ips
}

func tlsRegular(path string, private bool) error {
	s, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !s.Mode().IsRegular() || (private && (s.Mode().Perm()&0077 != 0 || s.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()))) {
		return fmt.Errorf("TLS files must be regular, with private keys mode 0600: %s", path)
	}
	return nil
}

func tlsKeyPairFiles(certPath, keyPath string) (*x509.Certificate, error) {
	if err := tlsRegular(certPath, false); err != nil {
		return nil, err
	}
	if err := tlsRegular(keyPath, true); err != nil {
		return nil, err
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil || len(pair.Certificate) == 0 {
		return nil, fmt.Errorf("invalid Web Installer TLS identity")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	now := time.Now()
	if err != nil || now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return nil, fmt.Errorf("invalid, not-yet-valid or expired Web Installer certificate")
	}
	return cert, nil
}

func newTLSKey() (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func tlsSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic(err)
	}
	return n.Add(n, big.NewInt(1))
}

// The CA survives leaf renewal and IP changes, so clients enroll trust once.
// A broken/existing CA is never silently replaced and trust is never bypassed.
func ensureTLSCA(dir string, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	crt, keyPath := filepath.Join(dir, "https-ca.crt"), filepath.Join(dir, "https-ca.key")
	_, ce := os.Lstat(crt)
	_, ke := os.Lstat(keyPath)
	if os.IsNotExist(ce) && os.IsNotExist(ke) {
		key, kpem, err := newTLSKey()
		if err != nil {
			return nil, nil, err
		}
		tpl := &x509.Certificate{SerialNumber: tlsSerial(), Subject: pkix.Name{CommonName: "Titanus Installer Local CA " + filepath.Base(dir)}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
		der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
		if err != nil {
			return nil, nil, err
		}
		if err = atomicWrite(keyPath, kpem, 0600); err != nil {
			return nil, nil, err
		}
		if err = atomicWrite(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
			return nil, nil, err
		}
	}
	ca, err := tlsKeyPairFiles(crt, keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("installer CA needs explicit recovery: %w", err)
	}
	if !ca.IsCA || !ca.BasicConstraintsValid || ca.KeyUsage&x509.KeyUsageCertSign == 0 || now.Add(90*24*time.Hour).After(ca.NotAfter) || ca.CheckSignatureFrom(ca) != nil {
		return nil, nil, fmt.Errorf("invalid or expiring installer CA; explicit trust rotation required")
	}
	pair, err := tls.LoadX509KeyPair(crt, keyPath)
	if err != nil {
		return nil, nil, err
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("unsupported installer CA key")
	}
	return ca, key, nil
}

func verifyTLSNames(cert *x509.Certificate, pair tls.Certificate, roots *x509.CertPool, names []string, ips []net.IP, now time.Time) error {
	intermediate := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		c, err := x509.ParseCertificate(der)
		if err != nil {
			return err
		}
		intermediate.AddCert(c)
	}
	for _, ip := range ips {
		names = append(names, ip.String())
	}
	for _, name := range uniqueStrings(names) {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediate, DNSName: name, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			return fmt.Errorf("TLS identity is not valid for %s: %w", name, err)
		}
	}
	return nil
}

func ensureLocalTLS(dir string, names []string, ips []net.IP, now time.Time) (string, string, error) {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("TLS state must be a real directory")
	}
	fd, err := syscall.Open(filepath.Join(dir, ".https.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return "", "", err
	}
	lock := os.NewFile(uintptr(fd), "tls-lock")
	defer lock.Close()
	st, err := lock.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return "", "", fmt.Errorf("unsafe TLS identity lock")
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return "", "", err
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)

	ca, key, err := ensureTLSCA(dir, now)
	if err != nil {
		return "", "", err
	}
	crt, keyPath := filepath.Join(dir, "https.crt"), filepath.Join(dir, "https.key")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if cert, err := tlsKeyPairFiles(crt, keyPath); err == nil && now.Add(tlsRenewBefore).Before(cert.NotAfter) {
		pair, _ := tls.LoadX509KeyPair(crt, keyPath)
		if verifyTLSNames(cert, pair, roots, append([]string{}, names...), ips, now) == nil {
			return crt, keyPath, nil
		}
	}
	// Refuse redirected destinations even when replacing an old self-signed leaf.
	for _, p := range []string{crt, keyPath} {
		if _, err := os.Lstat(p); err == nil {
			if err = tlsRegular(p, p == keyPath); err != nil {
				return "", "", err
			}
		} else if !os.IsNotExist(err) {
			return "", "", err
		}
	}
	leaf, kpem, err := newTLSKey()
	if err != nil {
		return "", "", err
	}
	tpl := &x509.Certificate{SerialNumber: tlsSerial(), Subject: pkix.Name{CommonName: "Titanus Core Web Installer"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(90 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, DNSNames: uniqueStrings(names), IPAddresses: ips}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &leaf.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	if err = atomicWrite(keyPath, kpem, 0600); err != nil {
		return "", "", err
	}
	if err = atomicWrite(crt, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		return "", "", err
	}
	return crt, keyPath, nil
}

func ensureTLSIdentity(dir string) (string, string, error) {
	names, ips := localTLSNames()
	return ensureLocalTLS(dir, names, ips, time.Now().UTC())
}

func tlsIdentity(cfg Config) (string, string, string, error) {
	if cfg.TLSCert == "" && cfg.TLSKey == "" && cfg.TLSCA == "" {
		data, err := fencing.ReadPrivateFile("/etc/titanus/installer-tls.json")
		if err == nil {
			var selected struct {
				Certificate string `json:"certificate"`
				Key         string `json:"key"`
				CA          string `json:"ca"`
			}
			d := json.NewDecoder(bytes.NewReader(data))
			d.DisallowUnknownFields()
			if err = d.Decode(&selected); err != nil || d.Decode(new(any)) != io.EOF {
				return "", "", "", fmt.Errorf("invalid private installer TLS settings")
			}
			cfg.TLSCert, cfg.TLSKey, cfg.TLSCA = selected.Certificate, selected.Key, selected.CA
			if cfg.TLSCert == "" || cfg.TLSKey == "" || cfg.TLSCA == "" {
				return "", "", "", fmt.Errorf("complete installer TLS settings required")
			}
		} else if !os.IsNotExist(err) {
			return "", "", "", err
		}
	}

	if cfg.TLSCert == "" && cfg.TLSKey == "" && cfg.TLSCA == "" {
		c, k, e := ensureTLSIdentity(cfg.StateDir)
		return c, k, filepath.Join(cfg.StateDir, "https-ca.crt"), e
	}
	if cfg.TLSCert == "" || cfg.TLSKey == "" || cfg.TLSCA == "" {
		return "", "", "", fmt.Errorf("custom TLS requires certificate chain, private key and trusted issuer CA files")
	}
	cert, err := tlsKeyPairFiles(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return "", "", "", err
	}
	if err = tlsRegular(cfg.TLSCA, false); err != nil {
		return "", "", "", err
	}
	data, err := os.ReadFile(cfg.TLSCA)
	if err != nil {
		return "", "", "", err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return "", "", "", fmt.Errorf("invalid configured TLS CA")
	}
	pair, _ := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	names, ips := localTLSNames()
	if err = verifyTLSNames(cert, pair, roots, names, ips, time.Now()); err != nil {
		return "", "", "", err
	}
	return cfg.TLSCert, cfg.TLSKey, cfg.TLSCA, nil
}

type TLSDetails struct {
	Certificate   string    `json:"certificate"`
	CAFile        string    `json:"ca_file"`
	CAFingerprint string    `json:"ca_sha256"`
	Addresses     []string  `json:"addresses"`
	Expires       time.Time `json:"expires"`
	CustomCA      bool      `json:"custom_ca"`
}

func PrepareTLS(cfg Config) (TLSDetails, error) {
	var result TLSDetails
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return result, err
	}
	c, selectedKey, ca, err := tlsIdentity(cfg)
	if err != nil {
		return result, err
	}
	data, err := os.ReadFile(ca)
	if err != nil {
		return result, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return result, fmt.Errorf("invalid issuer CA")
	}
	digest := sha256.Sum256(block.Bytes)
	cert, err := tlsKeyPairFiles(c, selectedKey)
	if err != nil {
		return result, err
	}
	addresses := append([]string{}, cert.DNSNames...)
	for _, ip := range cert.IPAddresses {
		addresses = append(addresses, ip.String())
	}
	sort.Strings(addresses)
	return TLSDetails{Certificate: c, CAFile: ca, CAFingerprint: hex.EncodeToString(digest[:]), Addresses: addresses, Expires: cert.NotAfter, CustomCA: ca != filepath.Join(cfg.StateDir, "https-ca.crt")}, nil
}

// EnrollLocalCATrust is used only by the explicit launcher preparation command.
// It establishes trust on the installer host; remote browser trust remains a
// client/organization policy and cannot be granted by an HTTPS server.
func EnrollLocalCATrust(info TLSDetails) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("local CA enrollment requires root")
	}
	data, err := os.ReadFile(info.CAFile)
	if err != nil {
		return err
	}
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return fmt.Errorf("local trust enrollment requires one issuer CA certificate")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !ca.IsCA || ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		return fmt.Errorf("invalid issuer CA for local trust")
	}
	dir := "/usr/local/share/ca-certificates"
	if err = os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	digest := sha256.Sum256(block.Bytes)
	target := filepath.Join(dir, fmt.Sprintf("titanus-installer-%x.crt", digest))
	if old, err := os.ReadFile(target); err == nil {
		if err = tlsRegular(target, false); err != nil {
			return err
		}
		if !bytes.Equal(old, data) {
			return fmt.Errorf("local issuer CA identity collision")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		if err = atomicWrite(target, data, 0644); err != nil {
			return err
		}
	}
	if _, err = exec.Command("update-ca-certificates").CombinedOutput(); err != nil {
		return fmt.Errorf("cannot enroll installer CA in the local trust store: %w", err)
	}
	return nil
}

func tlsCertificateLoader(cfg Config) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	var mu sync.Mutex
	var current *tls.Certificate
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		mu.Lock()
		defer mu.Unlock()
		c, k, _, err := tlsIdentity(cfg)
		if err != nil {
			return nil, err
		}
		pair, err := tls.LoadX509KeyPair(c, k)
		if err != nil {
			return nil, err
		}
		current = &pair
		return current, nil
	}
}
