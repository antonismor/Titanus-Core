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
	rules := renderNATTransaction(
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
	// Exercise first creation and replacement in an isolated native network
	// namespace, then prove an invalid replacement leaves the old table intact.
	dir := t.TempDir()
	transaction := rules
	if err := os.WriteFile(filepath.Join(dir, "rules.nft"), []byte(transaction), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "invalid.nft"), []byte(transaction+"add rule ip titanus_nat absent accept\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command("unshare", "--net", "--", "sh", "-eu", "-c", `
nft -f "$1/rules.nft"
nft -f "$1/rules.nft"
nft list table ip titanus_nat > "$1/before"
if nft -f "$1/invalid.nft"; then exit 1; fi
nft list table ip titanus_nat > "$1/after"
cmp "$1/before" "$1/after"
`, "titanus-nft-transaction", dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("native atomic NAT replacement/retention failed: %v\n%s", err, output)
	}
}

func TestNormalizePoliciesCanonicalizesSourcesAndPorts(t *testing.T) {
	policies, err := normalizePolicies([]Policy{{
		Name:         "web-ingress",
		Destinations: []string{"10.240.2.10", "10.240.2.10"},
		DefaultDeny:  true,
		Rules: []PolicyRule{{
			Sources:  []string{"10.240.1.55/24", "10.240.1.0/24"},
			Protocol: "TCP",
			Ports:    []int{443, 80, 443},
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

func TestRenderFilterRulesComposesIngressAndEgress(t *testing.T) {
	policies, err := normalizePolicies([]Policy{
		{
			Name:         "web-ingress",
			Destinations: []string{"10.240.2.10"},
			DefaultDeny:  true,
			Rules: []PolicyRule{{
				Sources:  []string{"10.240.1.0/24"},
				Protocol: "tcp",
				Ports:    []int{80, 443},
			}},
		},
		{
			Name:              "api-egress",
			Sources:           []string{"10.240.1.10"},
			DefaultDenyEgress: true,
			Egress: []EgressRule{{
				Destinations: []string{"10.240.2.0/24"},
				Protocol:     "tcp",
				Ports:        []int{443},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	rules := renderFilterRules(policies)
	if !strings.Contains(rules, "table bridge titanus_filter_bridge") ||
		!strings.Contains(rules, "table ip titanus_filter_ip") {
		t.Fatalf("policy rules must cover bridge and IPv4 forwarding paths:\n%s", rules)
	}
	if !strings.Contains(rules, "ip daddr 10.240.2.10 jump ti_i_") {
		t.Fatalf("missing ingress dispatch chain:\n%s", rules)
	}
	if !strings.Contains(rules, "ip saddr 10.240.1.10 jump ti_e_") {
		t.Fatalf("missing egress dispatch chain:\n%s", rules)
	}
	if !strings.Contains(rules, "ip saddr 10.240.1.0/24 tcp dport { 80, 443 } return") {
		t.Fatalf("missing ingress allow-return rule:\n%s", rules)
	}
	if !strings.Contains(rules, "ip daddr 10.240.2.0/24 tcp dport 443 return") {
		t.Fatalf("missing egress allow-return rule:\n%s", rules)
	}
	if !strings.Contains(rules, "drop comment \"Titanus ingress default deny\"") ||
		!strings.Contains(rules, "drop comment \"Titanus egress default deny\"") {
		t.Fatalf("missing directional default-deny rules:\n%s", rules)
	}
}

func TestNormalizePoliciesCanonicalizesEgressDestinations(t *testing.T) {
	policies, err := normalizePolicies([]Policy{{
		Name:              "api-egress",
		Sources:           []string{"10.240.1.10", "10.240.1.10"},
		DefaultDenyEgress: true,
		Egress: []EgressRule{{
			Destinations: []string{"192.0.2.55/24", "192.0.2.0/24"},
			Protocol:     "UDP",
			Ports:        []int{53, 53},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(policies) != 1 || len(policies[0].Sources) != 1 {
		t.Fatalf("unexpected normalized policy sources: %#v", policies)
	}
	rule := policies[0].Egress[0]
	if rule.Protocol != "udp" || len(rule.Destinations) != 1 || rule.Destinations[0] != "192.0.2.0/24" {
		t.Fatalf("unexpected normalized egress rule: %#v", rule)
	}
	if len(rule.Ports) != 1 || rule.Ports[0] != 53 {
		t.Fatalf("unexpected normalized egress ports: %#v", rule.Ports)
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
	policies, err := normalizePolicies([]Policy{
		{
			Name:         "web-ingress",
			Destinations: []string{"10.240.2.10"},
			DefaultDeny:  true,
			Rules: []PolicyRule{
				{Sources: []string{"10.240.1.0/24"}, Protocol: "tcp", Ports: []int{80, 443}},
				{AnySource: true, Protocol: "udp", Ports: []int{53}},
			},
		},
		{
			Name:              "api-egress",
			Sources:           []string{"10.240.1.10"},
			DefaultDenyEgress: true,
			Egress: []EgressRule{
				{Destinations: []string{"10.240.2.0/24"}, Protocol: "tcp", Ports: []int{443}},
				{AnyDestination: true, Protocol: "udp", Ports: []int{53}},
			},
		},
	})
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

// Native nft --check validates this rule as part of the rendered NAT fixture;
// the five-node test exercises the actual remote reply path after replacement.
func TestNodeServiceSNATPreservesWorkloadSources(t *testing.T) {
	rules := renderNATRules(Config{CIDR: "10.240.1.0/24", Bridge: "titanus0"}, state{Allocations: map[string]Allocation{}}, []Service{{Name: "web", Address: "10.250.0.10", Protocol: "tcp", Port: 18080, Backends: []ServiceBackend{{Address: "10.240.2.10", Port: 8080}}}})
	expected := "ct status dnat ct original ip daddr 10.250.0.10 meta l4proto tcp ct original proto-dst 18080 fib saddr type local masquerade"
	if !strings.Contains(rules, expected) {
		t.Fatalf("missing node-only Service return path: %s", rules)
	}
}

func TestEthernetIdentityAcrossReplacementAndSubnets(t *testing.T) {
	seen := map[string]bool{}
	for _, address := range []string{"10.240.0.1", "10.240.0.10", "10.240.1.1", "10.240.1.10", "10.241.0.10"} {
		mac, err := ethernetAddress(address)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := net.ParseMAC(mac)
		if err != nil || len(parsed) != 6 || parsed[0]&3 != 2 || seen[mac] {
			t.Fatalf("invalid or duplicate local unicast identity %s", mac)
		}
		seen[mac] = true
		again, err := ethernetAddress(address)
		if err != nil || mac != again {
			t.Fatalf("replacement changed identity for %s", address)
		}
	}
	for _, address := range []string{"", "invalid", "2001:db8::1"} {
		if _, err := ethernetAddress(address); err == nil {
			t.Fatalf("accepted non-IPv4 identity %q", address)
		}
	}
}
