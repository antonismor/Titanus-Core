package webinstaller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antonismor/Titanus-Core/internal/deploy"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/remote"
	"github.com/antonismor/Titanus-Core/internal/sshtrust"
	buildversion "github.com/antonismor/Titanus-Core/internal/version"
)

const (
	defaultListen   = "0.0.0.0:8088"
	maxRequestBytes = 2 << 20
	sessionCookie   = "titanus_installer_session"
)

//go:embed assets/*
var webAssets embed.FS

type Config struct {
	Listen   string
	StateDir string
	LogFile  string
}

type Fingerprint struct {
	Node        string `json:"node"`
	Address     string `json:"address"`
	Fingerprint string `json:"fingerprint"`
}

type InstallerState struct {
	SessionID      string        `json:"session_id,omitempty"`
	Status         string        `json:"status"`
	Phase          string        `json:"phase"`
	Progress       int           `json:"progress"`
	Message        string        `json:"message"`
	Error          string        `json:"error,omitempty"`
	Running        bool          `json:"running"`
	Completed      bool          `json:"completed"`
	PreparedOnly   bool          `json:"prepared_only,omitempty"`
	PlanPath       string        `json:"plan_path,omitempty"`
	DeploymentPath string        `json:"deployment_path,omitempty"`
	AppliedNodes   int           `json:"applied_nodes,omitempty"`
	TotalNodes     int           `json:"total_nodes,omitempty"`
	Fingerprints   []Fingerprint `json:"fingerprints,omitempty"`
	StartedAt      time.Time     `json:"started_at,omitempty"`
	UpdatedAt      time.Time     `json:"updated_at"`
	FinishedAt     time.Time     `json:"finished_at,omitempty"`
}

type bootstrapResponse struct {
	Product      string           `json:"product"`
	Edition      string           `json:"edition"`
	Enterprise   string           `json:"enterprise"`
	NodeLimit    int              `json:"community_node_limit"`
	Version      string           `json:"version"`
	Revision     string           `json:"revision"`
	StateProfile string           `json:"state_profile"`
	Listen       string           `json:"listen"`
	LocalIPs     []string         `json:"local_ips"`
	Capabilities []string         `json:"capabilities"`
	State        InstallerState   `json:"state"`
	Plan         *model.RealmPlan `json:"plan,omitempty"`
}

type sessionRequest struct {
	Token string `json:"token"`
}

type startRequest struct {
	Password     string            `json:"password"`
	Apply        bool              `json:"apply"`
	Fingerprints map[string]string `json:"fingerprints,omitempty"`
}

type verboseSSH struct {
	inner *remote.SSHExecutor
	logf  func(string, string, ...any)
}

func (v *verboseSSH) Run(node model.NodeSpec, command string) (remote.Result, error) {
	if v.logf != nil {
		v.logf("VERBOSE", "[%s] SSH: %s", node.Name, compactLog(command, 720))
	}
	result, err := v.inner.Run(node, command)
	if result.Stdout != "" && v.logf != nil {
		v.logf("OUTPUT", "[%s] %s", node.Name, compactLog(result.Stdout, 4096))
	}
	if err != nil && v.logf != nil {
		v.logf("ERROR", "[%s] SSH command failed: %s", node.Name, compactLog(err.Error(), 1024))
	}
	return result, err
}

func (v *verboseSSH) CopyFile(node model.NodeSpec, localPath, remotePath string) error {
	if v.logf != nil {
		v.logf("VERBOSE", "[%s] transfer %s -> %s", node.Name, filepath.Base(localPath), remotePath)
	}
	err := v.inner.CopyFile(node, localPath, remotePath)
	if err != nil && v.logf != nil {
		v.logf("ERROR", "[%s] transfer failed: %s", node.Name, compactLog(err.Error(), 1024))
	}
	return err
}

type Server struct {
	cfg         Config
	accessToken string
	mu          sync.RWMutex
	state       InstallerState
	plan        *model.RealmPlan
	running     bool
}

func DefaultConfig() Config {
	return Config{
		Listen:   defaultListen,
		StateDir: "/var/lib/titanus/installer-web",
		LogFile:  "/var/log/titanus/installer-web.log",
	}
}

func New(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.Listen) == "" {
		cfg.Listen = defaultListen
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		cfg.StateDir = "/var/lib/titanus/installer-web"
	}
	if strings.TrimSpace(cfg.LogFile) == "" {
		cfg.LogFile = "/var/log/titanus/installer-web.log"
	}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return nil, fmt.Errorf("create installer state directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.LogFile), 0755); err != nil {
		return nil, fmt.Errorf("create installer log directory: %w", err)
	}
	token, err := loadAccessToken(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	if _, _, err := ensureTLSIdentity(cfg.StateDir); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, accessToken: token}
	s.state = InstallerState{Status: "idle", Phase: "Configuration", Progress: 0, Message: "Ready to configure Titanus Core.", UpdatedAt: time.Now().UTC()}
	if err := s.loadState(); err != nil {
		return nil, err
	}
	if s.state.Running {
		s.state.Running = false
		s.state.Status = "interrupted"
		s.state.Message = "The installer service restarted. The last persisted installation status is preserved; validate the configuration before resuming."
		s.state.UpdatedAt = time.Now().UTC()
		_ = s.persistStateLocked()
	}
	_ = s.loadPlan()
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/session", http.HandlerFunc(s.handleSession))
	mux.Handle("/api/v1/bootstrap", s.requireToken(http.HandlerFunc(s.handleBootstrap)))
	mux.Handle("/api/v1/status", s.requireToken(http.HandlerFunc(s.handleStatus)))
	mux.Handle("/api/v1/logs", s.requireToken(http.HandlerFunc(s.handleLogs)))
	mux.Handle("/api/v1/validate", s.requireToken(http.HandlerFunc(s.handleValidate)))
	mux.Handle("/api/v1/discover", s.requireToken(http.HandlerFunc(s.handleDiscover)))
	mux.Handle("/api/v1/start", s.requireToken(http.HandlerFunc(s.handleStart)))
	mux.Handle("/api/v1/reset", s.requireToken(http.HandlerFunc(s.handleReset)))
	static, _ := fs.Sub(webAssets, "assets")
	mux.Handle("/", http.FileServer(http.FS(static)))
	return securityHeaders(mux)
}

func (s *Server) Serve(ctx context.Context) error {
	httpServer := &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	certFile, keyFile, err := ensureTLSIdentity(s.cfg.StateDir)
	if err != nil {
		return err
	}
	s.logf("INFO", "Titanus Core mini HTTPS server listening on %s", s.cfg.Listen)
	err = httpServer.ListenAndServeTLS(certFile, keyFile)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("X-Titanus-Installer-Token")
		if provided == "" {
			if cookie, err := r.Cookie(sessionCookie); err == nil {
				provided = cookie.Value
			}
		}
		if provided != s.accessToken {
			writeError(w, http.StatusUnauthorized, "open the installer using the access URL printed by tuigui")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req sessionRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	if req.Token != s.accessToken {
		writeError(w, http.StatusUnauthorized, "invalid installer access token")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: s.accessToken, Path: "/", HttpOnly: true, Secure: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 12 * 60 * 60,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"authenticated": true})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	s.mu.RLock()
	state := s.state
	var plan *model.RealmPlan
	if s.plan != nil {
		copy := *s.plan
		plan = &copy
	}
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, bootstrapResponse{
		Product:      "Titanus Core Universal Installer",
		Edition:      "Community",
		Enterprise:   "Soon Available",
		NodeLimit:    model.CommunityNodeLimit,
		Version:      buildversion.Version,
		Revision:     buildversion.Revision,
		StateProfile: buildversion.StateProfile,
		Listen:       s.cfg.Listen,
		LocalIPs:     localIPv4s(),
		Capabilities: []string{"CONTROL", "EXECUTION", "STORAGE", "GATEWAY", "GPU", "BACKUP"},
		State:        state,
		Plan:         plan,
	})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	s.mu.RLock()
	state := s.state
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	data, err := os.ReadFile(s.cfg.LogFile)
	if err != nil && !os.IsNotExist(err) {
		writeError(w, http.StatusInternalServerError, "cannot read installer log")
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > 600 {
		lines = lines[len(lines)-600:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "an installation is already running")
		return
	}
	s.mu.Unlock()

	var plan model.RealmPlan
	if err := decodeJSON(w, r, &plan); err != nil {
		return
	}
	plan.Normalize()
	if err := plan.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if plan.Installation == nil {
		writeError(w, http.StatusBadRequest, "the Web Installer requires a titanus-plan/v2 installation")
		return
	}
	plan.AutoDeploy = false
	planPath := filepath.Join(s.cfg.StateDir, "plan.json")
	if err := plan.Save(planPath); err != nil {
		writeError(w, http.StatusInternalServerError, "cannot save validated plan")
		return
	}
	s.mu.Lock()
	s.plan = &plan
	s.state = InstallerState{
		SessionID:    newSessionID(),
		Status:       "validated",
		Phase:        "Review",
		Progress:     8,
		Message:      "Configuration validated successfully. Review SSH host fingerprints before deployment.",
		PlanPath:     planPath,
		TotalNodes:   len(plan.Nodes),
		UpdatedAt:    time.Now().UTC(),
		Fingerprints: nil,
	}
	_ = s.persistStateLocked()
	state := s.state
	s.mu.Unlock()
	s.logf("OK", "Plan validated: realm=%s profile=%s nodes=%d", plan.RealmName, plan.Installation.Profile, len(plan.Nodes))
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	plan, ok := s.currentPlan()
	if !ok {
		writeError(w, http.StatusBadRequest, "validate a configuration fir