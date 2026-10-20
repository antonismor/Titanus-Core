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
	mux.H