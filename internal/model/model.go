package model

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"time"
)

type Capability string

const (
	CapabilityControl   Capability = "CONTROL"
	CapabilityExecution Capability = "EXECUTION"
	CapabilityStorage   Capability = "STORAGE"
	CapabilityGateway   Capability = "GATEWAY"
	CapabilityGPU       Capability = "GPU"
	CapabilityBackup    Capability = "BACKUP"
)

type NodeSpec struct {
	Name          string       `json:"name"`
	ManagementIP  string       `json:"management_ip"`
	FabricIP      string       `json:"fabric_ip,omitempty"`
	CephPublicIP  string       `json:"ceph_public_ip,omitempty"`
	CephClusterIP string       `json:"ceph_cluster_ip,omitempty"`
	SSHUser       string       `json:"ssh_user"`
	SSHPort       int          `json:"ssh_port"`
	Capabilities  []Capability `json:"capabilities"`
	CephDevices   []string     `json:"ceph_devices,omitempty"`
}

type CephSpec struct {
	Enabled      bool   `json:"enabled"`
	PublicCIDR   string `json:"public_cidr,omitempty"`
	ClusterCIDR  string `json:"cluster_cidr,omitempty"`
	Replication  int    `json:"replication"`
	EnableRBD    bool   `json:"enable_rbd"`
	EnableCephFS bool   `json:"enable_cephfs"`
	EnableRGW    bool   `json:"enable_rgw"`
	RequestedTB  int    `json:"requested_usable_tb,omitempty"`
	Provision    bool   `json:"provision"`
}

type RealmPlan struct {
	Version      string        `json:"version"`
	RealmName    string        `json:"realm_name"`
	ControlVIP   string        `json:"control_vip,omitempty"`
	FabricCIDR   string        `json:"fabric_cidr"`
	ServiceCIDR  string        `json:"service_cidr"`
	CreatedAt    time.Time     `json:"created_at"`
	Nodes        []NodeSpec    `json:"nodes"`
	Ceph         CephSpec      `json:"ceph"`
	AutoDeploy   bool          `json:"auto_deploy"`
	Installation *Installation `json:"installation,omitempty"`
}

func (p *RealmPlan) Normalize() {
	p.RealmName = strings.TrimSpace(p.RealmName)
	for i := range p.Nodes {
		p.Nodes[i].Name = strings.TrimSpace(p.Nodes[i].Name)
		p.Nodes[i].SSHUser = strings.TrimSpace(p.Nodes[i].SSHUser)
		for j := range p.Nodes[i].CephDevices {
			p.Nodes[i].CephDevices[j] = strings.TrimSpace(p.Nodes[i].CephDevices[j])
		}
		if p.Nodes[i].SSHPort == 0 {
			p.Nodes[i].SSHPort = 22
		}
	}
	if p.Version == "" {
		p.Version = "titanus-plan/v1"
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
}

func (p RealmPlan) Validate() error {
	if err := p.validateInstallation(); err != nil {
		return err
	}
	if p.Version != "" && p.Version != "titanus-plan/v1" && p.Version != "titanus-plan/v2" {
		return fmt.Errorf("unsupported plan version")
	}
	if p.Installation != nil {
		if p.Version != "titanus-plan/v2" {
			return fmt.Errorf("versioned installation requires plan v2")
		}
		if err := p.Installation.Validate(); err != nil {
			return err
		}
		if p.ControlVIP != "" {
			return fmt.Errorf("control VIP requires a separately registered/fenced Gateway; controller origins are used for installation")
		}
	}
	if strings.TrimSpace(p.RealmName) == "" {
		return errors.New("realm name is required")
	}
	if len(p.Nodes) == 0 {
		return errors.New("at least one node is required")
	}
	if err := validateCIDR("Fabric CIDR", p.FabricCIDR); err != nil {
		return err
	}
	if err := validateCIDR("Service CIDR", p.ServiceCIDR); err != nil {
		return err
	}
	if p.ControlVIP != "" && net.ParseIP(p.ControlVIP) == nil {
		return fmt.Errorf("invalid control VIP %q", p.ControlVIP)
	}

	seenNames := map[string]bool{}
	seenIPs := map[string]string{}
	controlCount := 0
	storageCount := 0

	for _, n := range p.Nodes {
		if n.Name == "" {
			return errors.New("node name cannot be empty")
		}
		if seenNames[n.Name] {
			return fmt.Errorf("duplicate node name %q", n.Name)
		}
		seenNames[n.Name] = true

		if n.SSHUser == "" {
			return fmt.Errorf("node %s has no SSH user", n.Name)
		}
		if n.SSHPort < 1 || n.SSHPort > 65535 {
			return fmt.Errorf("node %s has invalid SSH port", n.Name)
		}

		for label, ip := range map[string]string{
			"management":   n.ManagementIP,
			"fabric":       n.FabricIP,
			"ceph-public":  n.CephPublicIP,
			"ceph-cluster": n.CephClusterIP,
		} {
			if ip == "" {
				if label == "management" {
					return fmt.Errorf("node %s requires a management IP", n.Name)
				}
				continue
			}
			if net.ParseIP(ip) == nil {
				return fmt.Errorf("node %s has invalid %s IP %q", n.Name, label, ip)
			}
			if owner, exists := seenIPs[ip]; exists {
				return fmt.Errorf("IP %s is used by both %s and %s", ip, owner, n.Name)
			}
			seenIPs[ip] = n.Name
		}

		if p.Ceph.Enabled && p.Ceph.Provision {
			isStorage := false
			for _, capability := range n.Capabilities {
				if capability == CapabilityStorage {
					isStorage = true
					break
				}
			}
			if isStorage {
				if len(n.CephDevices) == 0 {
					return fmt.Errorf("Ceph provisioning is enabled but storage node %s has no selected data devices", n.Name)
				}
				seenDevices := map[string]bool{}
				for _, device := range n.CephDevices {
					if !strings.HasPrefix(device, "/dev/") || strings.ContainsAny(device, "\r\n\x00") {
						return fmt.Errorf("node %s has invalid Ceph device %q", n.Name, device)
					}
					if seenDevices[device] {
						return fmt.Errorf("node %s has duplicate Ceph device %s", n.Name, device)
					}
					seenDevices[device] = true
				}
			}
		}

		for _, c := range n.Capabilities {
			switch c {
			case CapabilityControl:
				controlCount++
			case CapabilityStorage:
				storageCount++
			}
		}
	}

	if p.Installation != nil && controlCount != 1 && controlCount != 3 && controlCount != 5 {
		return fmt.Errorf("CONTROL node count must be 1, 3 or 5")
	}
	if controlCount == 0 {
		return errors.New("at least one CONTROL-capable node is required")
	}

	if p.Ceph.Enabled {
		if storageCount < 1 {
			return errors.New("Ceph is enabled but no STORAGE-capable node exists")
		}
		if err := validateCIDR("Ceph public CIDR", p.Ceph.PublicCIDR); err != nil {
			return err
		}
		if err := validateCIDR("Ceph cluster CIDR", p.Ceph.ClusterCIDR); err != nil {
			return err
		}
		if p.Ceph.Replication < 1 {
			return errors.New("Ceph replication must be at least 1")
		}
		if p.Ceph.Replication > storageCount {
			return fmt.Errorf("Ceph replication %d exceeds storage node count %d", p.Ceph.Replication, storageCount)
		}
	}

	return nil
}

func (p RealmPlan) ControlNodes() []NodeSpec {
	return nodesWithCapability(p.Nodes, CapabilityControl)
}

func (p RealmPlan) StorageNodes() []NodeSpec {
	return nodesWithCapability(p.Nodes, CapabilityStorage)
}

func nodesWithCapability(nodes []NodeSpec, wanted Capability) []NodeSpec {
	out := make([]NodeSpec, 0)
	for _, n := range nodes {
		for _, c := range n.Capabilities {
			if c == wanted {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

func (p RealmPlan) Save(path string) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0600)
}

func LoadPlan(path string) (RealmPlan, error) {
	var p RealmPlan
	data, err := os.ReadFile(path)
	if err != nil {
		return p, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&p); err != nil {
		return p, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return p, fmt.Errorf("trailing plan content")
	}
	p.Normalize()
	return p, p.Validate()
}

func ParseCapabilities(input string) ([]Capability, error) {
	parts := strings.Split(input, ",")
	seen := map[Capability]bool{}
	caps := make([]Capability, 0, len(parts))
	for _, raw := range parts {
		c := Capability(strings.ToUpper(strings.TrimSpace(raw)))
		switch c {
		case CapabilityControl, CapabilityExecution, CapabilityStorage, CapabilityGateway, CapabilityGPU, CapabilityBackup:
		default:
			return nil, fmt.Errorf("unknown capability %q", raw)
		}
		if !seen[c] {
			seen[c] = true
			caps = append(caps, c)
		}
	}
	sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
	return caps, nil
}

func validateCIDR(label, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", label)
	}
	if _, _, err := net.ParseCIDR(value); err != nil {
		return fmt.Errorf("invalid %s %q: %w", label, value, err)
	}
	return nil
}
