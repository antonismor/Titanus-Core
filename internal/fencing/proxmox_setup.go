package fencing

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type ProxmoxSetup struct {
	Address    string         `json:"address"`
	Hypervisor string         `json:"hypervisor"`
	User       string         `json:"user"`
	Secret     string         `json:"secret"`
	CAPEM      string         `json:"ca_pem"`
	VMIDs      map[string]int `json:"vm_ids"`
}

// ProbeProxmox verifies API trust, VM identity and effective privileges using GET
// only. It never sends a power command, even when readiness fails.
func (p *Power) ProbeProxmox(node string) error {
	t, ok := p.Targets[node]
	if !ok || t.Provider != "proxmox" {
		return fmt.Errorf("Proxmox fencing target required")
	}
	data, err := p.proxmoxRequest(t, http.MethodGet, "/config")
	if err != nil {
		return err
	}
	uuid, err := proxmoxVMUUID(data)
	if err != nil {
		return err
	}
	if !strings.EqualFold(uuid, t.UUID) {
		return fmt.Errorf("out-of-band node UUID changed")
	}
	data, err = p.proxmoxRequest(t, http.MethodGet, "/status/current")
	if err != nil {
		return err
	}
	var state struct {
		Status string `json:"status"`
	}
	if err = json.Unmarshal(data, &state); err != nil {
		return err
	}
	if state.Status != "running" {
		return fmt.Errorf("Worker must be running before initial installation")
	}
	u, err := url.Parse(t.Address)
	if err != nil {
		return err
	}
	vmid := filepath.Base(u.Path)
	u.Path = "/api2/json/access/permissions"
	t.Address = u.String()
	data, err = p.proxmoxRequest(t, http.MethodGet, "?path="+url.QueryEscape("/vms/"+vmid))
	if err != nil {
		return err
	}
	var permissions map[string]map[string]json.RawMessage
	if err = json.Unmarshal(data, &permissions); err != nil {
		return err
	}
	for _, priv := range []string{"VM.Audit", "VM.PowerMgmt"} {
		if _, ok = permissions["/vms/"+vmid][priv]; !ok {
			return fmt.Errorf("Proxmox API token needs %s on VM %s", priv, vmid)
		}
	}
	return nil
}

// ConfigureProxmox creates private installer-local files after independently
// discovered guest UUIDs have been supplied. No credentials enter a Plan.
func ConfigureProxmox(parent string, input ProxmoxSetup, uuids map[string]string) (map[string]Target, error) {
	u, err := url.Parse(input.Address)
	if err != nil || u.Scheme != "https" || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, fmt.Errorf("Proxmox management address must be an HTTPS origin")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(input.Hypervisor) || input.Secret == "" || len(input.Secret) > 4096 || strings.ContainsAny(input.Secret, "\x00\r\n") || len(input.CAPEM) > 1<<20 {
		return nil, fmt.Errorf("complete Proxmox hypervisor, token and CA settings required")
	}
	if len(uuids) == 0 || len(input.VMIDs) != len(uuids) {
		return nil, fmt.Errorf("one VM ID is required for every EXECUTION Worker")
	}
	if err = os.MkdirAll(parent, 0700); err != nil {
		return nil, err
	}
	st, err := os.Lstat(parent)
	if err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("private fencing directory required")
	}
	dir, err := os.MkdirTemp(parent, "proxmox-")
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			os.RemoveAll(dir)
		}
	}()
	credential, ca := filepath.Join(dir, "token"), filepath.Join(dir, "ca.crt")
	if err = os.WriteFile(credential, []byte(input.Secret+"\n"), 0600); err != nil {
		return nil, err
	}
	if err = os.WriteFile(ca, []byte(input.CAPEM), 0600); err != nil {
		return nil, err
	}
	targets := map[string]Target{}
	seen := map[int]bool{}
	for name, uuid := range uuids {
		id := input.VMIDs[name]
		if id < 1 || seen[id] {
			return nil, fmt.Errorf("unique positive Worker VM IDs required")
		}
		seen[id] = true
		targets[name] = Target{Provider: "proxmox", UUID: uuid, Address: strings.TrimRight(input.Address, "/") + "/api2/json/nodes/" + input.Hypervisor + "/qemu/" + strconv.Itoa(id), User: input.User, CredentialFile: credential, CAFile: ca}
	}
	if err = ValidateTargets(targets, true); err != nil {
		return nil, err
	}
	p := &Power{Targets: targets}
	for name := range targets {
		if err = p.ProbeProxmox(name); err != nil {
			return nil, fmt.Errorf("%s readiness: %w", name, err)
		}
	}
	keep = true
	return targets, nil
}
