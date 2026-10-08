package identity

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
)

type Role string

const (
	RoleNode       Role = "node"
	RoleController Role = "controller"
	RoleAdmin      Role = "admin"
)

var identityName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type Principal struct {
	ID   string
	Role Role
}
type principalKey struct{}

func CertificatePrincipal(cert *x509.Certificate) (Principal, error) {
	if cert == nil || !identityName.MatchString(cert.Subject.CommonName) || len(cert.Subject.OrganizationalUnit) != 1 {
		return Principal{}, fmt.Errorf("certificate has no explicit Titanus role")
	}
	role := Role(cert.Subject.OrganizationalUnit[0])
	if role != RoleNode && role != RoleController && role != RoleAdmin {
		return Principal{}, fmt.Errorf("unknown certificate role")
	}
	return Principal{ID: cert.Subject.CommonName, Role: role}, nil
}

func RequestPrincipal(r *http.Request) (Principal, bool) {
	p, ok := r.Context().Value(principalKey{}).(Principal)
	return p, ok
}

// LocalManagement is used only by the privileged Unix socket listener. A plain
// TCP request or a nil TLS field never implies administrator authorization.
func LocalManagement(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, Principal{ID: "local", Role: RoleAdmin})))
	})
}

// Authenticate re-checks certificate validity and the signed CRL on every
// request, including reused HTTP/1.1 and HTTP/2 connections after revocation.
func Authenticate(next http.Handler, caPath string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
			http.Error(w, "verified client certificate required", http.StatusUnauthorized)
			return
		}
		cert := r.TLS.PeerCertificates[0]
		if err := ValidatePeer(cert, caPath); err != nil {
			http.Error(w, "client certificate is not authorized", http.StatusUnauthorized)
			return
		}
		principal, err := CertificatePrincipal(cert)
		if err != nil {
			http.Error(w, "explicit Titanus role required", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Scheme != "https" || parsed.Host != r.Host {
				http.Error(w, "cross-origin request denied", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal)))
	})
}

func Allowed(p Principal, method, path string) bool {
	if p.Role == RoleAdmin {
		return true
	}
	if method == http.MethodGet && (path == "/v1/metrics" || path == "/v1/diagnostics" || path == "/v1/events") {
		return p.Role == RoleController
	}
	if method == http.MethodGet && (path == "/v1/health" || path == "/v1/version" || path == "/v1/realm/state" || path == "/v1/identity/crl") {
		return p.Role == RoleNode || p.Role == RoleController
	}
	if method == http.MethodPost && (path == "/v1/realm/pulse" || path == "/v1/realm/nodes" || path == "/v1/identity/renew") {
		return p.Role == RoleNode || p.Role == RoleController
	}
	if p.Role == RoleController {
		return len(path) > len("/v1/node/") && path[:len("/v1/node/")] == "/v1/node/" || method == http.MethodGet && len(path) > len("/v1/realm/") && path[:len("/v1/realm/")] == "/v1/realm/"
	}
	return false
}
