package fabric

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReserveUniqueAddresses(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root)
	if err := os.MkdirAll(filepath.Join(root, "fabric"), 0750); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Bridge: "titanus0", CIDR: "10.240.0.0/24", Gateway: "10.240.0.1/24"}
	if err := writeJSON(manager.configPath(), cfg, 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(manager.statePath(), state{Allocations: map[string]Allocation{}}, 0600); err != nil {
		t.Fatal(err)
	}

	a, err := manager.Reserve("unit-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := manager.Reserve("unit-b")
	if err != nil {
		t.Fatal(err)
	}
	if a.Address == b.Address {
		t.Fatalf("duplicate allocation %s", a.Address)
	}
	if a.Address != "10.240.0.10" || b.Address != "10.240.0.11" {
		t.Fatalf("unexpected allocations: %s %s", a.Address, b.Address)
	}
}

func TestParsePort(t *testing.T) {
	p, err := ParsePort("8080:80/tcp")
	if err != nil {
		t.Fatal(err)
	}
	if p.HostPort != 8080 || p.ContainerPort != 80 || p.Protocol != "tcp" {
		t.Fatalf("unexpected port: %#v", p)
	}
}


func TestNormalizePortsDefaultsTCP(t *testing.T) {
	ports := normalizePorts([]Port{{HostPort: 8080, ContainerPort: 80}})
	if len(ports) != 1 || ports[0].Protocol != "tcp" {
		t.Fatalf("unexpected normalized ports: %#v", ports)
	}
}

func TestNormalizedPeersRejectIPv6ForFabricV1(t *testing.T) {
	peers := normalizedPeers([]Peer{
		{NodeID: "ipv6", VTEP: "2001:db8::1", CIDR: "10.241.0.0/24"},
		{NodeID: "good", VTEP: "192.0.2.10", CIDR: "10.242.0.0/24"},
	}, "192.0.2.1")
	if len(peers) != 1 || peers[0].NodeID != "good" {
		t.Fatalf("unexpected peers: %#v", peers)
	}
}


func TestNormalizeServicesSortsAndDeduplicatesBackends(t *testing.T) {
	_, network, err := net.ParseCIDR("10.250.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	services, err := normalizeServices(network, []Service{{
		Name: "web", Address: "10.250.0.10", Protocol: "TCP", Port: 8080,
		Backends: []ServiceBackend{
			{Address: "10.240.2.10", Port: 80},
			{Address: "10.240.1.10", Port: 80},
			{Address: "10.240.1.10", Port: 80},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || services[0].Protocol != "tcp" || len(services[0].Backends) != 2 {
		t.Fatalf("unexpected services: %#v", services)
	}
	if services[0].Backends[0].Address != "10.240.1.10" {
		t.Fatalf("backends were not sorted: %#v", services[0].Backends)
	}
}

func TestWriteServiceRulesLoadBalancesHealthyBackends(t *testing.T) {
	var b strings.Builder
	writeServiceRules(&b, []Service{{
		Name: "web", Address: "10.250.0.10", Protocol: "tcp", Port: 8080,
		Backends: []ServiceBackend{
			{Address: "10.240.1.10", Port: 80},
			{Address: "10.240.2.10", Port: 80},
		},
	}})
	rules := b.String()
	if !strings.Contains(rules, "ip daddr 10.250.0.10 tcp dport 8080 dnat to numgen inc mod 2 map { 0 : 10.240.1.10, 1 : 10.240.2.10 } : 80") {
		t.Fatalf("unexpected service rules: %s", rules)
	}
}

func TestNormalizeServicesRejectsAddressOutsideServiceCIDR(t *testing.T) {
	_, network, err := net.ParseCIDR("10.250.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	_, err = normalizeServices(network, []Service{{
		Name: "web", Address: "10.251.0.10", Protocol: "tcp", Port: 80,
	}})
	if err == nil {
		t.Fatal("expected service address outside CIDR to be rejected")
	}
}


func TestNormalizeServicesRejectsMixedBackendPorts(t *testing.T) {
	_, network, err := net.ParseCIDR("10.250.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	_, err = normalizeServices(network, []Service{{
		Name: "web", Address: "10.250.0.10", Protocol: "tcp", Port: 8080,
		Backends: []ServiceBackend{
			{Address: "10.240.1.10", Port: 80},
			{Address: "10.240.2.10", Port: 81},
		},
	}})
	if err == nil {
		t.Fatal("expected mixed backend ports to be rejected")
	}
}


func TestRenderedNATRulesPassNftCheck(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("nft syntax validation requires root")
	}
	nft, err := exec.LookPath("nft")
	if err != nil {
		t.Skip("nft is not installed")
	}
	rules := renderNATRules(
		Config{CIDR: "10.240.1.0/24", Bridge: "titanus-ci0"},
		state{Allocations: map[string]Allocation{}},
		[]Service{{
			Name: "web", Address: "10.250.0.10", Protocol: "tcp", Port: 8080,
			Backends: []ServiceBackend{
				{Address: "10.240.1.10", Port: 80},
				{Address: "10.240.2.10", Port: 80},
			},
		}},
	)
	cmd := exec.Command(nft, "-c", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("nft rejected Titanus rules: %v\n%s\nRules:\n%s", err, string(output), rules)
	}
}


func TestNormalizePoliciesCanonicalizesSourcesAndPorts(t *testing.T) {
	policies, err := normalizePolicies([]Policy{{
		Name: "web-ingress",
		Destinations: []string{"10.240.2.10", "10.240.2.10"},
		DefaultDeny: true,
		Rules: []PolicyRule{{
			Sources: []string{"10.240.1.55/24", "10.240.1.0/24"},
			Protocol: "TCP",
			Ports: []int{443, 80, 443},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 1 || len(policies[0].Destinations) != 1 {
		t.Fatalf("unexpected normalized policies: %#v", policies)
	}
	rule := policies[0].Rules[0]
	if rule.Protocol != "tcp" || len(rule.Sources) != 1 || rule.Sources[0] != "10.240.1.0/24" {
		t.Fatalf("unexpected normalized rule: %#v", rule)
	}
	if len(rule.Ports) != 2 || rule.Ports[0] != 80 || rule.Ports[1] != 443 {
		t.Fatalf("unexpected normalized ports: %#v", rule.Ports)
	}
}

func TestRenderFilterRulesAllowsBeforeDefaultDeny(t *testing.T) {
	policies, err := normalizePolicies([]Policy{{
		Name: "web-ingress",
		Destinations: []string{"10.240.2.10"},
		DefaultDeny: true,
		Rules: []PolicyRule{{
			Sources: []string{"10.240.1.0/24"},
			Protocol: "tcp",
			Ports: []int{80, 443},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rules := renderFilterRules(policies)
	if !strings.Contains(rules, "table bridge titanus_filter_bridge") ||
		!strings.Contains(rules, "table ip titanus_filter_ip") {
		t.Fatalf("policy rules must cover bridge and IPv4 forwarding paths:\n%s", rules)
	}
	allow := "ip saddr 10.240.1.0/24 ip daddr 10.240.2.10 tcp dport { 80, 443 } accept"
	drop := "ip daddr 10.240.2.10 drop"
	if !strings.Contains(rules, allow) || !strings.Contains(rules, drop) {
		t.Fatalf("missing allow/default-deny policy rules:\n%s", rules)
	}
	for _, table := range []string{"titanus_filter_bridge", "titanus_filter_ip"} {
		start := strings.Index(rules, table)
		if start < 0 {
			t.Fatalf("missing table %s", table)
		}
		section := rules[start:]
		allowIndex := strings.Index(section, allow)
		dropIndex := strings.Index(section, drop)
		if allowIndex < 0 || dropIndex < 0 || allowIndex > dropIndex {
			t.Fatalf("allow rule must precede default deny in %s:\n%s", table, section)
		}
	}
}

func TestRenderedPolicyRulesPassNftCheck(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("nft syntax validation requires root")
	}
	nft, err := exec.LookPath("nft")
	if err != nil {
		t.Skip("nft is not installed")
	}
	policies, err := normalizePolicies([]Policy{{
		Name: "web-ingress",
		Destinations: []string{"10.240.2.10"},
		DefaultDeny: true,
		Rules: []PolicyRule{
			{Sources: []string{"10.240.1.0/24"}, Protocol: "tcp", Ports: []int{80, 443}},
			{AnySource: true, Protocol: "udp", Ports: []int{53}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rules := renderFilterRules(policies)
	cmd := exec.Command(nft, "-c", "-f", "-")
	cmd.Stdin = strings.NewReader(rules)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("nft rejected Titanus policy rules: %v\n%s\nRules:\n%s", err, string(output), rules)
	}
}
