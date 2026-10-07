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

type Config struct {
	Bridge  string `json:"bridge"`
	CIDR    string `json:"cidr"`
	Gateway string `json:"gateway"`
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
	if err := m.ensureHostFabric(cfg); err != nil {
		return Allocation{}, err
	}

	allocation, err := m.Reserve(unitID)
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
		{"-t", strconv.Itoa(pid), "-n", "--", "ip", "addr", "add", fmt.Sprintf("%s/%d", allocation.Address, ones), "dev", "eth0"},
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

func (m *Manager) ensureHostFabric(cfg Config) error {
	for _, binary := range []string{"ip", "nft", "nsenter"} {
		if _, err := exec.LookPath(binary); err != nil {
			return fmt.Errorf("%s is required for Titanus Fabric v1: %w", binary, err)
		}
	}
	if _, err := runOutput("ip", "link", "show", cfg.Bridge); err != nil {
		if out, createErr := runOutput("ip", "link", "add", cfg.Bridge, "type", "bridge"); createErr != nil {
			return fmt.Errorf("create Fabric bridge: %w: %s", createErr, out)
		}
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
	st, err := m.loadState()
	if err != nil {
		return err
	}
	_, _ = runOutput("nft", "delete", "table", "ip", "titanus_nat")

	var b strings.Builder
	b.WriteString("table ip titanus_nat {\n")
	b.WriteString(" chain prerouting { type nat hook prerouting priority dstnat; policy accept;\n")
	for _, allocation := range sortedActive(st) {
		for _, port := range allocation.Ports {
			fmt.Fprintf(&b, "  %s dport %d dnat to %s:%d comment \"Titanus:%s\"\n",
				port.Protocol, port.HostPort, allocation.Address, port.ContainerPort, allocation.UnitID)
		}
	}
	b.WriteString(" }\n")
	b.WriteString(" chain output { type nat hook output priority dstnat; policy accept;\n")
	for _, allocation := range sortedActive(st) {
		for _, port := range allocation.Ports {
			fmt.Fprintf(&b, "  ip daddr 127.0.0.1 %s dport %d dnat to %s:%d comment \"Titanus:%s\"\n",
				port.Protocol, port.HostPort, allocation.Address, port.ContainerPort, allocation.UnitID)
		}
	}
	b.WriteString(" }\n")
	b.WriteString(" chain postrouting { type nat hook postrouting priority srcnat; policy accept;\n")
	fmt.Fprintf(&b, "  ip saddr %s oifname != \"%s\" masquerade comment \"Titanus Fabric NAT\"\n", cfg.CIDR, cfg.Bridge)
	b.WriteString(" }\n")
	b.WriteString("}\n")

	cmd := exec.Command("nft", "-f", "-")
	cmd.Stdin = strings.NewReader(b.String())
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("reconcile nftables: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
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

func (m *Manager) fabricDir() string { return filepath.Join(m.StateRoot, "fabric") }
func (m *Manager) configPath() string { return filepath.Join(m.fabricDir(), "config.json") }
func (m *Manager) statePath() string { return filepath.Join(m.fabricDir(), "allocations.json") }

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
