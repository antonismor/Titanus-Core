package controlapi

import (
	"bytes"
	"context"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func (s *Server) certificateRevocations(w http.ResponseWriter, r *http.Request) {
	if s.CAPath == "" {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("PKI unavailable"))
		return
	}
	if r.Method == http.MethodPost {
		if s.Authority == nil {
			writeError(w, http.StatusServiceUnavailable, fmt.Errorf("CA signing is available only on the controller"))
			return
		}
		var req struct {
			Serial string `json:"serial"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		var serial *big.Int
		if req.Serial != "" {
			var ok bool
			serial, ok = new(big.Int).SetString(req.Serial, 16)
			if !ok {
				writeError(w, http.StatusBadRequest, fmt.Errorf("serial must be hexadecimal"))
				return
			}
		}
		policy, err := s.Store.MutatePKI(func(p identity.Policy) (identity.Policy, error) {
			if p.CA == "" {
				var e error
				p, e = s.Authority.InitialPolicy()
				if e != nil {
					return p, e
				}
			}
			return s.Authority.SignPolicy(p, serial)
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if err = identity.InstallCRL(s.CAPath, policy.CRL); err != nil {
			writeError(w, 503, err)
			return
		}
	} else if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if policy := s.Store.Snapshot().PKI; policy.CA != "" {
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(policy.CRL)
		return
	}
	file, err := os.Open(filepath.Join(filepath.Dir(s.CAPath), "ca.crl"))
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, file)
}

func (s *Server) certificateRenewal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Authority == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		w.Header().Set("X-Titanus-Rejected", "true")
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("renewal requires an authenticated controller endpoint"))
		return
	}
	var req struct {
		CSR string `json:"csr"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var data []byte
	_, err := s.Store.MutatePKI(func(p identity.Policy) (identity.Policy, error) {
		if p.CA == "" {
			var e error
			p, e = s.Authority.InitialPolicy()
			if e != nil {
				return p, e
			}
		}
		if _, e := s.Authority.CheckPolicy(p); e != nil {
			return p, e
		}
		current := r.TLS.PeerCertificates[0]
		if p.Revoked(current.SerialNumber) {
			return p, fmt.Errorf("current certificate revoked or policy expired")
		}
		serial, e := p.NextSerial()
		if e != nil {
			return p, e
		}
		data, e = s.Authority.RenewCSRWithSerial(current, []byte(req.CSR), serial)
		if e != nil {
			return p, e
		}
		p.Issued++
		return p, nil
	})
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	_, _ = w.Write(data)
}

// MaintainPKI imports/refreshes policy on the leader, and installs committed
// CRLs on every controller. There are no controller-local HA signing writes.
func (s *Server) MaintainPKI(ctx context.Context) {
	interval := time.NewTicker(2 * time.Second)
	defer interval.Stop()
	for {
		if s.Authority != nil && s.Store.CheckLeader() == nil {
			p := s.Store.Snapshot().PKI
			refresh := p.CA == ""
			if !refresh {
				list, e := s.Authority.CheckPolicy(p)
				refresh = e == nil && time.Until(list.NextUpdate) < 48*time.Hour
			}
			if refresh {
				_, err := s.Store.MutatePKI(func(p identity.Policy) (identity.Policy, error) {
					if p.CA == "" {
						return s.Authority.InitialPolicy()
					}
					return s.Authority.SignPolicy(p, nil)
				})
				if err != nil {
					log.Printf("PKI quorum maintenance: %v", err)
				}
			}
		}
		p := s.Store.Snapshot().PKI
		if p.CA != "" {
			old, _ := os.ReadFile(filepath.Join(filepath.Dir(s.CAPath), "ca.crl"))
			if !bytes.Equal(old, p.CRL) {
				if err := identity.InstallCRL(s.CAPath, p.CRL); err != nil {
					log.Printf("PKI committed CRL installation: %v", err)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-interval.C:
		}
	}
}
