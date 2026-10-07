package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/antonismor/Titanus-Core/internal/consensus"
	"github.com/antonismor/Titanus-Core/internal/controlapi"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/lease"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/realmclient"
	"github.com/antonismor/Titanus-Core/internal/reconcile"
	"github.com/antonismor/Titanus-Core/internal/route"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
)

const (
	version    = "0.3.0-dev"
	socketPath = "/run/titanus/titanus.sock"
)

func main() {
	if os.Geteuid() != 0 {
		log.Fatal("titanusd must run as root")
	}

	stateRoot := envDefault("TITANUS_STATE_ROOT", "/var/lib/titanus")
	realmName := envDefault("TITANUS_REALM_NAME", "TITANUS-REALM")
	store, err := realm.Open(stateRoot, realmName)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":   "healthy",
			"service":  "titanusd",
			"version":  version,
			"realm":    store.Snapshot().Name,
			"revision": store.Snapshot().Revision,
			"time":     time.Now().UTC(),
		})
	})
	mux.HandleFunc("/v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": version})
	})
	runtimeConfig := unitruntime.DefaultConfig()
	runtimeConfig.StateRoot = stateRoot
	if cgroupRoot := strings.TrimSpace(os.Getenv("TITANUS_CGROUP_ROOT")); cgroupRoot != "" {
		runtimeConfig.CgroupRoot = cgroupRoot
	}
	if initBinary := strings.TrimSpace(os.Getenv("TITANUS_INIT_BINARY")); initBinary != "" {
		runtimeConfig.InitBinary = initBinary
	}
	sourceManager := source.NewManager(stateRoot)
	runtimeManager := unitruntime.NewManager(runtimeConfig)
	if err := runtimeManager.Recover(); err != nil {
		log.Fatalf("runtime recovery: %v", err)
	}
	leaseManager, err := lease.Open(stateRoot, runtimeManager)
	if err != nil {
		log.Fatalf("lease recovery: %v", err)
	}
	defer leaseManager.Close()
	api := controlapi.New(store, runtimeManager, sourceManager, leaseManager)
	api.CAPath = envDefault("TITANUS_CA", "/etc/titanus/pki/ca.crt")
	if _, e := os.Stat(filepath.Join(filepath.Dir(api.CAPath), "ca.key")); envBool("TITANUS_CONTROLLER_MODE") && e == nil {
		api.Authority = &identity.Authority{Dir: filepath.Dir(api.CAPath), CertPath: api.CAPath, KeyPath: filepath.Join(filepath.Dir(api.CAPath), "ca.key"), Realm: realmName}
	}
	if _, e := os.Stat(filepath.Join(stateRoot, "realm", "consensus", "membership.json")); e == nil && strings.TrimSpace(os.Getenv("TITANUS_HA_CONFIG")) == "" {
		log.Fatal("HA-managed Realm cannot start without TITANUS_HA_CONFIG")
	}
	var quorum *consensus.Node
	if configPath := strings.TrimSpace(os.Getenv("TITANUS_HA_CONFIG")); configPath != "" {
		if !envBool("TITANUS_CONTROLLER_MODE") {
			log.Fatal("HA requires controller mode")
		}
		cfg, e := consensus.LoadConfig(configPath)
		if e != nil {
			log.Fatal(e)
		}
		if cfg.Realm != realmName || cfg.ID != os.Getenv("TITANUS_NODE_ID") {
			log.Fatal("HA config must match daemon Realm and Node identity")
		}
		quorum, e = consensus.Open(stateRoot, cfg, store.Snapshot(), api.CAPath, envDefault("TITANUS_CERT", "/etc/titanus/pki/node.crt"), envDefault("TITANUS_KEY", "/etc/titanus/pki/node.key"))
		if e != nil {
			log.Fatalf("Realm consensus: %v", e)
		}
		defer quorum.Close()
		if e = store.EnableConsensus(quorum); e != nil {
			log.Fatal(e)
		}
		api.Consensus = quorum
	}
	api.Register(mux)

	socket := envDefault("TITANUS_SOCKET", socketPath)
	unixListener, err := unixSocketAt(socket)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		_ = unixListener.Close()
		_ = os.Remove(socket)
	}()

	servers := []*http.Server{newHTTPServer(identity.LocalManagement(mux))}
	listeners := []net.Listener{unixListener}

	ca := envDefault("TITANUS_CA", "/etc/titanus/pki/ca.crt")
	cert := envDefault("TITANUS_CERT", "/etc/titanus/pki/node.crt")
	key := envDefault("TITANUS_KEY", "/etc/titanus/pki/node.key")
	clusterListen := strings.TrimSpace(os.Getenv("TITANUS_CLUSTER_LISTEN"))
	if clusterListen != "" {
		tlsConfig, err := identity.TLSConfig(ca, cert, key, true)
		if err != nil {
			log.Fatalf("Realm mTLS configuration: %v", err)
		}
		listener, err := tls.Listen("tcp", clusterListen, tlsConfig)
		if err != nil {
			log.Fatalf("Realm TCP listener: %v", err)
		}
		listeners = append(listeners, listener)
		servers = append(servers, newHTTPServer(identity.Authenticate(mux, ca)))
		log.Printf("Titanus Realm mTLS API listening on %s", clusterListen)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if quorum != nil {
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				_ = quorum.Initialize()
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	go healthLoop(ctx, store)
	go runtimeManager.RunHealth(ctx)
	if api.Authority != nil {
		go api.Authority.MaintainCRL(ctx)
	}

	controllerMode := envBool("TITANUS_CONTROLLER_MODE")
	gatewayMode := envBool("TITANUS_GATEWAY_MODE")
	controllerEndpoint := strings.TrimSpace(os.Getenv("TITANUS_CONTROLLER_ENDPOINT"))

	var clusterClient *realmclient.Client
	if controllerMode || (gatewayMode && controllerEndpoint != "") {
		clusterClient, err = realmclient.New(ca, cert, key)
		if err != nil {
			log.Fatalf("Realm mTLS client: %v", err)
		}
	}

	if gatewayMode {
		var routeManager *route.Manager
		if controllerMode {
			if quorum != nil {
				routeManager = route.NewManagerWithStateProvider(func() (realm.State, error) {
					return clusterClient.RealmState(controllerEndpoint)
				})
			} else {
				routeManager = route.NewManager(store)
			}
			log.Printf("Titanus Route gateway enabled with local controller state")
		} else if controllerEndpoint != "" {
			routeManager = route.NewManagerWithStateProvider(func() (realm.State, error) {
				return clusterClient.RealmState(controllerEndpoint)
			})
			log.Printf("Titanus Route gateway enabled with remote controller state %s", controllerEndpoint)
		} else {
			log.Printf("Titanus Route gateway disabled: TITANUS_CONTROLLER_ENDPOINT is required on non-controller gateways")
		}
		if routeManager != nil {
			go routeManager.Run(ctx)
		}
	}

	if controllerMode {
		var nodes reconcile.NodeRuntime = clusterClient
		if quorum != nil {
			nodes = &reconcile.GuardedNodes{NodeRuntime: clusterClient, Check: quorum.CheckLeader}
		}
		controller := &reconcile.Controller{Store: store, Nodes: nodes, Sources: sourceManager, Interval: 5 * time.Second}
		go controller.Run(ctx)
		log.Printf("Titanus Fleet reconciler enabled")
	}

	errCh := make(chan error, len(servers))
	for i := range servers {
		server := servers[i]
		listener := listeners[i]
		go func() {
			if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
				return
			}
			errCh <- nil
		}()
	}

	log.Printf("Titanus daemon %s Realm=%s listening on unix://%s", version, store.Snapshot().Name, socket)

	select {
	case <-ctx.Done():
		log.Print("Titanus daemon shutdown requested")
	case err := <-errCh:
		if err != nil {
			log.Printf("Titanus API listener failed: %v", err)
		}
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	for _, server := range servers {
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
			_ = server.Close()
		}
	}
	log.Print("Titanus daemon stopped")
}

func unixSocket() (net.Listener, error) { return unixSocketAt(socketPath) }

func unixSocketAt(socket string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0755); err != nil {
		return nil, err
	}
	_ = os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socket, 0660); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
}

func healthLoop(ctx context.Context, store *realm.Store) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if e := store.CheckLeader(); e != nil {
				continue
			}
			changed, err := store.EvaluateHealth(now.UTC(), 30*time.Second, 90*time.Second)
			if err != nil {
				log.Printf("Realm health evaluation failed: %v", err)
				continue
			}
			for _, node := range changed {
				log.Printf("Realm Node %s health -> %s", node.ID, node.State)
			}
		}
	}
}

func envDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
