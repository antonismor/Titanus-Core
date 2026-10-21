package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/antonismor/Titanus-Core/internal/fencing"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/sshtrust"
)

func (w *Wizard) configureProxmox(plan model.RealmPlan) (map[string]fencing.Target, error) {
	var input fencing.ProxmoxSetup
	var err error
	if input.Address, err = w.ask("Proxmox HTTPS origin (https://host:8006)", "", true); err != nil {
		return nil, err
	}
	if input.Hypervisor, err = w.ask("Proxmox hypervisor node name", "", true); err != nil {
		return nil, err
	}
	if input.User, err = w.ask("Proxmox API token identity (user@realm!token)", "", true); err != nil {
		return nil, err
	}
	secret, err := w.readSecret("Proxmox API token secret (hidden): ")
	if err != nil {
		return nil, err
	}
	defer func() {
		for i := range secret {
			secret[i] = 0
		}
	}()
	input.Secret = string(secret)
	defer func() { input.Secret = "" }()
	caFile, err := w.ask("Proxmox issuer CA certificate PEM file", "", true)
	if err != nil {
		return nil, err
	}
	st, err := os.Lstat(caFile)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return nil, fmt.Errorf("regular CA certificate file required")
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	input.CAPEM = string(ca)
	workers := []model.NodeSpec{}
	input.VMIDs = map[string]int{}
	for _, node := range plan.Nodes {
		if hasCapability(node, model.CapabilityExecution) {
			workers = append(workers, node)
			id, e := w.askInt("Proxmox VM ID for "+node.Name, 0, 1, 999999999)
			if e != nil {
				return nil, e
			}
			input.VMIDs[node.Name] = id
		}
	}
	password, err := w.readPassword()
	if err != nil {
		return nil, err
	}
	defer func() {
		for i := range password {
			password[i] = 0
		}
	}()
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	bootstrap := sshtrust.Bootstrap{Runner: sshtrust.ExecRunner{}, Directory: filepath.Join(home, ".ssh", "titanus-installer", sshtrust.RealmID(plan.RealmName)), ExistingKnownHosts: filepath.Join(home, ".ssh", "known_hosts")}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	uuids, err := bootstrap.ReadOnlyUUIDs(ctx, workers, password)
	if err != nil {
		return nil, err
	}
	targets, err := fencing.ConfigureProxmoxContext(ctx, "/var/lib/titanus/installer-web/fencing", input, uuids)
	if err == nil {
		fmt.Fprintln(w.out, "Proxmox fencing verified: trusted TLS, guest UUIDs and VM.Audit/VM.PowerMgmt. No power commands sent.")
	}
	return targets, err
}
