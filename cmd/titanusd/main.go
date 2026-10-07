package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const (
	version    = "0.1.0-dev"
	socketPath = "/run/titanus/titanus.sock"
)

func main() {
	if os.Geteuid() != 0 {
		log.Fatal("titanusd must run as root during the bootstrap development stage")
	}

	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		log.Fatal(err)
	}
	_ = os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	if err := os.Chmod(socketPath, 0660); err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "healthy",
			"service": "titanusd",
			"version": version,
			"time":    time.Now().UTC(),
		})
	})
	mux.HandleFunc("/v1/version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": version})
	})

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown failed: %v", err)
			_ = server.Close()
		}
	}()

	log.Printf("Titanus daemon %s listening on unix://%s", version, socketPath)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	log.Print("Titanus daemon stopped")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
