package main

import (
	"context"
	"encoding/json"
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
	flag.StringVar(&cfg.TLSCert, "tls-cert", cfg.TLSCert, "trusted server certificate chain")
	flag.StringVar(&cfg.TLSKey, "tls-key", cfg.TLSKey, "private server key")
	flag.StringVar(&cfg.TLSCA, "tls-ca", cfg.TLSCA, "trusted issuer CA certificate")
	prepareTLS := flag.Bool("prepare-tls", false, "detect local addresses, prepare TLS and print public trust information")
	installTrust := flag.Bool("install-local-trust", false, "enroll the issuer CA in this host trust store during --prepare-tls")
	flag.Parse()
	if *prepareTLS {
		info, err := webinstaller.PrepareTLS(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if *installTrust {
			if err = webinstaller.EnrollLocalCATrust(info); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
		_ = json.NewEncoder(os.Stdout).Encode(info)
		return
	}

	if *installTrust {
		fmt.Fprintln(os.Stderr, "--install-local-trust requires --prepare-tls")
		os.Exit(1)
	}
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
