// Package fencing provides explicit out-of-band, identity-bound power fences.
// No deadline or failed application API is accepted as a successful fence.
package fencing

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type Target struct {
	Provider       string `json:"provider"`
	UUID           string `json:"uuid"`
	Address        string `json:"address"`
	User           string `json:"user,omitempty"`
	CredentialFile string `json:"credential_file,omitempty"`
	CAFile         string `json:"ca_file,omitempty"`
}
type Power struct {
	Targets map[string]Target
	run     func(string, ...string) (string, error)
	request func(Target, string, string) (json.RawMessage, error)
}

func privateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("fencing configuration/credentials require root-owned private regular files")
	}
	return nil
}
func Load(path string) (*Power, error) {
	if err := privateFile(path); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var targets map[string]Target
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&targets); err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no power fencing targets")
	}
	uuids := map[string]bool{}
	for node, target := range targets {
		if node == "" || !regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`).MatchString(target.UUID) || uuids[strings.ToLower(target.UUID)] {
			return nil, fmt.Errorf("invalid/duplicate power target identity")
		}
		uuids[strings.ToLower(target.UUID)] = true
		switch target.Provider {
		case "ipmi":
			if net.ParseIP(target.Address) == nil || target.User == "" {
				return nil, fmt.Errorf("IPMI requires literal BMC address and user")
			}
			if err = privateFile(target.CredentialFile); err != nil {
				return nil, err
			}
		case "libvirt":
			uri, e := url.Parse(target.Address)
			valid := e == nil && uri.Path == "/system" && uri.RawQuery == "" && uri.Fragment == "" && (uri.Scheme == "qemu+ssh" && uri.Host != "" || uri.Scheme == "qemu" && uri.Host == "")
			if uri != nil && uri.User != nil {
				if _, present := uri.User.Password(); present {
					valid = false
				}
			}
			if !valid {
				return nil, fmt.Errorf("libvirt requires an explicit privileged hypervisor URI")
			}
		case "proxmox":
			uri, e := url.Parse(target.Address)
			valid := e == nil && uri != nil && uri.Scheme == "https" && uri.Host != "" &&
				uri.User == nil && uri.RawQuery == "" && uri.Fragment == "" &&
				regexp.MustCompile(`^/api2/json/nodes/[A-Za-z0-9_.-]+/qemu/[1-9][0-9]*$`).MatchString(uri.Path) &&
				regexp.MustCompile(`^[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+![A-Za-z0-9_.-]+$`).MatchString(target.User)
			if !valid {
				return nil, fmt.Errorf("Proxmox requires an HTTPS VM API URL and API token identity")
			}
			if err = privateFile(target.CredentialFile); err != nil {
				return nil, err
			}
			if err = privateFile(target.CAFile); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported power fence provider")
		}
	}
	return &Power{Targets: targets}, nil
}
func (p *Power) command(binary string, args ...string) (string, error) {
	if p.run != nil {
		return p.run(binary, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
	// Never echo a tool response that may contain management credentials.
	if err != nil {
		return "", fmt.Errorf("out-of-band fence command failed: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}
func (p *Power) proxmoxRequest(target Target, method, suffix string) (json.RawMessage, error) {
	if p.request != nil {
		return p.request(target, method, suffix)
	}
	credential, err := os.ReadFile(target.CredentialFile)
	if err != nil {
		return nil, err
	}
	secret := strings.TrimSpace(string(credential))
	if secret == "" {
		return nil, fmt.Errorf("empty Proxmox API token secret")
	}
	caPEM, err := os.ReadFile(target.CAFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("invalid Proxmox CA certificate")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest(method, strings.TrimRight(target.Address, "/")+suffix, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "PVEAPIToken="+target.User+"="+secret)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Proxmox fence API request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("Proxmox fence API %s returned HTTP %d", suffix, resp.StatusCode)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err = json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("invalid Proxmox API response: %w", err)
	}
	return envelope.Data, nil
}

func proxmoxVMUUID(data json.RawMessage) (string, error) {
	var config struct {
		SMBIOS1 string `json:"smbios1"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return "", err
	}
	match := regexp.MustCompile(`(?:^|,)uuid=([0-9a-fA-F-]{36})(?:,|$)`).FindStringSubmatch(config.SMBIOS1)
	if len(match) != 2 {
		return "", fmt.Errorf("Proxmox VM configuration has no exact SMBIOS UUID")
	}
	return match[1], nil
}

func (p *Power) fenceProxmox(target Target, check func() error) error {
	identify := func() error {
		data, err := p.proxmoxRequest(target, http.MethodGet, "/config")
		if err != nil {
			return err
		}
		actual, err := proxmoxVMUUID(data)
		if err != nil {
			return err
		}
		if !strings.EqualFold(actual, target.UUID) {
			return fmt.Errorf("out-of-band node UUID changed")
		}
		return nil
	}
	isOff := func() (bool, error) {
		data, err := p.proxmoxRequest(target, http.MethodGet, "/status/current")
		if err != nil {
			return false, err
		}
		var status struct {
			Status string `json:"status"`
		}
		if err = json.Unmarshal(data, &status); err != nil {
			return false, err
		}
		switch status.Status {
		case "stopped":
			return true, nil
		case "running":
			return false, nil
		default:
			return false, fmt.Errorf("unexpected Proxmox VM status %q", status.Status)
		}
	}
	if err := identify(); err != nil {
		return err
	}
	stopped, err := isOff()
	if err != nil {
		return err
	}
	if !stopped {
		if err = check(); err != nil {
			return err
		}
		if _, err = p.proxmoxRequest(target, http.MethodPost, "/status/stop"); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	confirmations := 0
	for time.Now().Before(deadline) {
		if err = check(); err != nil {
			return err
		}
		stopped, err = isOff()
		if err != nil {
			return err
		}
		if stopped {
			confirmations++
		} else {
			confirmations = 0
		}
		if confirmations >= 2 {
			if err = identify(); err != nil {
				return err
			}
			return check()
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("physical/VM power-off was not confirmed")
}

func (p *Power) Fence(node string, check func() error) error {
	target, ok := p.Targets[node]
	if !ok || check == nil {
		return fmt.Errorf("no bound out-of-band fence for node")
	}
	if err := check(); err != nil {
		return err
	}
	if target.Provider == "proxmox" {
		return p.fenceProxmox(target, check)
	}
	binary := "/usr/bin/ipmitool"
	base := []string{"-I", "lanplus", "-C", "17", "-H", target.Address, "-U", target.User, "-f", target.CredentialFile, "-R", "1", "-N", "2"}
	identity := []string{"mc", "guid"}
	off := []string{"chassis", "power", "off"}
	status := []string{"chassis", "power", "status"}
	if target.Provider == "libvirt" {
		binary = "/usr/bin/virsh"
		base = []string{"-c", target.Address}
		identity = []string{"domuuid", target.UUID}
		off = []string{"destroy", target.UUID}
		status = []string{"domstate", target.UUID}
	}
	identify := func() error {
		output, err := p.command(binary, append(append([]string{}, base...), identity...)...)
		if err != nil {
			return err
		}
		actual := output
		if target.Provider == "ipmi" {
			match := regexp.MustCompile(`(?m)^System GUID\s*:\s*([0-9a-fA-F-]{36})\s*$`).FindStringSubmatch(output)
			if len(match) != 2 {
				return fmt.Errorf("BMC did not return an exact UUID")
			}
			actual = match[1]
		}
		if !strings.EqualFold(actual, target.UUID) {
			return fmt.Errorf("out-of-band node UUID changed")
		}
		return nil
	}
	if err := identify(); err != nil {
		return err
	}
	isOff := func() (bool, error) {
		output, err := p.command(binary, append(append([]string{}, base...), status...)...)
		if err != nil {
			return false, err
		}
		if target.Provider == "ipmi" {
			return output == "Chassis Power is off", nil
		}
		return output == "shut off", nil
	}
	stopped, err := isOff()
	if err != nil {
		return err
	}
	if !stopped {
		if err = check(); err != nil {
			return err
		}
		if _, err = p.command(binary, append(append([]string{}, base...), off...)...); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(30 * time.Second)
	confirmations := 0
	for time.Now().Before(deadline) {
		if err = check(); err != nil {
			return err
		}
		stopped, err = isOff()
		if err != nil {
			return err
		}
		if stopped {
			confirmations++
		} else {
			confirmations = 0
		}
		if confirmations >= 2 {
			if err = identify(); err != nil {
				return err
			}
			return check()
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("physical/VM power-off was not confirmed")
}
