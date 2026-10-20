package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	buildversion "github.com/antonismor/Titanus-Core/internal/version"
	"github.com/antonismor/Titanus-Core/internal/webinstaller"
)

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "--version-json") {
		buildversion.Print(os.Args[1] == "--version-json")
		return
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "Titanus Web Installer must run as root.")
		os.Exit(1)
	}
	cfg := webinstaller.DefaultConfig()
	flag.StringVar(&cfg.Listen, "listen", cfg.Listen, "HTTPS listen address")
	flag.StringVar(&cfg.StateDir, "state-dir", cfg.StateDir, "private persistent installer state directory")
	flag.StringVar(&cfg.LogFile, "log-file", cfg.LogFile, "verbose installer log file")
	flag.Parse()

	server, err := webinstaller.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Titanus Web Installer:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := server.Serve(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "Titanus Web Installer:", err)
		os.Exit(1)
	}
}
