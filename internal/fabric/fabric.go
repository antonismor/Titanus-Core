package fabric

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type Peer struct {
	NodeID string `json:"node_id"`
	VTEP   string `json:"vtep"`
	CIDR   string `json:"cidr"`
}

type Config struct {
	Bridge         string `json:"bridge"`
	CIDR           string `json:"cidr"`
	Gateway        string `json:"gateway"`
	ServiceCIDR    string `json:"service_cidr,omitempty"`
	Mode           string `json:"mode,omitempty"`
	MTU            int    `json:"mtu,omitempty"`
	VXLANInterface string `json:"vxlan_interface,omitempty"`
	VXLANID        int    `json:"vxlan_id,omitempty"`
	VTEP           string `json:"vtep,omitempty"`
	Peers          []Peer `json:"peers,omitempty"`
}

type Port struct {
	Protocol      string `json:"protocol"`
	HostPort      int    `json:"host_port"`
	ContainerPort int    `json:"container_port"`
}

type Allocation struct {
	UnitID  string `json:"unit_id"`
	Address string `json:"address"`
	HostIf  string `json:"host_if"`
	Active  bool   `json:"active"`
	Ports   []Port `json:"ports,omitempty"`
}

type ServiceBackend struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
}

type Service struct {
	Name     string           `json:"name"`
	Address  string           `json:"address"`
	Protocol string           `json:"protocol"`
	Port     int              `json:"port"`
	Backends []ServiceBackend `json:"backends,omitempty"`
}

type PolicyRule struct {
	Sources   []string `json:"sources,omitempty"`
	AnySource bool     `json:"any_source,omitempty"`
	Protocol  string   `json:"protocol"`
	Ports     []int    `json:"ports,omitempty"`
}

type EgressRule struct {
	Destinations   []string `json:"destinations,omitempty"`
	AnyDestination bool     `json:"any_destination,omitempty"`
	Protocol       string   `json:"protocol"`
	Ports          []int    `json:"ports,omitempty"`
}

type Policy struct {
	Name              string       `json:"name"`
	Destinations      []string     `json:"destinations,omitempty"`
	Sources           []string     `json:"sources,omitempty"`
	DefaultDeny       bool         `json:"default_deny"`
	DefaultDenyEgress bool         `json:"default_deny_egress,omitempty"`
	Rules             []PolicyRule `json:"rules,omitempty"`
	Egress            []EgressRule `json:"egress,omitempty"`
}

type state struct {
	Allocations map[string]Allocation `json:"allocations"`
}

type Manager struct {
	StateRoot string
}

func NewManager(stateRoot string) *Manager {
	if strings.TrimSpace(stateRoot) == "" {
		stateRoot = "/var/lib/titanus"
	}
	return &Manager{StateRoot: filepath.Clean(stateRoot)}
}

func (m *Manager) Init(cidr, bridge string) (Config, error) {
	if bridge == "" {
		bridge = "titanus0"
	}
	if len(bridge) > 15 {
		return Config{}, fmt.Errorf("bridge name %q exceeds Linux interface-name limit", bridge)
	}
	ip, network, err := net.ParseCIDR(cidr)
	if err != nil {
		return Config{}, fmt.Errorf("invalid Fabric CIDR: %w", err)
	}
	if ip.To4() == nil {
		return Config{}, fmt.Errorf("Titanus Fabric v1 currently requires IPv4")
	}
	gatewayIP := addIPv4(network.IP.To4(), 1)
	if gatewayIP == nil || !network.Contains(gatewayIP) {
		return Config{}, fmt.Errorf("unable to derive gateway from %s", cidr)
	}
	ones, _ := network.Mask.Size()
	cfg := Config{
		Bridge:  bridge,
		CIDR:    network.String(),
		Gateway: fmt.Sprintf("%s/%d", gatewayIP.String(), ones),
		Mode:    "local",
		MTU:     1450,
	}
	if err := os.MkdirAll(m.fabricDir(), 0750); err != nil {
		return Config{}, err
	}
	if err := writeJSON(m.configPath(), cfg, 0600); err != nil {
		return Config{}, err
	}
	if _, err := os.Stat(m.statePath()); errors.Is(err, os.ErrNotExist) {
		if err := writeJSON(m.statePath(), state{Allocations: map[string]Allocation{}}, 0600); err != nil {
			return Config{}, err
		}
	}
	if os.Geteuid() == 0 {
		if err := m.ensureHostFabric(cfg); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

func (m *Manager) ConfigureMesh(localVTEP string, vxlanID int, peers []Peer) (Config, error) {
	if os.Geteuid() != 0 {
		return Config{}, fmt.Errorf("Realm Fabric mesh configuration requires root")
	}
	if ip := net.ParseIP(localVTEP); ip == nil || ip.To4() == nil {
		return Config{}, fmt.Errorf("invalid IPv4 local VTEP address %q", localVTEP)
	}
	if vxlanID < 1 || vxlanID > 16777215 {
		return Config{}, fmt.Errorf("invalid VXLAN ID %d", vxlanID)
	}
	cfg, err := m.Config()
	if err != nil {
		return Config{}, err
	}
	if cfg.MTU == 0 {
		cfg.MTU = 1450
	}

	oldPeers := append([]Peer(nil), cfg.Peers...)
	previousInterface := cfg.VXLANInterface
	previousID := cfg.VXLANID
	previousVTEP := cfg.VTEP

	cfg.Mode = "realm"
	cfg.VXLANInterface = "titanusvx"
	cfg.VXLANID = vxlanID
	cfg.VTEP = localVTEP
	cfg.Peers = normalizedPeers(peers, localVTEP)

	if err := m.ensureHostFabric(cfg); err != nil {
		return Config{}, err
	}

	needsCreate := previousInterface != cfg.VXLANInterface ||
		previousID != vxlanID ||
		previousVTEP != localVTEP
	if _, err := runOutput("ip", "link", "show", cfg.VXLANInterface); err != nil {
		needsCreate = true
	}

	if needsCreate {
		_, _ = runOutput("ip", "link", "del", cfg.VXLANInterface)
		args := []string{
			"link", "add", cfg.VXLANInterface, "type", "vxlan",
			"id", strconv.Itoa(vxlanID),
			"local", localVTEP,
			"dstport", "4789",
			"nolearning",
		}
		if out, err := runOutput("ip", args...); err != nil {
			return Config{}, fmt.Errorf("create Titanus VXLAN: %w: %s", err, out)
		}
	}
	if out, err := runOutput("ip", "link", "set", cfg.VXLANInterface, "mtu", strconv.Itoa(cfg.MTU)); err != nil {
		return Config{}, fmt.Errorf("set VXLAN MTU: %w: %s", err, out)
	}
	if out, err := runOutput("ip", "link", "set", cfg.VXLANInterface, "master", cfg.Bridge); err != nil {
		return Config{}, fmt.Errorf("attach VXLAN to Fabric bridge: %w: %s", err, out)
	}
	if out, err := runOutput("ip", "link", "set", cfg.VXLANInterface, "up"); err != nil {
		return Config{}, fmt.Errorf("raise VXLAN interface: %w: %s", err, out)
	}

	if !needsCreate {
		for _, peer := range oldPeers {
			if peer.CIDR != "" && !containsPeerCIDR(cfg.Peers, peer.CIDR) {
				_, _ = runOutput("ip", "route", "del", peer.CIDR, "dev", cfg.Bridge)
			}
		}
		_, _ = runOutput("bridge", "fdb", "flush", "dev", cfg.VXLANInterface)
	}

	for _, peer := range cfg.Peers {
		if out, err := runOutput("bridge", "fdb", "append", "00:00:00:00:00:00", "dev", cfg.VXLANInterface, "dst", peer.VTEP); err != nil {
			return Config{}, fmt.Errorf("add VXLAN peer %s: %w: %s", peer.NodeID, err, out)
		}
		if out, err := runOutput("ip", "route", "replace", peer.CIDR, "dev", cfg.Bridge); err != nil {
			return Config{}, fmt.Errorf("add Fabric route to %s: %w: %s", peer.CIDR, err, out)
		}
	}
	if err := writeJSON(m.configPath(), cfg, 0600); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func normalizedPeers(peers []Peer, localVTEP string) []Peer {
	out := make([]Peer, 0, len(peers))
	seen := map[string]bool{}
	for _, peer := range peers {
		peer.NodeID = strings.TrimSpace(peer.NodeID)
		peer.VTEP = strings.TrimSpace(peer.VTEP)
		peer.CIDR = strings.TrimSpace(peer.CIDR)
		if peer.VTEP == "" || peer.VTEP == localVTEP {
			continue
		}
		if ip := net.ParseIP(peer.VTEP); ip == nil || ip.To4() == nil {
			continue
		}
		if ip, network, err := net.ParseCIDR(peer.CIDR); err != nil || ip.To4() == nil {
			continue
		} else {
			peer.CIDR = network.String()
		}
		key := peer.VTEP + "|" + peer.CIDR
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, peer)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].NodeID == out[j].NodeID {
			return out[i].CIDR < out[j].CIDR
		}
		return out[i].NodeID < out[j].NodeID
	})
	return out
}

func containsPeerCIDR(peers []Peer, cidr string) bool {
	for _, peer := range peers {
		if peer.CIDR == cidr {
			return true
		}
	}
	return false
}

func (m *Manager) Config() (Config, error) {
	var cfg Config
	if err := readJSON(m.configPath(), &cfg); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cfg, fmt.Errorf("Titanus Fabric is not initialized")
		}
		return cfg, err
	}
	return cfg, nil
}

func (m *Manager) Reserve(unitID string) (Allocation, error) {
	cfg, err := m.Config()
	if err != nil {
		return Allocation{}, err
	}
	unlock, err := m.lock()
	if err != nil {
		return Allocation{}, err
	}
	defer unlock()

	st, err := m.loadState()
	if err != nil {
		return Allocation{}, err
	}
	if existing, ok := st.Allocations[unitID]; ok {
		return existing, nil
	}

	_, network, err := net.ParseCIDR(cfg.CIDR)
	if err != nil {
		return Allocation{}, err
	}
	used := map[string]bool{}
	for _, allocation := range st.Allocations {
		used[allocation.Address] = true
	}
	gatewayIP, _, _ := net.ParseCIDR(cfg.Gateway)
	if gatewayIP != nil {
		used[gatewayIP.String()] = true
	}

	var selected net.IP
	for offset := uint32(10); offset < 1<<24; offset++ {
		candidate := addIPv4(network.IP.To4(), offset)
		if candidate == nil || !network.Contains(candidate) {
			break
		}
		if isIPv4Broadcast(candidate, network) {
			continue
		}
		if !used[candidate.String()] {
			selected = candidate
			break
		}
	}
	if selected == nil {
		return Allocation{}, fmt.Errorf("Fabric %s has no free Unit addresses", cfg.CIDR)
	}

	allocation := Allocation{
		UnitID:  unitID,
		Address: selected.String(),
		HostIf:  interfaceName(unitID, "th"),
	}
	st.Allocations[unitID] = allocation
	if err := writeJSON(m.statePath(), st, 0600); err != nil {
		return Allocation{}, err
	}
	return allocation, nil
}

func (m *Manager) Attach(unitID string, pid int, ports []Port) (Allocation, error) {
	if os.Geteuid() != 0 {
		return Allocation{}, fmt.Errorf("Fabric attachment requires root")
	}
	if pid <= 0 {
		return Allocation{}, fmt.Errorf("invalid Unit PID %d", pid)
	}
	cfg, err := m.Config()
	if err != nil {
		return Allocation{}, err
	}
	if err := validatePorts(ports); err != nil {
		return Allocation{}, err
	}
	ports = normalizePorts(ports)
	if err := m.ensureHostFabric(cfg); err != nil {
		return Allocation{}, err
	}

	allocation, err := m.Reserve(unitID)
	if err != nil {
		return Allocation{}, err
	}
	mac, err := ethernetAddress(allocation.Address)
	if err != nil {
		return Allocation{}, err
	}
	peer := interfaceName(unitID, "tp")

	_ = run("ip", "link", "del", allocation.HostIf)
	if out, err := runOutput("ip", "link", "add", allocation.HostIf, "type", "veth", "peer", "name", peer); err != nil {
		return Allocation{}, fmt.Errorf("create veth pair: %w: %s", err, out)
	}
	cleanup := func() { _, _ = runOutput("ip", "link", "del", allocation.HostIf) }

	if out, err := runOutput("ip", "link", "set", allocation.HostIf, "master", cfg.Bridge); err != nil {
		cleanup()
		return Allocation{}, fmt.Errorf("attach host veth to bridge: %w: %s", err, out)
	}
	if out, err := runOutput("ip", "link", "set", allocation.HostIf, "mtu", strconv.Itoa(cfg.MTU)); err != nil {
		cleanup()
		return Allocation{}, fmt.Errorf("set host veth MTU: %w: %s", err, out)
	}
	if out, err := runOutput("ip", "link", "set", allocation.HostIf, "up"); err != nil {
		cleanup()
		return Allocation{}, fmt.Errorf("raise host veth: %w: %s", err, out)
	}
	if out, err := runOutput("ip", "link", "set", peer, "netns", strconv.Itoa(pid)); err != nil {
		cleanup()
		return Allocation{}, fmt.Errorf("move peer into Unit netns: %w: %s", err, out)
	}

	_, network, _ := net.ParseCIDR(cfg.CIDR)
	ones, _ := network.Mask.Size()
	gatewayIP, _, _ := net.ParseCIDR(cfg.Gateway)

	commands := [][]string{
		{"-t", strconv.Itoa(pid), "-n", "--", "ip", "link", "set", peer, "name", "eth0"},
		{"-t", strconv.Itoa(pid), "-n", "--", "ip", "link", "set", "eth0", "address", mac},
		{"-t", strconv.Itoa(pid), "-n", "--", "ip", "addr", "add", fmt.Sprintf("%s/%d", allocation.Address, ones), "dev", "eth0"},
		{"-t", strconv.Itoa(pid), "-n", "--", "ip", "link", "set", "eth0", "mtu", strconv.Itoa(cfg.MTU)},
		{"-t", strconv.Itoa(pid), "-n", "--", "ip", "link", "set", "eth0", "up"},
		{"-t", strconv.Itoa(pid), "-n", "--", "ip", "route", "replace", "default", "via", gatewayIP.String()},
	}
	for _, args := range commands {
		if out, err := runOutput("nsenter", args...); err != nil {
			cleanup()
			return Allocation{}, fmt.Errorf("configure Unit network: %w: %s", err, out)
		}
	}

	unlock, err := m.lock()
	if err != nil {
		cleanup()
		return Allocation{}, err
	}
	st, err := m.loadState()
	if err != nil {
		unlock()
		cleanup()
		return Allocation{}, err
	}
	if err := ensureNoPortConflict(st, unitID, ports); err != nil {
		unlock()
		cleanup()
		return Allocation{}, err
	}
	allocation.Active = true
	allocation.Ports = append([]Port(nil), ports...)
	st.Allocations[unitID] = allocation
	if err := writeJSON(m.statePath(), st, 0600); err != nil {
		unlock()
		cleanup()
		return Allocation{}, err
	}
	unlock()

	if err := m.reconcileNAT(cfg); err != nil {
		_ = m.Detach(unitID)
		return Allocation{}, err
	}
	return allocation, nil
}

func (m *Manager) Detach(unitID string) error {
	cfg, err := m.Config()
	if err != nil {
		return err
	}
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	st, err := m.loadState()
	if err != nil {
		unlock()
		return err
	}
	allocation, ok := st.Allocations[unitID]
	if ok {
		allocation.Active = false
		allocation.Ports = nil
		st.Allocations[unitID] = allocation
		if err := writeJSON(m.statePath(), st, 0600); err != nil {
			unlock()
			return err
		}
	}
	unlock()
	if ok && allocation.HostIf != "" {
		_, _ = runOutput("ip", "link", "del", allocation.HostIf)
	}
	return m.reconcileNAT(cfg)
}

func (m *Manager) Release(unitID string) error {
	cfg, err := m.Config()
	if err != nil {
		return err
	}
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	st, err := m.loadState()
	if err != nil {
		unlock()
		return err
	}
	allocation, ok := st.Allocations[unitID]
	if ok {
		delete(st.Allocations, unitID)
		if err := writeJSON(m.statePath(), st, 0600); err != nil {
			unlock()
			return err
		}
	}
	unlock()
	if ok && allocation.HostIf != "" {
		_, _ = runOutput("ip", "link", "del", allocation.HostIf)
	}
	return m.reconcileNAT(cfg)
}

func (m *Manager) Allocations() ([]Allocation, error) {
	st, err := m.loadState()
	if err != nil {
		return nil, err
	}
	out := make([]Allocation, 0, len(st.Allocations))
	for _, allocation := range st.Allocations {
		out = append(out, allocation)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UnitID < out[j].UnitID })
	return out, nil
}

func (m *Manager) ConfigureServices(serviceCIDR string, services []Service) error {
	cfg, err := m.Config()
	if err != nil {
		return err
	}
	ip, network, err := net.ParseCIDR(strings.TrimSpace(serviceCIDR))
	if err != nil || ip.To4() == nil {
		return fmt.Errorf("invalid IPv4 Service CIDR %q", serviceCIDR)
	}
	normalized, err := normalizeServices(network, services)
	if err != nil {
		return err
	}

	unlock, err := m.lock()
	if err != nil {
		return err
	}
	cfg.ServiceCIDR = network.String()
	if err := writeJSON(m.configPath(), cfg, 0600); err != nil {
		unlock()
		return err
	}
	if err := writeJSON(m.servicesPath(), normalized, 0600); err != nil {
		unlock()
		return err
	}
	unlock()

	if os.Geteuid() == 0 {
		return m.reconcileNAT(cfg)
	}
	return nil
}

func (m *Manager) Services() ([]Service, error) {
	services, err := m.loadServices()
	if err != nil {
		return nil, err
	}
	return append([]Service(nil), services...), nil
}

func (m *Manager) ConfigurePolicies(policies []Policy) error {
	normalized, err := normalizePolicies(policies)
	if err != nil {
		return err
	}
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	if err := writeJSON(m.policiesPath(), normalized, 0600); err != nil {
		unlock()
		return err
	}
	unlock()
	if os.Geteuid() == 0 {
		return m.reconcileFilter()
	}
	return nil
}

func (m *Manager) Policies() ([]Policy, error) {
	policies, err := m.loadPolicies()
	if err != nil {
		return nil, err
	}
	return append([]Policy(nil), policies...), nil
}

func normalizePolicies(policies []Policy) ([]Policy, error) {
	out := make([]Policy, 0, len(policies))
	for _, policy := range policies {
		policy.Name = strings.TrimSpace(policy.Name)
		if policy.Name == "" {
			return nil, fmt.Errorf("Fabric Policy name is required")
		}

		var err error
		policy.Destinations, err = normalizePolicyAddresses(policy.Name, "destination", policy.Destinations)
		if err != nil {
			return nil, err
		}
		policy.Sources, err = normalizePolicyAddresses(policy.Name, "source", policy.Sources)
		if err != nil {
			return nil, err
		}

		ingress := make([]PolicyRule, 0, len(policy.Rules))
		for index, rule := range policy.Rules {
			rule.Protocol, err = normalizePolicyProtocol(policy.Name, "ingress", index, rule.Protocol)
			if err != nil {
				return nil, err
			}
			if rule.AnySource && len(rule.Sources) > 0 {
				return nil, fmt.Errorf("Fabric Policy %s ingress rule %d mixes any-source with source CIDRs", policy.Name, index+1)
			}
			rule.Sources, err = normalizePolicyCIDRs(policy.Name, "ingress source", index, rule.Sources)
			if err != nil {
				return nil, err
			}
			if !rule.AnySource && len(rule.Sources) == 0 {
				ingress = append(ingress, rule)
				continue
			}
			rule.Ports, err = normalizePolicyPorts(policy.Name, "ingress", index, rule.Protocol, rule.Ports)
			if err != nil {
				return nil, err
			}
			ingress = append(ingress, rule)
		}
		policy.Rules = ingress

		egress := make([]EgressRule, 0, len(policy.Egress))
		for index, rule := range policy.Egress {
			rule.Protocol, err = normalizePolicyProtocol(policy.Name, "egress", index, rule.Protocol)
			if err != nil {
				return nil, err
			}
			if rule.AnyDestination && len(rule.Destinations) > 0 {
				return nil, fmt.Errorf("Fabric Policy %s egress rule %d mixes any-destination with destination CIDRs", policy.Name, index+1)
			}
			rule.Destinations, err = normalizePolicyCIDRs(policy.Name, "egress destination", index, rule.Destinations)
			if err != nil {
				return nil, err
			}
			if !rule.AnyDestination && len(rule.Destinations) == 0 {
				egress = append(egress, rule)
				continue
			}
			rule.Ports, err = normalizePolicyPorts(policy.Name, "egress", index, rule.Protocol, rule.Ports)
			if err != nil {
				return nil, err
			}
			egress = append(egress, rule)
		}
		policy.Egress = egress
		out = append(out, policy)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func normalizePolicyAddresses(policyName, label string, values []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		ip := net.ParseIP(strings.TrimSpace(value))
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("Fabric Policy %s has invalid %s %q", policyName, label, value)
		}
		text := ip.To4().String()
		if !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
	}
	sort.Strings(out)
	return out, nil
}

func normalizePolicyCIDRs(policyName, label string, index int, values []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		ip, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err != nil || ip.To4() == nil {
			return nil, fmt.Errorf("Fabric Policy %s %s rule %d has invalid IPv4 CIDR %q", policyName, label, index+1, value)
		}
		text := network.String()
		if !seen[text] {
			seen[text] = true
			out = append(out, text)
		}
	}
	sort.Strings(out)
	return out, nil
}

func normalizePolicyProtocol(policyName, direction string, index int, protocol string) (string, error) {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol == "" {
		protocol = "tcp"
	}
	if protocol != "tcp" && protocol != "udp" && protocol != "any" {
		return "", fmt.Errorf("Fabric Policy %s %s rule %d has unsupported protocol %q", policyName, direction, index+1, protocol)
	}
	return protocol, nil
}

func normalizePolicyPorts(policyName, direction string, index int, protocol string, values []int) ([]int, error) {
	seen := map[int]bool{}
	ports := make([]int, 0, len(values))
	for _, port := range values {
		if port < 1 || port > 65535 {
			return nil, fmt.Errorf("Fabric Policy %s %s rule %d has invalid port %d", policyName, direction, index+1, port)
		}
		if !seen[port] {
			seen[port] = true
			ports = append(ports, port)
		}
	}
	sort.Ints(ports)
	if protocol == "any" && len(ports) > 0 {
		return nil, fmt.Errorf("Fabric Policy %s %s rule %d cannot combine protocol any with ports", policyName, direction, index+1)
	}
	return ports, nil
}

func normalizeServices(network *net.IPNet, services []Service) ([]Service, error) {
	out := make([]Service, 0, len(services))
	seenEndpoint := map[string]string{}
	for _, service := range services {
		service.Name = strings.TrimSpace(service.Name)
		service.Address = strings.TrimSpace(service.Address)
		service.Protocol = strings.ToLower(strings.TrimSpace(service.Protocol))
		if service.Protocol == "" {
			service.Protocol = "tcp"
		}
		if service.Name == "" {
			return nil, fmt.Errorf("Fabric Service name is required")
		}
		ip := net.ParseIP(service.Address)
		if ip == nil || ip.To4() == nil || !network.Contains(ip.To4()) {
			return nil, fmt.Errorf("Fabric Service %s address %q is outside %s", service.Name, service.Address, network.String())
		}
		service.Address = ip.To4().String()
		if service.Protocol != "tcp" && service.Protocol != "udp" {
			return nil, fmt.Errorf("Fabric Service %s has unsupported protocol %q", service.Name, service.Protocol)
		}
		if service.Port < 1 || service.Port > 65535 {
			return nil, fmt.Errorf("Fabric Service %s has invalid port %d", service.Name, service.Port)
		}
		endpointKey := fmt.Sprintf("%s/%s/%d", service.Address, service.Protocol, service.Port)
		if owner, ok := seenEndpoint[endpointKey]; ok {
			return nil, fmt.Errorf("Fabric Services %s and %s share endpoint %s", owner, service.Name, endpointKey)
		}
		seenEndpoint[endpointKey] = service.Name

		seenBackend := map[string]bool{}
		backends := make([]ServiceBackend, 0, len(service.Backends))
		backendPort := 0
		for _, backend := range service.Backends {
			ip := net.ParseIP(strings.TrimSpace(backend.Address))
			if ip == nil || ip.To4() == nil {
				return nil, fmt.Errorf("Fabric Service %s has invalid backend address %q", service.Name, backend.Address)
			}
			if backend.Port < 1 || backend.Port > 65535 {
				return nil, fmt.Errorf("Fabric Service %s has invalid backend port %d", service.Name, backend.Port)
			}
			if backendPort == 0 {
				backendPort = backend.Port
			} else if backend.Port != backendPort {
				return nil, fmt.Errorf("Fabric Service %s requires one target port across all backends", service.Name)
			}
			backend.Address = ip.To4().String()
			key := fmt.Sprintf("%s:%d", backend.Address, backend.Port)
			if seenBackend[key] {
				continue
			}
			seenBackend[key] = true
			backends = append(backends, backend)
		}
		sort.Slice(backends, func(i, j int) bool {
			if backends[i].Address == backends[j].Address {
				return backends[i].Port < backends[j].Port
			}
			return backends[i].Address < backends[j].Address
		})
		service.Backends = backends
		out = append(out, service)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// A reused Fabric IP retains its Ethernet identity across Unit replacement.
// The locally administered unicast prefix plus all four IPv4 octets is unique
// within the Realm; hashing a Unit ID or accepting random veth MACs is not.
func ethernetAddress(address string) (string, error) {
	ip := net.ParseIP(address).To4()
	if ip == nil {
		return "", fmt.Errorf("invalid Fabric IPv4 address %q", address)
	}
	return fmt.Sprintf("02:54:%02x:%02x:%02x:%02x", ip[0], ip[1], ip[2], ip[3]), nil
}

func (m *Manager) ensureHostFabric(cfg Config) error {
	for _, binary := range []string{"ip", "bridge", "nft", "nsenter"} {
		if _, err := exec.LookPath(binary); err != nil {
			return fmt.Errorf("%s is required for Titanus Fabric v1: %w", binary, err)
		}
	}
	if _, err := runOutput("ip", "link", "show", cfg.Bridge); err != nil {
		if out, createErr := runOutput("ip", "link", "add", cfg.Bridge, "type", "bridge"); createErr != nil {
			return fmt.Errorf("create Fabric bridge: %w: %s", createErr, out)
		}
	}
	gateway, _, err := net.ParseCIDR(cfg.Gateway)
	if err != nil {
		return fmt.Errorf("invalid Fabric gateway: %w", err)
	}
	mac, err := ethernetAddress(gateway.String())
	if err != nil {
		return err
	}
	// Explicit assignment disables bridge MAC changes as veth ports come/go.
	if out, err := runOutput("ip", "link", "set", cfg.Bridge, "address", mac); err != nil {
		return fmt.Errorf("pin Fabric gateway Ethernet address: %w: %s", err, out)
	}
	if cfg.MTU == 0 {
		cfg.MTU = 1450
	}
	if out, err := runOutput("ip", "link", "set", cfg.Bridge, "mtu", strconv.Itoa(cfg.MTU)); err != nil {
		return fmt.Errorf("configure Fabric bridge MTU: %w: %s", err, out)
	}
	if out, err := runOutput("ip", "addr", "replace", cfg.Gateway, "dev", cfg.Bridge); err != nil {
		return fmt.Errorf("configure Fabric gateway: %w: %s", err, out)
	}
	if out, err := runOutput("ip", "link", "set", cfg.Bridge, "up"); err != nil {
		return fmt.Errorf("raise Fabric bridge: %w: %s", err, out)
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0644); err != nil {
		return fmt.Errorf("enable IPv4 forwarding: %w", err)
	}
	return m.reconcileNAT(cfg)
}

func (m *Manager) reconcileNAT(cfg Config) error {
	if os.Geteuid() != 0 {
		return nil
	}
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()

	st, err := m.loadState()
	if err != nil {
		return err
	}
	services, err := m.loadServices()
	if err != nil {
		return err
	}
	rules := renderNATTransaction(cfg, st, services)

	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("reconcile nftables: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func renderNATTransaction(cfg Config, st state, services []Service) string {
	// Add is idempotent for an existing table. Keep creation, deletion and
	// replacement in one nft transaction: packets never see an absent table,
	// and a rejected replacement retains the previously enforced rules.
	return "add table ip titanus_nat\ndelete table ip titanus_nat\n" + renderNATRules(cfg, st, services)
}

func renderNATRules(cfg Config, st state, services []Service) string {
	var b strings.Builder
	b.WriteString("table ip titanus_nat {\n")
	b.WriteString(" chain prerouting { type nat hook prerouting priority dstnat; policy accept;\n")
	writeServiceRules(&b, services)
	for _, allocation := range sortedActive(st) {
		for _, port := range allocation.Ports {
			fmt.Fprintf(&b, "  %s dport %d dnat to %s:%d comment \"Titanus:%s\"\n",
				port.Protocol, port.HostPort, allocation.Address, port.ContainerPort, allocation.UnitID)
		}
	}
	b.WriteString(" }\n")
	b.WriteString(" chain output { type nat hook output priority dstnat; policy accept;\n")
	writeServiceRules(&b, services)
	for _, allocation := range sortedActive(st) {
		for _, port := range allocation.Ports {
			fmt.Fprintf(&b, "  ip daddr 127.0.0.1 %s dport %d dnat to %s:%d comment \"Titanus:%s\"\n",
				port.Protocol, port.HostPort, allocation.Address, port.ContainerPort, allocation.UnitID)
		}
	}
	b.WriteString(" }\n")
	b.WriteString(" chain postrouting { type nat hook postrouting priority srcnat; policy accept;\n")
	// Host-originated Service connections initially select an underlay source.
	// Remote backends must reply through this node's Fabric/conntrack path,
	// rather than bypassing it via the underlay. Preserve workload source IPs:
	// fib type local matches addresses owned by the node, not routed Units.
	for _, service := range services {
		if len(service.Backends) > 0 {
			fmt.Fprintf(&b, "  ct status dnat ct original ip daddr %s meta l4proto %s ct original proto-dst %d fib saddr type local masquerade comment \"Titanus node Service return path\"\n", service.Address, service.Protocol, service.Port)
		}
	}
	fmt.Fprintf(&b, "  ip saddr %s oifname != \"%s\" masquerade comment \"Titanus Fabric NAT\"\n", cfg.CIDR, cfg.Bridge)
	b.WriteString(" }\n")
	b.WriteString("}\n")
	return b.String()
}

func writeServiceRules(b *strings.Builder, services []Service) {
	for _, service := range services {
		if len(service.Backends) == 0 {
			continue
		}
		h := fnv.New32a()
		_, _ = h.Write([]byte(service.Name))
		if len(service.Backends) == 1 {
			backend := service.Backends[0]
			fmt.Fprintf(b, "  ip daddr %s %s dport %d dnat to %s:%d comment \"Titanus:service:%08x\"\n",
				service.Address, service.Protocol, service.Port, backend.Address, backend.Port, h.Sum32())
			continue
		}
		fmt.Fprintf(b, "  ip daddr %s %s dport %d dnat to numgen inc mod %d map { ",
			service.Address, service.Protocol, service.Port, len(service.Backends))
		for i, backend := range service.Backends {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(b, "%d : %s", i, backend.Address)
		}
		fmt.Fprintf(b, " } : %d comment \"Titanus:service:%08x\"\n",
			service.Backends[0].Port, h.Sum32())
	}
}

func (m *Manager) reconcileFilter() error {
	if os.Geteuid() != 0 {
		return nil
	}
	unlock, err := m.lock()
	if err != nil {
		return err
	}
	defer unlock()
	policies, err := m.loadPolicies()
	if err != nil {
		return err
	}
	_, _ = runOutput("nft", "delete", "table", "ip", "titanus_filter_ip")
	_, _ = runOutput("nft", "delete", "table", "bridge", "titanus_filter_bridge")
	if len(policies) == 0 {
		return nil
	}
	rules := renderFilterRules(policies)
	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("reconcile Fabric policy nftables: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func renderFilterRules(policies []Policy) string {
	var b strings.Builder
	writePolicyTable(&b, "bridge", "titanus_filter_bridge", policies)
	writePolicyTable(&b, "ip", "titanus_filter_ip", policies)
	return b.String()
}

type ingressPolicyBucket struct {
	Rules       []PolicyRule
	DefaultDeny bool
}

type egressPolicyBucket struct {
	Rules       []EgressRule
	DefaultDeny bool
}

func writePolicyTable(b *strings.Builder, family, table string, policies []Policy) {
	ingress := map[string]*ingressPolicyBucket{}
	egress := map[string]*egressPolicyBucket{}
	for _, policy := range policies {
		for _, destination := range policy.Destinations {
			bucket := ingress[destination]
			if bucket == nil {
				bucket = &ingressPolicyBucket{}
				ingress[destination] = bucket
			}
			bucket.Rules = append(bucket.Rules, policy.Rules...)
			bucket.DefaultDeny = bucket.DefaultDeny || policy.DefaultDeny
		}
		for _, source := range policy.Sources {
			bucket := egress[source]
			if bucket == nil {
				bucket = &egressPolicyBucket{}
				egress[source] = bucket
			}
			bucket.Rules = append(bucket.Rules, policy.Egress...)
			bucket.DefaultDeny = bucket.DefaultDeny || policy.DefaultDenyEgress
		}
	}

	ingressAddresses := make([]string, 0, len(ingress))
	for address := range ingress {
		ingressAddresses = append(ingressAddresses, address)
	}
	sort.Strings(ingressAddresses)
	egressAddresses := make([]string, 0, len(egress))
	for address := range egress {
		egressAddresses = append(egressAddresses, address)
	}
	sort.Strings(egressAddresses)

	fmt.Fprintf(b, "table %s %s {\n", family, table)
	b.WriteString(" chain forward { type filter hook forward priority 0; policy accept;\n")
	b.WriteString("  ct state established,related accept comment \"Titanus policy established\"\n")
	for _, destination := range ingressAddresses {
		fmt.Fprintf(b, "  ip daddr %s jump %s\n", destination, policyChainName("i", destination))
	}
	for _, source := range egressAddresses {
		fmt.Fprintf(b, "  ip saddr %s jump %s\n", source, policyChainName("e", source))
	}
	b.WriteString(" }\n")

	for _, destination := range ingressAddresses {
		bucket := ingress[destination]
		fmt.Fprintf(b, " chain %s {\n", policyChainName("i", destination))
		for _, rule := range bucket.Rules {
			writeIngressPolicyRule(b, rule)
		}
		if bucket.DefaultDeny {
			b.WriteString("  drop comment \"Titanus ingress default deny\"\n")
		}
		b.WriteString(" }\n")
	}
	for _, source := range egressAddresses {
		bucket := egress[source]
		fmt.Fprintf(b, " chain %s {\n", policyChainName("e", source))
		for _, rule := range bucket.Rules {
			writeEgressPolicyRule(b, rule)
		}
		if bucket.DefaultDeny {
			b.WriteString("  drop comment \"Titanus egress default deny\"\n")
		}
		b.WriteString(" }\n")
	}
	b.WriteString("}\n")
}

func policyChainName(direction, address string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(direction + ":" + address))
	return fmt.Sprintf("ti_%s_%08x", direction, h.Sum32())
}

func writeIngressPolicyRule(b *strings.Builder, rule PolicyRule) {
	if rule.AnySource {
		writePolicyReturn(b, "", "", rule.Protocol, rule.Ports, "Titanus ingress allow")
		return
	}
	for _, source := range rule.Sources {
		writePolicyReturn(b, source, "", rule.Protocol, rule.Ports, "Titanus ingress allow")
	}
}

func writeEgressPolicyRule(b *strings.Builder, rule EgressRule) {
	if rule.AnyDestination {
		writePolicyReturn(b, "", "", rule.Protocol, rule.Ports, "Titanus egress allow")
		return
	}
	for _, destination := range rule.Destinations {
		writePolicyReturn(b, "", destination, rule.Protocol, rule.Ports, "Titanus egress allow")
	}
}

func writePolicyReturn(b *strings.Builder, source, destination, protocol string, ports []int, comment string) {
	b.WriteString("  ")
	if source != "" {
		fmt.Fprintf(b, "ip saddr %s ", source)
	}
	if destination != "" {
		fmt.Fprintf(b, "ip daddr %s ", destination)
	}
	switch protocol {
	case "tcp", "udp":
		if len(ports) == 0 {
			fmt.Fprintf(b, "ip protocol %s ", protocol)
		} else if len(ports) == 1 {
			fmt.Fprintf(b, "%s dport %d ", protocol, ports[0])
		} else {
			fmt.Fprintf(b, "%s dport { ", protocol)
			for i, port := range ports {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(b, "%d", port)
			}
			b.WriteString(" } ")
		}
	}
	fmt.Fprintf(b, "return comment \"%s\"\n", comment)
}

func sortedActive(st state) []Allocation {
	items := make([]Allocation, 0)
	for _, allocation := range st.Allocations {
		if allocation.Active {
			items = append(items, allocation)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UnitID < items[j].UnitID })
	return items
}

func ValidatePorts(ports []Port) error {
	return validatePorts(ports)
}

func normalizePorts(ports []Port) []Port {
	out := make([]Port, len(ports))
	for i, port := range ports {
		port.Protocol = strings.ToLower(strings.TrimSpace(port.Protocol))
		if port.Protocol == "" {
			port.Protocol = "tcp"
		}
		out[i] = port
	}
	return out
}

func validatePorts(ports []Port) error {
	seen := map[string]bool{}
	for _, port := range ports {
		protocol := strings.ToLower(strings.TrimSpace(port.Protocol))
		if protocol == "" {
			protocol = "tcp"
		}
		if protocol != "tcp" && protocol != "udp" {
			return fmt.Errorf("unsupported port protocol %q", port.Protocol)
		}
		if port.HostPort < 1 || port.HostPort > 65535 || port.ContainerPort < 1 || port.ContainerPort > 65535 {
			return fmt.Errorf("invalid port mapping %d:%d", port.HostPort, port.ContainerPort)
		}
		key := fmt.Sprintf("%s/%d", protocol, port.HostPort)
		if seen[key] {
			return fmt.Errorf("duplicate port mapping %s", key)
		}
		seen[key] = true
	}
	return nil
}

func ensureNoPortConflict(st state, unitID string, ports []Port) error {
	for _, candidate := range ports {
		protocol := strings.ToLower(candidate.Protocol)
		if protocol == "" {
			protocol = "tcp"
		}
		for otherID, allocation := range st.Allocations {
			if otherID == unitID || !allocation.Active {
				continue
			}
			for _, existing := range allocation.Ports {
				existingProtocol := strings.ToLower(existing.Protocol)
				if existingProtocol == "" {
					existingProtocol = "tcp"
				}
				if protocol == existingProtocol && candidate.HostPort == existing.HostPort {
					return fmt.Errorf("host port %s/%d is already published by Unit %s", protocol, candidate.HostPort, otherID)
				}
			}
		}
	}
	return nil
}

func (m *Manager) loadState() (state, error) {
	var st state
	if err := readJSON(m.statePath(), &st); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			st.Allocations = map[string]Allocation{}
			return st, nil
		}
		return st, err
	}
	if st.Allocations == nil {
		st.Allocations = map[string]Allocation{}
	}
	return st, nil
}

func (m *Manager) lock() (func(), error) {
	if err := os.MkdirAll(m.fabricDir(), 0750); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(m.fabricDir(), ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func (m *Manager) fabricDir() string    { return filepath.Join(m.StateRoot, "fabric") }
func (m *Manager) configPath() string   { return filepath.Join(m.fabricDir(), "config.json") }
func (m *Manager) statePath() string    { return filepath.Join(m.fabricDir(), "allocations.json") }
func (m *Manager) servicesPath() string { return filepath.Join(m.fabricDir(), "services.json") }
func (m *Manager) policiesPath() string { return filepath.Join(m.fabricDir(), "policies.json") }

func (m *Manager) loadPolicies() ([]Policy, error) {
	var policies []Policy
	if err := readJSON(m.policiesPath(), &policies); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Policy{}, nil
		}
		return nil, err
	}
	return policies, nil
}

func (m *Manager) loadServices() ([]Service, error) {
	var services []Service
	if err := readJSON(m.servicesPath(), &services); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Service{}, nil
		}
		return nil, err
	}
	return services, nil
}

func writeJSON(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func interfaceName(unitID, prefix string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(unitID))
	return fmt.Sprintf("%s%08x", prefix, h.Sum32())[:10]
}

func addIPv4(ip net.IP, offset uint32) net.IP {
	if ip == nil || len(ip) < 4 {
		return nil
	}
	base := uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
	value := base + offset
	return net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
}

func isIPv4Broadcast(ip net.IP, network *net.IPNet) bool {
	ip4 := ip.To4()
	base := network.IP.To4()
	if ip4 == nil || base == nil {
		return false
	}
	mask := network.Mask
	for i := 0; i < 4; i++ {
		if ip4[i] != (base[i] | ^mask[i]) {
			return false
		}
	}
	return true
}

func run(name string, args ...string) error {
	_, err := runOutput(name, args...)
	return err
}

func runOutput(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func ParsePort(value string) (Port, error) {
	protocol := "tcp"
	text := strings.TrimSpace(value)
	if slash := strings.LastIndex(text, "/"); slash >= 0 {
		protocol = strings.ToLower(strings.TrimSpace(text[slash+1:]))
		text = text[:slash]
	}
	parts := strings.Split(text, ":")
	if len(parts) != 2 {
		return Port{}, fmt.Errorf("port mapping must be HOST:UNIT[/tcp|udp]")
	}
	host, err := strconv.Atoi(parts[0])
	if err != nil {
		return Port{}, fmt.Errorf("invalid host port %q", parts[0])
	}
	container, err := strconv.Atoi(parts[1])
	if err != nil {
		return Port{}, fmt.Errorf("invalid Unit port %q", parts[1])
	}
	port := Port{Protocol: protocol, HostPort: host, ContainerPort: container}
	return port, validatePorts([]Port{port})
}

func ReadPublishedPorts(path string) ([]Port, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var ports []Port
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		port, err := ParsePort(line)
		if err != nil {
			return nil, err
		}
		ports = append(ports, port)
	}
	return ports, scanner.Err()
}
