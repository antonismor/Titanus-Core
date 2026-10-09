package realm

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"github.com/antonismor/Titanus-Core/internal/security"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/fabric"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/version"
)

type NodeState string

const (
	NodeReady       NodeState = "READY"
	NodeSuspect     NodeState = "SUSPECT"
	NodeUnreachable NodeState = "UNREACHABLE"
	NodeDraining    NodeState = "DRAINING"
	NodeDisabled    NodeState = "DISABLED"
)

type Resources struct {
	CPUMilliCapacity int64 `json:"cpu_milli_capacity"`
	CPUMilliUsed     int64 `json:"cpu_milli_used"`
	MemoryBytes      int64 `json:"memory_bytes"`
	MemoryUsedBytes  int64 `json:"memory_used_bytes"`
	GPUCount         int   `json:"gpu_count"`
	UnitCount        int   `json:"unit_count"`
}

type RealmNetwork struct {
	FabricCIDR  string `json:"fabric_cidr"`
	ServiceCIDR string `json:"service_cidr"`
	NodePrefix  int    `json:"node_prefix"`
	VXLANID     int    `json:"vxlan_id"`
}

type Node struct {
	Compatibility      *version.Capabilities `json:"compatibility,omitempty"`
	PowerQuarantined   bool                  `json:"power_quarantined,omitempty"`
	StorageQuarantined bool                  `json:"storage_quarantined,omitempty"`
	ID                 string                `json:"id"`
	Address            string                `json:"address"`
	FabricAddress      string                `json:"fabric_address,omitempty"`
	FabricCIDR         string                `json:"fabric_cidr,omitempty"`
	Capabilities       []model.Capability    `json:"capabilities"`
	Labels             map[string]string     `json:"labels,omitempty"`
	Resources          Resources             `json:"resources"`
	State              NodeState             `json:"state"`
	LastPulse          time.Time             `json:"last_pulse"`
	JoinedAt           time.Time             `json:"joined_at"`
	Failures           uint64                `json:"failures"`
	Successes          uint64                `json:"successes"`
}

type UnitTemplate struct {
	Secrets     []secrets.Ref      `json:"secrets,omitempty"`
	Health      unitruntime.Health `json:"health"`
	Source      string             `json:"source"`
	Command     []string           `json:"command"`
	Environment []string           `json:"environment,omitempty"`
	MemoryBytes int64              `json:"memory_bytes"`
	CPUPercent  int                `json:"cpu_percent"`
	PidsMax     int                `json:"pids_max"`
	Fabric      bool               `json:"fabric"`
	Ports       []fabric.Port      `json:"ports,omitempty"`
	Mounts      []disk.Mount       `json:"mounts,omitempty"`
	Security    security.Policy    `json:"security"`
}

type Fleet struct {
	MaxSurge         int               `json:"max_surge"`
	History          []FleetRevision   `json:"history,omitempty"`
	Name             string            `json:"name"`
	Instances        int               `json:"instances"`
	MinimumAvailable int               `json:"minimum_available"`
	Template         UnitTemplate      `json:"template"`
	RequiredLabels   map[string]string `json:"required_labels,omitempty"`
	SpreadLabel      string            `json:"spread_label,omitempty"`
	Generation       uint64            `json:"generation"`
	UpdatedAt        time.Time         `json:"updated_at"`
}

type AssignmentState string

const (
	AssignmentPlanned  AssignmentState = "PLANNED"
	AssignmentStarting AssignmentState = "STARTING"
	AssignmentActive   AssignmentState = "ACTIVE"
	AssignmentImpaired AssignmentState = "IMPAIRED"
	AssignmentStopped  AssignmentState = "STOPPED"
)

type Route struct {
	Gateway    string    `json:"gateway,omitempty"`
	Name       string    `json:"name"`
	Fleet      string    `json:"fleet"`
	ServiceIP  string    `json:"service_ip,omitempty"`
	ListenIP   string    `json:"listen_ip"`
	ListenPort int       `json:"listen_port"`
	TargetPort int       `json:"target_port"`
	Protocol   string    `json:"protocol"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type NetworkPolicyRule struct {
	FromFleet string `json:"from_fleet,omitempty"`
	FromCIDR  string `json:"from_cidr,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	Ports     []int  `json:"ports,omitempty"`
}

type NetworkPolicyEgressRule struct {
	ToFleet  string `json:"to_fleet,omitempty"`
	ToCIDR   string `json:"to_cidr,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Ports    []int  `json:"ports,omitempty"`
}

type NetworkPolicy struct {
	Name              string                    `json:"name"`
	Fleet             string                    `json:"fleet"`
	DefaultDeny       bool                      `json:"default_deny"`
	DefaultDenyEgress bool                      `json:"default_deny_egress,omitempty"`
	Ingress           []NetworkPolicyRule       `json:"ingress,omitempty"`
	Egress            []NetworkPolicyEgressRule `json:"egress,omitempty"`
	CreatedAt         time.Time                 `json:"created_at"`
	UpdatedAt         time.Time                 `json:"updated_at"`
}

type Assignment struct {
	StorageWriters map[string]disk.Writer `json:"storage_writers,omitempty"`
	ID             string                 `json:"id"`
	Fleet          string                 `json:"fleet"`
	NodeID         string                 `json:"node_id"`
	State          AssignmentState        `json:"state"`
	Generation     uint64                 `json:"generation"`
	CreatedAt      time.Time              `json:"created_at"`
	UpdatedAt      time.Time              `json:"updated_at"`
	LeaseToken     string                 `json:"lease_token,omitempty"`
	LeaseExpiresAt time.Time              `json:"lease_expires_at,omitempty"`
	StartAfter     time.Time              `json:"start_after,omitempty"`
	NetworkAddress string                 `json:"network_address,omitempty"`
}

type State struct {
	TaskSchedules    map[string]TaskSchedule          `json:"task_schedules,omitempty"`
	SecretRotations  []SecretRotation                 `json:"secret_rotations,omitempty"`
	SchemaVersion    int                              `json:"schema_version,omitempty"`
	SchemaMigrations []SchemaMigration                `json:"schema_migrations,omitempty"`
	Gateways         map[string]Gateway               `json:"gateways,omitempty"`
	GatewayTransfers map[string]GatewayTransfer       `json:"gateway_transfers,omitempty"`
	Sources          map[string]SourceRecord          `json:"sources,omitempty"`
	PKI              identity.Policy                  `json:"pki,omitempty"`
	Disks            map[string]disk.Catalog          `json:"disks,omitempty"`
	UnitMappings     map[string]unitruntime.IDMapping `json:"unit_mappings,omitempty"`
	StorageFailovers map[string]StorageFailover       `json:"storage_failovers,omitempty"`
	Tasks            map[string]Task                  `json:"tasks,omitempty"`
	Secrets          map[string][]secrets.Record      `json:"secrets,omitempty"`
	Autoscalers      map[string]Autoscaler            `json:"autoscalers,omitempty"`
	Name             string                           `json:"name"`
	Network          RealmNetwork                     `json:"network"`
	Revision         uint64                           `json:"revision"`
	Nodes            map[string]Node                  `json:"nodes"`
	Fleets           map[string]Fleet                 `json:"fleets"`
	Assignments      map[string]Assignment            `json:"assignments"`
	Routes           map[string]Route                 `json:"routes"`
	Policies         map[string]NetworkPolicy         `json:"policies"`
	UpdatedAt        time.Time                        `json:"updated_at"`
}

type Store struct {
	// Serializes desired changes and physical rollout operations.
	Orchestration     sync.Mutex
	path              string
	stateRoot         string
	mu                sync.Mutex
	data              State
	consensus         Consensus
	haManaged         bool
	transactionBefore *State
}

func Open(stateRoot, realmName string) (*Store, error) {
	if strings.TrimSpace(stateRoot) == "" {
		stateRoot = "/var/lib/titanus"
	}
	path := filepath.Join(stateRoot, "realm", "state.json")
	_, haErr := os.Stat(filepath.Join(stateRoot, "realm", "consensus", "membership.json"))
	if haErr != nil && !os.IsNotExist(haErr) {
		return nil, haErr
	}
	store := &Store{stateRoot: stateRoot, path: path, haManaged: haErr == nil}
	if err := store.load(realmName); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Snapshot() State {
	s.lock()
	defer s.unlock()
	return cloneState(s.data)
}

func (s *Store) UpsertNode(node Node) error {
	s.lock()
	defer s.unlock()
	if strings.TrimSpace(node.ID) == "" {
		return fmt.Errorf("node ID is required")
	}
	if !version.Admits(node.Compatibility, s.data.SchemaVersion) {
		return fmt.Errorf("node protocol/data capabilities do not admit active schema")
	}
	existing, existed := s.data.Nodes[node.ID]
	if node.JoinedAt.IsZero() {
		if existed {
			node.JoinedAt = existing.JoinedAt
		} else {
			node.JoinedAt = time.Now().UTC()
		}
	}
	if existed {
		node.PowerQuarantined = existing.PowerQuarantined
		node.StorageQuarantined = existing.StorageQuarantined
		if node.FabricAddress == "" {
			node.FabricAddress = existing.FabricAddress
		}
		if node.FabricCIDR == "" {
			node.FabricCIDR = existing.FabricCIDR
		}
		if node.Failures == 0 {
			node.Failures = existing.Failures
		}
		if node.Successes == 0 {
			node.Successes = existing.Successes
		}
	}
	if node.LastPulse.IsZero() {
		node.LastPulse = time.Now().UTC()
	}
	if node.State == "" {
		node.State = NodeReady
	}
	if node.Labels == nil {
		node.Labels = map[string]string{}
	}
	if node.FabricCIDR == "" && s.data.Network.FabricCIDR != "" {
		subnet, err := s.allocateNodeSubnetLocked()
		if err != nil {
			return err
		}
		node.FabricCIDR = subnet
	}
	s.data.Nodes[node.ID] = node
	return s.commitLocked()
}

func (s *Store) ConfigureNetwork(network RealmNetwork) error {
	s.lock()
	defer s.unlock()
	if network.NodePrefix == 0 {
		network.NodePrefix = 24
	}
	if network.VXLANID == 0 {
		network.VXLANID = 4242
	}
	if network.VXLANID < 1 || network.VXLANID > 16777215 {
		return fmt.Errorf("VXLAN ID must be between 1 and 16777215")
	}
	fabricIP, fabricNet, err := net.ParseCIDR(network.FabricCIDR)
	if err != nil {
		return fmt.Errorf("invalid Realm Fabric CIDR: %w", err)
	}
	ones, bits := fabricNet.Mask.Size()
	if bits != 32 || fabricIP.To4() == nil || network.NodePrefix <= ones || network.NodePrefix > 30 {
		return fmt.Errorf("Node prefix /%d is incompatible with IPv4 Fabric %s", network.NodePrefix, network.FabricCIDR)
	}
	network.FabricCIDR = fabricNet.String()

	serviceIP, serviceNet, err := net.ParseCIDR(network.ServiceCIDR)
	if err != nil {
		return fmt.Errorf("invalid Realm Service CIDR: %w", err)
	}
	serviceOnes, serviceBits := serviceNet.Mask.Size()
	if serviceBits != 32 || serviceIP.To4() == nil || serviceOnes > 30 {
		return fmt.Errorf("Realm Service CIDR must be IPv4 with at least two usable addresses")
	}
	network.ServiceCIDR = serviceNet.String()
	if cidrsOverlap(fabricNet, serviceNet) {
		return fmt.Errorf("Realm Fabric CIDR %s overlaps Service CIDR %s", network.FabricCIDR, network.ServiceCIDR)
	}
	for name, route := range s.data.Routes {
		if strings.TrimSpace(route.ServiceIP) == "" {
			continue
		}
		ip := net.ParseIP(route.ServiceIP)
		if ip == nil || ip.To4() == nil || !serviceNet.Contains(ip.To4()) {
			return fmt.Errorf("Route %s service IP %s is outside requested Service CIDR %s", name, route.ServiceIP, network.ServiceCIDR)
		}
	}
	s.data.Network = network
	return s.commitLocked()
}

func (s *Store) allocateNodeSubnetLocked() (string, error) {
	_, network, err := net.ParseCIDR(s.data.Network.FabricCIDR)
	if err != nil {
		return "", err
	}
	ones, bits := network.Mask.Size()
	prefix := s.data.Network.NodePrefix
	if bits != 32 || prefix <= ones {
		return "", fmt.Errorf("unsupported Realm Fabric layout")
	}
	used := map[string]bool{}
	for _, node := range s.data.Nodes {
		if node.FabricCIDR != "" {
			used[node.FabricCIDR] = true
		}
	}
	count := 1 << uint(prefix-ones)
	base := ipv4Uint(network.IP.To4())
	step := uint32(1) << uint(32-prefix)
	for index := 1; index < count; index++ {
		ip := uintIPv4(base + uint32(index)*step)
		candidate := fmt.Sprintf("%s/%d", ip.String(), prefix)
		_, candidateNet, _ := net.ParseCIDR(candidate)
		candidate = candidateNet.String()
		if !used[candidate] {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("Realm Fabric %s has no free Node subnets", s.data.Network.FabricCIDR)
}

func cidrsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

func (s *Store) allocateServiceIPLocked(routeName string) (string, error) {
	_, network, err := net.ParseCIDR(s.data.Network.ServiceCIDR)
	if err != nil {
		return "", fmt.Errorf("Realm Service CIDR is not configured: %w", err)
	}
	ones, bits := network.Mask.Size()
	if bits != 32 {
		return "", fmt.Errorf("Titanus Service Fabric v1 requires IPv4")
	}
	hostCount := uint64(1) << uint(bits-ones)
	start := uint64(10)
	if hostCount <= start+1 {
		start = 1
	}
	used := map[string]bool{}
	for name, route := range s.data.Routes {
		if name != routeName && route.ServiceIP != "" {
			used[route.ServiceIP] = true
		}
	}
	base := ipv4Uint(network.IP.To4())
	for offset := start; offset+1 < hostCount; offset++ {
		candidate := uintIPv4(base + uint32(offset))
		if candidate == nil || !network.Contains(candidate) {
			continue
		}
		text := candidate.String()
		if !used[text] {
			return text, nil
		}
	}
	return "", fmt.Errorf("Realm Service CIDR %s has no free Route addresses", s.data.Network.ServiceCIDR)
}

func (s *Store) validateServiceIPLocked(routeName, value string) error {
	ip := net.ParseIP(strings.TrimSpace(value))
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("invalid Route service IP %q", value)
	}
	_, network, err := net.ParseCIDR(s.data.Network.ServiceCIDR)
	if err != nil {
		return fmt.Errorf("Realm Service CIDR is not configured: %w", err)
	}
	ip = ip.To4()
	if !network.Contains(ip) {
		return fmt.Errorf("Route service IP %s is outside Realm Service CIDR %s", ip.String(), network.String())
	}
	ones, bits := network.Mask.Size()
	hostCount := uint64(1) << uint(bits-ones)
	offset := uint64(ipv4Uint(ip) - ipv4Uint(network.IP.To4()))
	if offset == 0 || offset+1 >= hostCount {
		return fmt.Errorf("Route service IP %s is not a usable address in %s", ip.String(), network.String())
	}
	for name, existing := range s.data.Routes {
		if name != routeName && existing.ServiceIP == ip.String() {
			return fmt.Errorf("Route %s already uses service IP %s", name, ip.String())
		}
	}
	return nil
}

func ipv4Uint(ip net.IP) uint32 {
	if ip == nil {
		return 0
	}
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func uintIPv4(value uint32) net.IP {
	return net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value))
}

func (s *Store) Pulse(nodeID string, resources Resources) error {
	s.lock()
	defer s.unlock()
	node, ok := s.data.Nodes[nodeID]
	if !ok {
		return fmt.Errorf("unknown Realm Node %s", nodeID)
	}
	node.Resources = resources
	node.LastPulse = time.Now().UTC()
	if node.State != NodeDraining && node.State != NodeDisabled {
		node.State = NodeReady
	}
	node.Successes++
	s.data.Nodes[nodeID] = node
	return s.commitLocked()
}

func (s *Store) EvaluateHealth(now time.Time, suspectAfter, unreachableAfter time.Duration) ([]Node, error) {
	s.lock()
	defer s.unlock()
	changed := make([]Node, 0)
	for id, node := range s.data.Nodes {
		if node.State == NodeDisabled || node.State == NodeDraining {
			continue
		}
		age := now.Sub(node.LastPulse)
		next := NodeReady
		if age >= unreachableAfter {
			next = NodeUnreachable
		} else if age >= suspectAfter {
			next = NodeSuspect
		}
		if node.State != next {
			if next == NodeUnreachable {
				node.Failures++
			}
			node.State = next
			s.data.Nodes[id] = node
			changed = append(changed, node)
		}
	}
	if len(changed) > 0 {
		if err := s.commitLocked(); err != nil {
			return nil, err
		}
	}
	return changed, nil
}

func (s *Store) PutFleet(fleet Fleet) error {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	return s.putFleet(fleet)
}
func (s *Store) putFleet(fleet Fleet) error {
	s.lock()
	defer s.unlock()
	if err := validateCatalogFleet(s.data, fleet); err != nil {
		return err
	}
	if strings.TrimSpace(fleet.Name) == "" {
		return fmt.Errorf("Fleet name is required")
	}
	if fleet.Instances < 0 {
		return fmt.Errorf("Fleet instances cannot be negative")
	}
	if fleet.MinimumAvailable < 0 || fleet.MinimumAvailable > fleet.Instances {
		return fmt.Errorf("Fleet minimum_available must be between 0 and instances")
	}
	if fleet.MaxSurge == 0 {
		fleet.MaxSurge = 1
	}
	if fleet.MaxSurge < 1 || fleet.MaxSurge > 100 {
		return fmt.Errorf("max_surge must be between 1 and 100")
	}
	if len(fleet.Template.Command) == 0 || strings.TrimSpace(fleet.Template.Source) == "" {
		return fmt.Errorf("Fleet requires Source and command")
	}
	if err := s.validateSecretsLocked(fleet.Template); err != nil {
		return err
	}
	fleet.Template.Health.Normalize("always")
	if err := fleet.Template.Health.Validate(); err != nil {
		return fmt.Errorf("Fleet health: %w", err)
	}
	fleet.Template.Security.Normalize()
	if err := fleet.Template.Security.Validate(); err != nil {
		return fmt.Errorf("Fleet security: %w", err)
	}
	if err := fabric.ValidatePorts(fleet.Template.Ports); err != nil {
		return err
	}
	if err := disk.ValidateMounts(fleet.Template.Mounts); err != nil {
		return err
	}
	if a, ok := s.data.Autoscalers[fleet.Name]; ok && (fleet.Template.CPUPercent < 1 || len(fleet.Template.Mounts) > 0 || fleet.MinimumAvailable > a.Min || fleet.Instances < a.Min || fleet.Instances > a.Max) {
		return fmt.Errorf("disable or update autoscaler before incompatible Fleet changes")
	}
	if existing, ok := s.data.Fleets[fleet.Name]; ok {
		fleet.Generation = existing.Generation + 1
		fleet.History = append(append([]FleetRevision(nil), existing.History...), revisionOf(existing))
		if len(fleet.History) > 10 {
			kept := fleet.History[:0]
			for i, r := range fleet.History {
				used := i >= len(fleet.History)-10
				for _, a := range s.data.Assignments {
					if a.Fleet == fleet.Name && a.Generation == r.Generation {
						used = true
					}
				}
				if used {
					kept = append(kept, r)
				}
			}
			fleet.History = kept
		}
	} else {
		fleet.Generation = 1
		fleet.History = nil
	}
	fleet.UpdatedAt = time.Now().UTC()
	s.data.Fleets[fleet.Name] = fleet
	return s.commitLocked()
}

func (s *Store) PutRoute(route Route) (Route, error) {
	s.lock()
	defer s.unlock()
	route.Name = strings.TrimSpace(route.Name)
	route.Fleet = strings.TrimSpace(route.Fleet)
	route.ServiceIP = strings.TrimSpace(route.ServiceIP)
	route.Protocol = strings.ToLower(strings.TrimSpace(route.Protocol))
	if route.Name == "" || route.Fleet == "" {
		return Route{}, fmt.Errorf("Route name and Fleet are required")
	}
	if _, ok := s.data.Fleets[route.Fleet]; !ok {
		return Route{}, fmt.Errorf("unknown Fleet %s", route.Fleet)
	}
	if route.ListenIP == "" {
		route.ListenIP = "0.0.0.0"
	}
	if ip := net.ParseIP(route.ListenIP); ip == nil {
		return Route{}, fmt.Errorf("invalid Route listen IP %q", route.ListenIP)
	}
	if route.Gateway != "" {
		g, ok := s.data.Gateways[route.Gateway]
		if !ok {
			return Route{}, fmt.Errorf("unknown Route Gateway")
		}
		ip, _, _ := net.ParseCIDR(g.VIP)
		if route.ListenIP != ip.String() {
			return Route{}, fmt.Errorf("managed Route must listen on its Gateway VIP")
		}
	}
	if route.Protocol == "" {
		route.Protocol = "tcp"
	}
	if route.Protocol != "tcp" {
		return Route{}, fmt.Errorf("Titanus Route v1 supports TCP only")
	}
	if route.ListenPort < 1 || route.ListenPort > 65535 || route.TargetPort < 1 || route.TargetPort > 65535 {
		return Route{}, fmt.Errorf("invalid Route ports")
	}
	for name, existing := range s.data.Routes {
		if name != route.Name && existing.ListenIP == route.ListenIP &&
			existing.ListenPort == route.ListenPort && existing.Protocol == route.Protocol {
			return Route{}, fmt.Errorf("Route %s already uses %s:%d/%s", name, route.ListenIP, route.ListenPort, route.Protocol)
		}
	}

	existing, existed := s.data.Routes[route.Name]
	if route.ServiceIP == "" && existed {
		route.ServiceIP = existing.ServiceIP
	}
	if route.ServiceIP == "" {
		serviceIP, err := s.allocateServiceIPLocked(route.Name)
		if err != nil {
			return Route{}, err
		}
		route.ServiceIP = serviceIP
	}
	if err := s.validateServiceIPLocked(route.Name, route.ServiceIP); err != nil {
		return Route{}, err
	}

	now := time.Now().UTC()
	if existed {
		route.CreatedAt = existing.CreatedAt
	} else {
		route.CreatedAt = now
	}
	route.UpdatedAt = now
	s.data.Routes[route.Name] = route
	if err := s.commitLocked(); err != nil {
		return Route{}, err
	}
	return route, nil
}

func (s *Store) PutPolicy(policy NetworkPolicy) (NetworkPolicy, error) {
	s.lock()
	defer s.unlock()

	policy.Name = strings.TrimSpace(policy.Name)
	policy.Fleet = strings.TrimSpace(policy.Fleet)
	if policy.Name == "" || policy.Fleet == "" {
		return NetworkPolicy{}, fmt.Errorf("Network Policy name and Fleet are required")
	}
	if _, ok := s.data.Fleets[policy.Fleet]; !ok {
		return NetworkPolicy{}, fmt.Errorf("unknown Fleet %s", policy.Fleet)
	}
	policy.DefaultDeny = policy.DefaultDeny || len(policy.Ingress) > 0
	policy.DefaultDenyEgress = policy.DefaultDenyEgress || len(policy.Egress) > 0
	if !policy.DefaultDeny && !policy.DefaultDenyEgress {
		return NetworkPolicy{}, fmt.Errorf("Network Policy %s must protect ingress, egress or both", policy.Name)
	}
	for i, rule := range policy.Ingress {
		rule.FromFleet = strings.TrimSpace(rule.FromFleet)
		rule.FromCIDR = strings.TrimSpace(rule.FromCIDR)
		rule.Protocol = strings.ToLower(strings.TrimSpace(rule.Protocol))
		if rule.Protocol == "" {
			rule.Protocol = "tcp"
		}
		if rule.Protocol != "tcp" && rule.Protocol != "udp" && rule.Protocol != "any" {
			return NetworkPolicy{}, fmt.Errorf("Network Policy %s rule %d has unsupported protocol %q", policy.Name, i+1, rule.Protocol)
		}
		if rule.FromFleet != "" {
			if _, ok := s.data.Fleets[rule.FromFleet]; !ok {
				return NetworkPolicy{}, fmt.Errorf("Network Policy %s references unknown source Fleet %s", policy.Name, rule.FromFleet)
			}
		}
		if rule.FromCIDR != "" {
			ip, network, err := net.ParseCIDR(rule.FromCIDR)
			if err != nil || ip.To4() == nil {
				return NetworkPolicy{}, fmt.Errorf("Network Policy %s rule %d has invalid IPv4 source CIDR %q", policy.Name, i+1, rule.FromCIDR)
			}
			rule.FromCIDR = network.String()
		}
		seenPorts := map[int]bool{}
		ports := make([]int, 0, len(rule.Ports))
		for _, port := range rule.Ports {
			if port < 1 || port > 65535 {
				return NetworkPolicy{}, fmt.Errorf("Network Policy %s rule %d has invalid port %d", policy.Name, i+1, port)
			}
			if seenPorts[port] {
				continue
			}
			seenPorts[port] = true
			ports = append(ports, port)
		}
		sort.Ints(ports)
		if rule.Protocol == "any" && len(ports) > 0 {
			return NetworkPolicy{}, fmt.Errorf("Network Policy %s ingress rule %d cannot combine protocol any with ports", policy.Name, i+1)
		}
		rule.Ports = ports
		policy.Ingress[i] = rule
	}
	for i, rule := range policy.Egress {
		rule.ToFleet = strings.TrimSpace(rule.ToFleet)
		rule.ToCIDR = strings.TrimSpace(rule.ToCIDR)
		rule.Protocol = strings.ToLower(strings.TrimSpace(rule.Protocol))
		if rule.Protocol == "" {
			rule.Protocol = "tcp"
		}
		if rule.Protocol != "tcp" && rule.Protocol != "udp" && rule.Protocol != "any" {
			return NetworkPolicy{}, fmt.Errorf("Network Policy %s egress rule %d has unsupported protocol %q", policy.Name, i+1, rule.Protocol)
		}
		if rule.ToFleet != "" {
			if _, ok := s.data.Fleets[rule.ToFleet]; !ok {
				return NetworkPolicy{}, fmt.Errorf("Network Policy %s references unknown destination Fleet %s", policy.Name, rule.ToFleet)
			}
		}
		if rule.ToCIDR != "" {
			ip, network, err := net.ParseCIDR(rule.ToCIDR)
			if err != nil || ip.To4() == nil {
				return NetworkPolicy{}, fmt.Errorf("Network Policy %s egress rule %d has invalid IPv4 destination CIDR %q", policy.Name, i+1, rule.ToCIDR)
			}
			rule.ToCIDR = network.String()
		}
		seenPorts := map[int]bool{}
		ports := make([]int, 0, len(rule.Ports))
		for _, port := range rule.Ports {
			if port < 1 || port > 65535 {
				return NetworkPolicy{}, fmt.Errorf("Network Policy %s egress rule %d has invalid port %d", policy.Name, i+1, port)
			}
			if seenPorts[port] {
				continue
			}
			seenPorts[port] = true
			ports = append(ports, port)
		}
		sort.Ints(ports)
		if rule.Protocol == "any" && len(ports) > 0 {
			return NetworkPolicy{}, fmt.Errorf("Network Policy %s egress rule %d cannot combine protocol any with ports", policy.Name, i+1)
		}
		rule.Ports = ports
		policy.Egress[i] = rule
	}
	now := time.Now().UTC()
	if existing, ok := s.data.Policies[policy.Name]; ok {
		policy.CreatedAt = existing.CreatedAt
	} else {
		policy.CreatedAt = now
	}
	policy.UpdatedAt = now
	s.data.Policies[policy.Name] = policy
	if err := s.commitLocked(); err != nil {
		return NetworkPolicy{}, err
	}
	return policy, nil
}

func (s *Store) GetPolicy(name string) (NetworkPolicy, bool) {
	s.lock()
	defer s.unlock()
	policy, ok := s.data.Policies[name]
	return policy, ok
}

func (s *Store) DeletePolicy(name string) error {
	s.lock()
	defer s.unlock()
	if _, ok := s.data.Policies[name]; !ok {
		return fmt.Errorf("unknown Network Policy %s", name)
	}
	delete(s.data.Policies, name)
	return s.commitLocked()
}

func (s *Store) DeleteRoute(name string) error {
	s.lock()
	defer s.unlock()
	if _, ok := s.data.Routes[name]; !ok {
		return fmt.Errorf("unknown Route %s", name)
	}
	delete(s.data.Routes, name)
	return s.commitLocked()
}

func (s *Store) GetRoute(name string) (Route, bool) {
	s.lock()
	defer s.unlock()
	route, ok := s.data.Routes[name]
	return route, ok
}

func (s *Store) UpdateAssignmentRuntime(id, address string, state AssignmentState) error {
	s.lock()
	defer s.unlock()
	assignment, ok := s.data.Assignments[id]
	if !ok {
		return fmt.Errorf("unknown assignment %s", id)
	}
	assignment.NetworkAddress = address
	assignment.State = state
	assignment.UpdatedAt = time.Now().UTC()
	s.data.Assignments[id] = assignment
	return s.commitLocked()
}

func (s *Store) GetFleet(name string) (Fleet, bool) {
	s.lock()
	defer s.unlock()
	fleet, ok := s.data.Fleets[name]
	return fleet, ok
}

func (s *Store) ScaleFleet(name string, instances int) (Fleet, error) {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	s.lock()
	defer s.unlock()
	fleet, ok := s.data.Fleets[name]
	if !ok {
		return Fleet{}, fmt.Errorf("unknown Fleet %s", name)
	}
	if instances < 0 {
		return Fleet{}, fmt.Errorf("Fleet instances cannot be negative")
	}
	if a, ok := s.data.Autoscalers[name]; ok && (instances < a.Min || instances > a.Max) {
		return Fleet{}, fmt.Errorf("manual scale is outside autoscaler bounds; disable policy first")
	}
	fleet.Instances = instances
	if err := validateCatalogFleet(s.data, fleet); err != nil {
		return Fleet{}, err
	}
	if fleet.MinimumAvailable > instances {
		fleet.MinimumAvailable = instances
	}
	fleet.UpdatedAt = time.Now().UTC()
	s.data.Fleets[name] = fleet
	if err := s.commitLocked(); err != nil {
		return Fleet{}, err
	}
	return fleet, nil
}

func (s *Store) DeleteFleet(name string) error {
	s.Orchestration.Lock()
	defer s.Orchestration.Unlock()
	s.lock()
	defer s.unlock()
	if _, ok := s.data.Fleets[name]; !ok {
		return fmt.Errorf("unknown Fleet %s", name)
	}
	for policyName, policy := range s.data.Policies {
		if policy.Fleet == name {
			return fmt.Errorf("Fleet %s is protected by Network Policy %s", name, policyName)
		}
		for _, rule := range policy.Ingress {
			if rule.FromFleet == name {
				return fmt.Errorf("Fleet %s is referenced by Network Policy %s", name, policyName)
			}
		}
		for _, rule := range policy.Egress {
			if rule.ToFleet == name {
				return fmt.Errorf("Fleet %s is referenced by Network Policy %s", name, policyName)
			}
		}
	}
	delete(s.data.Autoscalers, name)
	delete(s.data.Fleets, name)
	return s.commitLocked()
}

func (s *Store) SetAssignments(fleetName string, assignments []Assignment) error {
	s.lock()
	defer s.unlock()
	for id, assignment := range s.data.Assignments {
		if assignment.Fleet == fleetName {
			delete(s.data.Assignments, id)
		}
	}
	for _, assignment := range assignments {
		s.data.Assignments[assignment.ID] = assignment
	}
	return s.commitLocked()
}

func (s *Store) RenewAssignmentLease(id, token string, expiresAt time.Time) error {
	s.lock()
	defer s.unlock()
	assignment, ok := s.data.Assignments[id]
	if !ok {
		return fmt.Errorf("unknown assignment %s", id)
	}
	if assignment.LeaseToken != "" && assignment.LeaseToken != token && time.Now().UTC().Before(assignment.LeaseExpiresAt) {
		return fmt.Errorf("assignment %s has a different live lease", id)
	}
	assignment.LeaseToken = token
	assignment.LeaseExpiresAt = expiresAt.UTC()
	assignment.UpdatedAt = time.Now().UTC()
	s.data.Assignments[id] = assignment
	return s.commitLocked()
}

func (s *Store) UpdateAssignmentState(id string, next AssignmentState) error {
	s.lock()
	defer s.unlock()
	assignment, ok := s.data.Assignments[id]
	if !ok {
		return fmt.Errorf("unknown assignment %s", id)
	}
	assignment.State = next
	assignment.UpdatedAt = time.Now().UTC()
	s.data.Assignments[id] = assignment
	return s.commitLocked()
}

func (s *Store) load(realmName string) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0750); err != nil {
		return err
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		s.data = State{
			Name: realmName, Nodes: map[string]Node{},
			Fleets: map[string]Fleet{}, Assignments: map[string]Assignment{}, Routes: map[string]Route{},
			Policies: map[string]NetworkPolicy{}, UpdatedAt: time.Now().UTC(),
		}
		return s.commitLocked()
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		return err
	}
	if e := ValidateSchema(s.data); e != nil {
		return e
	}
	if s.data.SchemaVersion > 0 {
		if e := offline.CheckCommittedFloor(s.stateRoot, s.data.Name, s.data.SchemaMigrations[len(s.data.SchemaMigrations)-1].ID, s.data.SchemaVersion); e != nil {
			return e
		}
	}
	if s.data.Nodes == nil {
		s.data.Nodes = map[string]Node{}
	}
	if s.data.Fleets == nil {
		s.data.Fleets = map[string]Fleet{}
	}
	if s.data.Assignments == nil {
		s.data.Assignments = map[string]Assignment{}
	}
	if s.data.Routes == nil {
		s.data.Routes = map[string]Route{}
	}
	if s.data.Policies == nil {
		s.data.Policies = map[string]NetworkPolicy{}
	}
	if s.data.Tasks == nil {
		s.data.Tasks = map[string]Task{}
	}
	if s.data.Secrets == nil {
		s.data.Secrets = map[string][]secrets.Record{}
	}
	if s.data.Autoscalers == nil {
		s.data.Autoscalers = map[string]Autoscaler{}
	}
	if s.data.Name == "" {
		s.data.Name = realmName
	}
	if s.data.Network.ServiceCIDR != "" {
		names := make([]string, 0, len(s.data.Routes))
		for name := range s.data.Routes {
			names = append(names, name)
		}
		sort.Strings(names)
		changed := false
		for _, name := range names {
			route := s.data.Routes[name]
			if strings.TrimSpace(route.ServiceIP) != "" {
				continue
			}
			serviceIP, err := s.allocateServiceIPLocked(name)
			if err != nil {
				return fmt.Errorf("migrate Route %s Service IP: %w", name, err)
			}
			route.ServiceIP = serviceIP
			s.data.Routes[name] = route
			changed = true
		}
		if changed {
			return s.commitLocked()
		}
	}
	return nil
}

func (s *Store) commitLocked() error {
	if s.haManaged && s.consensus == nil {
		return fmt.Errorf("HA-managed Realm requires the daemon consensus API")
	}
	s.data.Revision++
	s.data.UpdatedAt = time.Now().UTC()
	if e := ValidateSchema(s.data); e != nil {
		return e
	}
	if s.consensus != nil {
		return s.consensus.Apply(cloneState(s.data))
	}
	if err := durable.WriteJSON(s.path, s.data, 0600); err != nil {
		return err
	}
	s.transactionBefore = nil
	return nil
}

func cloneState(in State) State {
	data, _ := json.Marshal(in)
	var out State
	_ = json.Unmarshal(data, &out)
	return out
}

func SortedNodes(state State) []Node {
	nodes := make([]Node, 0, len(state.Nodes))
	for _, node := range state.Nodes {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return nodes
}
