package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/sshtrust"
	"golang.org/x/sys/unix"
)

// readPassword refuses redirected input and always restores terminal echo.
// It uses the wizard's existing reader so no buffered input is silently lost.
func (w *Wizard) readPassword() ([]byte, error) {
	f, ok := w.terminal.(*os.File)
	if !ok {
		return nil, fmt.Errorf("bootstrap password requires an interactive terminal")
	}
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, fmt.Errorf("bootstrap password requires an interactive terminal")
	}
	next := *old
	next.Lflag &^= unix.ECHO
	if err = unix.IoctlSetTermios(fd, unix.TCSETS, &next); err != nil {
		return nil, err
	}
	defer unix.IoctlSetTermios(fd, unix.TCSETS, old)
	fmt.Fprint(w.out, "Common initial root password (hidden, never saved): ")
	raw, err := w.in.ReadBytes('\n')
	fmt.Fprintln(w.out)
	if err != nil {
		return nil, err
	}
	raw = raw[:len(raw)-1]
	if len(raw) > 0 && raw[len(raw)-1] == '\r' {
		raw = raw[:len(raw)-1]
	}
	return raw, nil
}

// BootstrapTrust is an explicit action after the offline plan has been saved
// and prepared. Rejecting fingerprints never authenticates or mutates a host.
func (w *Wizard) BootstrapTrust(ctx context.Context, plan model.RealmPlan) (sshtrust.Report, error) {
	if err := plan.Validate(); err != nil {
		return sshtrust.Report{}, err
	}
	if err := sshtrust.CheckTools(); err != nil {
		return sshtrust.Report{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return sshtrust.Report{}, err
	}
	b := sshtrust.Bootstrap{Runner: sshtrust.ExecRunner{}, Directory: filepath.Join(home, ".ssh", "titanus-installer", sshtrust.RealmID(plan.RealmName)), ExistingKnownHosts: filepath.Join(home, ".ssh", "known_hosts")}
	hosts, err := b.Discover(ctx, plan.Nodes)
	if err != nil {
		return sshtrust.Report{}, err
	}
	fmt.Fprintln(w.out, "Verify these Ed25519 fingerprints through a trusted console:")
	for _, h := range hosts {
		fmt.Fprintf(w.out, "  %-16s %-20s %s\n", h.Node.Name, h.Node.ManagementIP, h.Fingerprint)
	}
	answer, err := w.ask("Type TRUST to approve ALL fingerprints and configure SSH keys", "", true)
	if err != nil {
		return sshtrust.Report{}, err
	}
	if strings.TrimSpace(answer) != "TRUST" {
		return sshtrust.Report{}, fmt.Errorf("SSH trust bootstrap cancelled before authentication")
	}
	password, err := w.readPassword()
	if err != nil {
		return sshtrust.Report{}, err
	}
	defer func() {
		for i := range password {
			password[i] = 0
		}
	}()
	report, err := b.Apply(ctx, plan.RealmName, hosts, password)
	if err != nil {
		return report, err
	}
	fmt.Fprintf(w.out, "SSH full-mesh verified: %d/%d directed node pairs, plus installer key access to every node.\n", report.Verified, report.Expected)
	return report, nil
}

// RunTrustDefault can also bootstrap a previously saved plan without applying it.
func RunTrustDefault(ctx context.Context, plan model.RealmPlan) (sshtrust.Report, error) {
	return New(os.Stdin, os.Stdout).BootstrapTrust(ctx, plan)
}
