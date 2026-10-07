package controlapi

import (
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
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
		if _, err := s.Authority.Revoke(serial); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	} else if r.Method != http.MethodGet {
		methodNotAllowed(w)
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
	data, err := s.Authority.RenewCSR(r.TLS.PeerCertificates[0], []byte(req.CSR))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	_, _ = w.Write(data)
}
