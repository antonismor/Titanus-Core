package controlapi

import (
	"embed"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"net/http"
)

//go:embed commandcenter/*
var commandCenter embed.FS

func (s *Server) registerCommandCenter(mux *http.ServeMux) {
	handler := s.authorize(func(w http.ResponseWriter, r *http.Request) {
		p, _ := identity.RequestPrincipal(r)
		if p.Role != identity.RoleAdmin {
			writeError(w, 403, fmt.Errorf("Command Center requires admin role"))
			return
		}
		if r.Method != "GET" {
			methodNotAllowed(w)
			return
		}
		name, typ := "index.html", "text/html; charset=utf-8"
		switch r.URL.Path {
		case "/command-center", "/command-center/":
		case "/command-center/app.js":
			name, typ = "app.js", "text/javascript; charset=utf-8"
		case "/command-center/style.css":
			name, typ = "style.css", "text/css; charset=utf-8"
		default:
			http.NotFound(w, r)
			return
		}
		data, e := commandCenter.ReadFile("commandcenter/" + name)
		if e != nil {
			http.Error(w, "asset unavailable", 503)
			return
		}
		w.Header().Set("Content-Type", typ)
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
		w.Write(data)
	})
	mux.HandleFunc("/command-center", handler)
	mux.HandleFunc("/command-center/", handler)
}
