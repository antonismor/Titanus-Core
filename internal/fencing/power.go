// Package fencing provides explicit out-of-band, identity-bound power fences.
// No deadline or failed application API is accepted as a successful fence.
package fencing

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
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
}
type Power struct {
	Targets map[string]Target
	run     func(string, ...string) (string, error)
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
func (p *Power) Fence(node string, check func() error) error {
	target, ok := p.Targets[node]
	if !ok || check == nil {
		return fmt.Errorf("no bound out-of-band fence for node")
	}
	if err := check(); err != nil {
		return err
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
