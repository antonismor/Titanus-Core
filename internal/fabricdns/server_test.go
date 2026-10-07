package fabricdns

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/fabric"
)

func makeQuery(t *testing.T, name string, qtype uint16) []byte {
	t.Helper()
	packet := make([]byte, 12)
	binary.BigEndian.PutUint16(packet[0:2], 0x1234)
	binary.BigEndian.PutUint16(packet[2:4], 0x0100)
	binary.BigEndian.PutUint16(packet[4:6], 1)
	for _, label := range splitDNSName(name) {
		if len(label) > 63 {
			t.Fatal("test label too long")
		}
		packet = append(packet, byte(len(label)))
		packet = append(packet, label...)
	}
	packet = append(packet, 0, 0, byte(qtype), 0, 1)
	return packet
}

func splitDNSName(name string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(name); i++ {
		if i == len(name) || name[i] == '.' {
			if i > start {
				out = append(out, name[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func TestResolveServiceARecord(t *testing.T) {
	resolver := NewResolver("titanus", func() ([]fabric.Service, error) {
		return []fabric.Service{{Name: "web", Address: "10.250.0.10", Port: 8080, Protocol: "tcp"}}, nil
	})
	reply, handled, err := resolver.ResolvePacket(makeQuery(t, "web.titanus", dnsTypeA))
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("Titanus service query was not handled")
	}
	if binary.BigEndian.Uint16(reply[6:8]) != 1 {
		t.Fatalf("expected one answer: %#v", reply[:12])
	}
	if got := net.IP(reply[len(reply)-4:]).String(); got != "10.250.0.10" {
		t.Fatalf("unexpected service answer %s", got)
	}
}

func TestResolveUnknownTitanusNameReturnsNXDomain(t *testing.T) {
	resolver := NewResolver("titanus", func() ([]fabric.Service, error) {
		return []fabric.Service{}, nil
	})
	reply, handled, err := resolver.ResolvePacket(makeQuery(t, "missing.titanus", dnsTypeA))
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("Titanus domain query was not handled")
	}
	if binary.BigEndian.Uint16(reply[2:4])&0x000f != 3 {
		t.Fatalf("expected NXDOMAIN flags, got %#x", binary.BigEndian.Uint16(reply[2:4]))
	}
}

func TestNonTitanusNameIsForwardable(t *testing.T) {
	resolver := NewResolver("titanus", func() ([]fabric.Service, error) {
		return nil, nil
	})
	_, handled, err := resolver.ResolvePacket(makeQuery(t, "example.com", dnsTypeA))
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("external DNS query should be forwarded")
	}
}

func TestServiceAAAAReturnsNoData(t *testing.T) {
	resolver := NewResolver("titanus", func() ([]fabric.Service, error) {
		return []fabric.Service{{Name: "web", Address: "10.250.0.10"}}, nil
	})
	reply, handled, err := resolver.ResolvePacket(makeQuery(t, "web.titanus", dnsTypeAAAA))
	if err != nil {
		t.Fatal(err)
	}
	if !handled || binary.BigEndian.Uint16(reply[6:8]) != 0 || binary.BigEndian.Uint16(reply[2:4])&0xf != 0 {
		t.Fatalf("unexpected AAAA response header %#v", reply[:12])
	}
}

func TestSystemUpstreamsFiltersLocalFabricDNS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("nameserver 10.240.1.1\nnameserver 127.0.0.53\nnameserver 9.9.9.9\n"), 0600); err != nil {
		t.Fatal(err)
	}
	upstreams := SystemUpstreams(path, "10.240.1.1")
	if len(upstreams) != 2 || upstreams[0] != "127.0.0.53" || upstreams[1] != "9.9.9.9" {
		t.Fatalf("unexpected upstreams %#v", upstreams)
	}
}

func TestGatewayListenAddress(t *testing.T) {
	address, err := GatewayListenAddress("10.240.1.1/24")
	if err != nil {
		t.Fatal(err)
	}
	if address != "10.240.1.1:53" {
		t.Fatalf("unexpected DNS listen address %s", address)
	}
}
