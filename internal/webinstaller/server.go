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
		writeError(w, http.StatusBadRequest, "validate a configuration first")
		return
	}
	if err := sshtrust.CheckTools(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.setState("discovering", "Infrastructure", 15, "Discovering Ed25519 SSH host keys. No remote change is made.", "")
	bootstrap := s.bootstrapFor(plan)
	hosts, err := bootstrap.Discover(r.Context(), plan.Nodes)
	if err != nil {
		s.fail("SSH host-key discovery failed: " + err.Error())
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	fingerprints := make([]Fingerprint, 0, len(hosts))
	for _, host := range hosts {
		fingerprints = append(fingerprints, Fingerprint{Node: host.Node.Name, Address: host.Node.ManagementIP, Fingerprint: host.Fingerprint})
		s.logf("INFO", "SSH fingerprint %s (%s): %s", host.Node.Name, host.Node.ManagementIP, host.Fingerprint)
	}
	s.mu.Lock()
	s.state.Status = "fingerprint-review"
	s.state.Phase = "Infrastructure"
	s.state.Progress = 20
	s.state.Message = "Verify every SSH fingerprint through a trusted console, then approve them to continue."
	s.state.Fingerprints = fingerprints
	s.state.UpdatedAt = time.Now().UTC()
	_ = s.persistStateLocked()
	state := s.state
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req startRequest
	if err := decodeJSON(w, r, &req); err != nil {
		return
	}
	plan, ok := s.currentPlan()
	if !ok {
		writeError(w, http.StatusBadRequest, "validate a configuration first")
		return
	}
	if req.Apply {
		if req.Password == "" || len(req.Password) > 4096 || strings.ContainsAny(req.Password, "\x00\r\n") {
			writeError(w, http.StatusBadRequest, "a non-empty single-line bootstrap password is required")
			return
		}
		if len(req.Fingerprints) != len(plan.Nodes) {
			writeError(w, http.StatusBadRequest, "approve every discovered SSH fingerprint before deployment")
			return
		}
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "an installation is already running")
		return
	}
	s.running = true
	s.state.Running = true
	s.state.Completed = false
	s.state.PreparedOnly = !req.Apply
	s.state.Status = "running"
	s.state.Phase = "Deployment"
	s.state.Progress = 22
	s.state.Message = "Starting the validated Titanus installation workflow."
	s.state.Error = ""
	s.state.StartedAt = time.Now().UTC()
	s.state.FinishedAt = time.Time{}
	s.state.AppliedNodes = 0
	s.state.TotalNodes = len(plan.Nodes)
	s.state.UpdatedAt = time.Now().UTC()
	_ = s.persistStateLocked()
	state := s.state
	s.mu.Unlock()

	plan.AutoDeploy = req.Apply
	if err := plan.Save(filepath.Join(s.cfg.StateDir, "plan.json")); err != nil {
		s.finishRun(false)
		writeError(w, http.StatusInternalServerError, "cannot save deployment mode")
		return
	}
	password := []byte(req.Password)
	approved := make(map[string]string, len(req.Fingerprints))
	for k, v := range req.Fingerprints {
		approved[k] = v
	}
	go s.runInstallation(plan, password, approved, req.Apply)
	writeJSON(w, http.StatusAccepted, state)
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		writeError(w, http.StatusConflict, "cannot reset while an installation is running")
		return
	}
	_ = os.Remove(filepath.Join(s.cfg.StateDir, "plan.json"))
	s.plan = nil
	s.state = InstallerState{Status: "idle", Phase: "Configuration", Progress: 0, Message: "Ready to configure Titanus Core.", UpdatedAt: time.Now().UTC()}
	_ = s.persistStateLocked()
	s.logf("INFO", "Installer session reset by operator")
	writeJSON(w, http.StatusOK, s.state)
}

func (s *Server) runInstallation(plan model.RealmPlan, password []byte, approved map[string]string, apply bool) {
	defer func() {
		for i := range password {
			password[i] = 0
		}
		s.finishRun(true)
	}()

	s.logf("INFO", "Starting Titanus Core installation session %s", s.sessionID())
	if err := plan.Validate(); err != nil {
		s.fail("Plan validation failed: " + err.Error())
		return
	}
	s.setState("running", "Validate Infrastructure", 25, "Validated Titanus plan and safety constraints.", "")
	s.logf("OK", "Plan safety validation passed")

	var trust sshtrust.Report
	if apply {
		if err := sshtrust.CheckTools(); err != nil {
			s.fail(err.Error())
			return
		}
		s.setState("running", "SSH Trust Bootstrap", 30, "Re-checking approved SSH identities before authentication.", "")
		bootstrap := s.bootstrapFor(plan)
		hosts, err := bootstrap.Discover(context.Background(), plan.Nodes)
		if err != nil {
			s.fail("SSH host-key discovery failed: " + err.Error())
			return
		}
		for _, host := range hosts {
			if approved[host.Node.Name] != host.Fingerprint {
				s.fail("SSH fingerprint changed or was not approved for " + host.Node.Name)
				return
			}
		}
		s.logf("OK", "All %d approved SSH fingerprints match the current hosts", len(hosts))
		s.setState("running", "SSH Trust Bootstrap", 34, "Establishing Realm-scoped passwordless SSH and verifying the directed full mesh.", "")
		trust, err = bootstrap.Apply(context.Background(), plan.RealmName, hosts, password)
		if err != nil {
			s.fail("SSH trust bootstrap failed: " + err.Error())
			return
		}
		s.logf("OK", "SSH full-mesh verified: %d/%d directed node pairs", trust.Verified, trust.Expected)
	}

	s.setState("running", "Prepare Environment", 42, "Preparing the checksum-pinned versioned deployment.", "")
	deploymentPath := filepath.Join(s.cfg.StateDir, "deployments", s.sessionID())
	if err := os.MkdirAll(filepath.Dir(deploymentPath), 0700); err != nil {
		s.fail("Cannot create deployment directory: " + err.Error())
		return
	}
	if _, err := os.Lstat(deploymentPath); err == nil {
		deploymentPath += "-" + time.Now().UTC().Format("150405")
	}
	prepared, err := deploy.PrepareRelease(plan, deploymentPath)
	if err != nil {
		s.fail("Deployment preparation failed: " + err.Error())
		return
	}
	s.mu.Lock()
	s.state.DeploymentPath = deploymentPath
	s.state.UpdatedAt = time.Now().UTC()
	_ = s.persistStateLocked()
	s.mu.Unlock()
	s.logf("OK", "Verified private deployment prepared at %s", deploymentPath)
	if !apply {
		s.mu.Lock()
		s.state.Status = "prepared"
		s.state.Phase = "Complete"
		s.state.Progress = 100
		s.state.Completed = true
		s.state.PreparedOnly = true
		s.state.Message = "Deployment prepared successfully. No remote host was changed."
		s.state.FinishedAt = time.Now().UTC()
		s.state.UpdatedAt = time.Now().UTC()
		_ = s.persistStateLocked()
		s.mu.Unlock()
		s.logf("OK", "Prepare-only workflow completed; no remote host was changed")
		return
	}

	executor := remote.NewSSHExecutor()
	executor.CommandTimeout = 3 * time.Minute
	executor.IdentityFile = trust.IdentityFile
	executor.KnownHostsFile = trust.KnownHostsFile
	verbose := &verboseSSH{inner: executor, logf: s.logf}
	deployer := deploy.NewRealmDeployer()
	deployer.SSH = verbose

	s.setState("running", "Deploy Components", 52, "Applying the verified release to Titanus nodes.", "")
	s.logf("INFO", "Applying release to %d nodes serially", len(plan.Nodes))
	watchDone := make(chan struct{})
	go s.watchResults(prepared.ResultsPath, len(plan.Nodes), watchDone)
	results, err := deployer.ApplyPrepared(plan, prepared)
	close(watchDone)
	if err != nil {
		s.fail("Deployment failed: " + err.Error())
		return
	}
	for _, result := range results {
		s.logf("OK", "Node %s (%s) %s", result.Node, result.Address, result.Message)
	}

	s.setState("running", "Run Health Checks", 94, "Verifying Titanus services on every installed node.", "")
	for _, node := range plan.Nodes {
		if _, err := verbose.Run(node, "systemctl --no-pager is-active titanusd.service titanus-agent.service"); err != nil {
			s.fail("Service health check failed on " + node.Name + ": " + err.Error())
			return
		}
		s.logf("OK", "Service health check passed on %s", node.Name)
	}
	if plan.Ceph.Enabled && plan.Ceph.Provision {
		s.logf("OK", "Ceph provisioning returned successfully through the verified deployment engine")
	}

	s.mu.Lock()
	s.state.Status = "completed"
	s.state.Phase = "Complete"
	s.state.Progress = 100
	s.state.Completed = true
	s.state.PreparedOnly = false
	s.state.AppliedNodes = len(plan.Nodes)
	s.state.Message = "Titanus Core installation completed successfully."
	s.state.FinishedAt = time.Now().UTC()
	s.state.UpdatedAt = time.Now().UTC()
	_ = s.persistStateLocked()
	s.mu.Unlock()
	s.logf("OK", "Titanus Core installation completed successfully")
}

func (s *Server) watchResults(path string, total int, done <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	seen := 0
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var results []deploy.RealmNodeResult
			if json.Unmarshal(data, &results) != nil || len(results) <= seen {
				continue
			}
			for _, result := range results[seen:] {
				s.logf("OK", "Installed node %s (%s)", result.Node, result.Address)
			}
			seen = len(results)
			progress := 52
			if total > 0 {
				progress = 52 + (seen*36)/total
			}
			if progress > 88 {
				progress = 88
			}
			s.mu.Lock()
			s.state.AppliedNodes = seen
			s.state.Progress = progress
			s.state.Message = fmt.Sprintf("Installed %d of %d Titanus nodes.", seen, total)
			s.state.UpdatedAt = time.Now().UTC()
			_ = s.persistStateLocked()
			s.mu.Unlock()
		}
	}
}

func (s *Server) bootstrapFor(plan model.RealmPlan) sshtrust.Bootstrap {
	knownHosts := filepath.Join("/root", ".ssh", "known_hosts")
	if _, err := os.Stat(knownHosts); err != nil {
		knownHosts = ""
	}
	return sshtrust.Bootstrap{
		Runner:             sshtrust.ExecRunner{},
		Directory:          filepath.Join(s.cfg.StateDir, "ssh", sshtrust.RealmID(plan.RealmName)),
		ExistingKnownHosts: knownHosts,
	}
}

func (s *Server) currentPlan() (model.RealmPlan, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.plan == nil {
		return model.RealmPlan{}, false
	}
	return *s.plan, true
}

func (s *Server) setState(status, phase string, progress int, message, errorText string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Status = status
	s.state.Phase = phase
	s.state.Progress = progress
	s.state.Message = message
	s.state.Error = errorText
	s.state.UpdatedAt = time.Now().UTC()
	_ = s.persistStateLocked()
}

func (s *Server) fail(message string) {
	s.mu.Lock()
	s.state.Status = "failed"
	s.state.Phase = "Deployment"
	s.state.Running = false
	s.state.Completed = false
	s.state.Error = message
	s.state.Message = "Installation stopped safely. Review the verbose output before retrying."
	s.state.FinishedAt = time.Now().UTC()
	s.state.UpdatedAt = time.Now().UTC()
	s.running = false
	_ = s.persistStateLocked()
	s.mu.Unlock()
	s.logf("ERROR", "%s", message)
}

func (s *Server) finishRun(onlyIfRunning bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if onlyIfRunning && !s.running {
		return
	}
	s.running = false
	s.state.Running = false
	s.state.UpdatedAt = time.Now().UTC()
	_ = s.persistStateLocked()
}

func (s *Server) sessionID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state.SessionID
}

func (s *Server) persistStateLocked() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.cfg.StateDir, "state.json"), append(data, '\n'), 0600)
}

func (s *Server) loadState() error {
	data, err := os.ReadFile(filepath.Join(s.cfg.StateDir, "state.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var state InstallerState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("decode installer state: %w", err)
	}
	s.state = state
	return nil
}

func (s *Server) loadPlan() error {
	path := filepath.Join(s.cfg.StateDir, "plan.json")
	plan, err := model.LoadPlan(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return nil
	}
	s.plan = &plan
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (s *Server) logf(level, format string, args ...any) {
	line := fmt.Sprintf("[%s] [%s] %s\n", time.Now().UTC().Format(time.RFC3339), level, fmt.Sprintf(format, args...))
	f, err := os.OpenFile(s.cfg.LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return
	}
	_, _ = io.WriteString(f, line)
	_ = f.Close()
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON request: "+err.Error())
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, http.StatusBadRequest, "trailing JSON content")
		return fmt.Errorf("trailing JSON content")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func localIPv4s() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err != nil || ip.To4() == nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			value := ip.String()
			if !seen[value] {
				seen[value] = true
				out = append(out, value)
			}
		}
	}
	sort.Strings(out)
	return out
}

func loadAccessToken(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "access.token")
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return "", fmt.Errorf("unsafe Web Installer access token file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		token := strings.TrimSpace(string(data))
		if len(token) != 64 {
			return "", fmt.Errorf("invalid Web Installer access token")
		}
		if _, err := hex.DecodeString(token); err != nil {
			return "", fmt.Errorf("invalid Web Installer access token")
		}
		return token, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw[:])
	if err := atomicWrite(path, []byte(token+"\n"), 0600); err != nil {
		return "", err
	}
	return token, nil
}

func compactLog(value string, max int) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > max {
		value = value[:max] + "…"
	}
	return value
}

func ensureTLSIdentity(stateDir string) (string, string, error) {
	certPath := filepath.Join(stateDir, "https.crt")
	keyPath := filepath.Join(stateDir, "https.key")
	if certInfo, certErr := os.Lstat(certPath); certErr == nil {
		keyInfo, keyErr := os.Lstat(keyPath)
		if keyErr == nil && certInfo.Mode().IsRegular() && keyInfo.Mode().IsRegular() && keyInfo.Mode().Perm()&0077 == 0 {
			if _, err := tlsKeyPairFiles(certPath, keyPath); err == nil {
				return certPath, keyPath, nil
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "Titanus Core Web Installer"},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(397 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, DNSNames: []string{"localhost", "titanus-installer.local"},
	}
	template.IPAddresses = append(template.IPAddresses, net.ParseIP("127.0.0.1"))
	for _, value := range localIPv4s() {
		if ip := net.ParseIP(value); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := atomicWrite(certPath, certPEM, 0644); err != nil {
		return "", "", err
	}
	if err := atomicWrite(keyPath, keyPEM, 0600); err != nil {
		return "", "", err
	}
	if _, err := tlsKeyPairFiles(certPath, keyPath); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}

func tlsKeyPairFiles(certPath, keyPath string) (*x509.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil || len(pair.Certificate) == 0 {
		return nil, fmt.Errorf("invalid Web Installer TLS identity")
	}
	cert, err := x509.ParseCertificate(pair.Certifi