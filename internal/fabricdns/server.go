package fabricdns

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/antonismor/Titanus-Core/internal/fabric"
)

const (
	dnsTypeA    = 1
	dnsTypeAAAA = 28
	dnsTypeANY  = 255
	dnsClassIN  = 1
)

type ServiceSource func() ([]fabric.Service, error)

type Resolver struct {
	Domain    string
	TTL       uint32
	Services  ServiceSource
	Upstreams []string
	Timeout   time.Duration
}

func NewResolver(domain string, source ServiceSource) *Resolver {
	domain = strings.Trim(strings.ToLower(strings.TrimSpace(domain)), ".")
	if domain == "" {
		domain = "titanus"
	}
	return &Resolver{
		Domain: domain, TTL: 10, Services: source,
		Timeout: 2 * time.Second,
	}
}

func (r *Resolver) ResolvePacket(query []byte) ([]byte, bool, error) {
	q, err := parseQuestion(query)
	if err != nil {
		return nil, false, err
	}
	name := strings.Trim(strings.ToLower(q.Name), ".")
	suffix := "." + r.Domain
	if name != r.Domain && !strings.HasSuffix(name, suffix) {
		return nil, false, nil
	}

	serviceName := strings.TrimSuffix(name, suffix)
	serviceName = strings.TrimSuffix(serviceName, ".")
	if serviceName == "" {
		return response(query, q, nil, 3, r.TTL), true, nil
	}

	services, err := r.Services()
	if err != nil {
		return response(query, q, nil, 2, r.TTL), true, err
	}
	for _, service := range services {
		if !strings.EqualFold(strings.TrimSpace(service.Name), serviceName) {
			continue
		}
		if q.Class != dnsClassIN {
			return response(query, q, nil, 0, r.TTL), true, nil
		}
		if q.Type == dnsTypeAAAA {
			return response(query, q, nil, 0, r.TTL), true, nil
		}
		if q.Type != dnsTypeA && q.Type != dnsTypeANY {
			return response(query, q, nil, 0, r.TTL), true, nil
		}
		ip := net.ParseIP(service.Address)
		if ip == nil || ip.To4() == nil {
			return response(query, q, nil, 2, r.TTL), true, fmt.Errorf("service %s has invalid IPv4 address %q", service.Name, service.Address)
		}
		return response(query, q, ip.To4(), 0, r.TTL), true, nil
	}
	return response(query, q, nil, 3, r.TTL), true, nil
}

func (r *Resolver) HandleUDP(query []byte) ([]byte, error) {
	if reply, handled, err := r.ResolvePacket(query); handled {
		return reply, err
	}
	return r.forwardUDP(query)
}

func (r *Resolver) HandleTCP(query []byte) ([]byte, error) {
	if reply, handled, err := r.ResolvePacket(query); handled {
		return reply, err
	}
	return r.forwardTCP(query)
}

func (r *Resolver) forwardUDP(query []byte) ([]byte, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	var lastErr error
	for _, upstream := range r.Upstreams {
		conn, err := net.DialTimeout("udp", dnsAddress(upstream), timeout)
		if err != nil {
			lastErr = err
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(timeout))
		if _, err := conn.Write(query); err != nil {
			lastErr = err
			_ = conn.Close()
			continue
		}
		buf := make([]byte, 65535)
		n, err := conn.Read(buf)
		_ = conn.Close()
		if err == nil {
			return append([]byte(nil), buf[:n]...), nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no DNS upstreams configured")
	}
	return servfail(query), lastErr
}

func (r *Resolver) forwardTCP(query []byte) ([]byte, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	var lastErr error
	for _, upstream := range r.Upstreams {
		conn, err := net.DialTimeout("tcp", dnsAddress(upstream), timeout)
		if err != nil {
			lastErr = err
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(timeout))
		frame := make([]byte, 2+len(query))
		binary.BigEndian.PutUint16(frame[:2], uint16(len(query)))
		copy(frame[2:], query)
		if _, err := conn.Write(frame); err != nil {
			lastErr = err
			_ = conn.Close()
			continue
		}
		var length [2]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			lastErr = err
			_ = conn.Close()
			continue
		}
		n := int(binary.BigEndian.Uint16(length[:]))
		if n < 12 || n > 65535 {
			lastErr = fmt.Errorf("invalid DNS TCP response length %d", n)
			_ = conn.Close()
			continue
		}
		reply := make([]byte, n)
		_, err = io.ReadFull(conn, reply)
		_ = conn.Close()
		if err == nil {
			return reply, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no DNS upstreams configured")
	}
	return servfail(query), lastErr
}

type question struct {
	Name        string
	Type        uint16
	Class       uint16
	QuestionEnd int
}

func parseQuestion(packet []byte) (question, error) {
	var q question
	if len(packet) < 12 {
		return q, errors.New("DNS packet is shorter than header")
	}
	if binary.BigEndian.Uint16(packet[4:6]) != 1 {
		return q, errors.New("Titanus DNS supports one question per request")
	}
	name, next, err := parseName(packet, 12, 0)
	if err != nil {
		return q, err
	}
	if next+4 > len(packet) {
		return q, errors.New("truncated DNS question")
	}
	q.Name = name
	q.Type = binary.BigEndian.Uint16(packet[next : next+2])
	q.Class = binary.BigEndian.Uint16(packet[next+2 : next+4])
	q.QuestionEnd = next + 4
	return q, nil
}

func parseName(packet []byte, offset, depth int) (string, int, error) {
	if depth > 8 {
		return "", 0, errors.New("DNS compression pointer depth exceeded")
	}
	labels := make([]string, 0, 4)
	cursor := offset
	next := -1
	for {
		if cursor >= len(packet) {
			return "", 0, errors.New("truncated DNS name")
		}
		length := int(packet[cursor])
		if length == 0 {
			cursor++
			if next < 0 {
				next = cursor
			}
			break
		}
		if length&0xc0 == 0xc0 {
			if cursor+1 >= len(packet) {
				return "", 0, errors.New("truncated DNS compression pointer")
			}
			pointer := int(binary.BigEndian.Uint16(packet[cursor:cursor+2]) & 0x3fff)
			name, _, err := parseName(packet, pointer, depth+1)
			if err != nil {
				return "", 0, err
			}
			if name != "" {
				labels = append(labels, name)
			}
			if next < 0 {
				next = cursor + 2
			}
			break
		}
		if length > 63 || cursor+1+length > len(packet) {
			return "", 0, errors.New("invalid DNS label")
		}
		labels = append(labels, string(packet[cursor+1:cursor+1+length]))
		cursor += 1 + length
	}
	return strings.Join(labels, "."), next, nil
}

func response(query []byte, q question, answer net.IP, rcode int, ttl uint32) []byte {
	if len(query) < q.QuestionEnd || q.QuestionEnd < 12 {
		return servfail(query)
	}
	out := make([]byte, q.QuestionEnd)
	copy(out, query[:q.QuestionEnd])
	flags := uint16(0x8400) | uint16(rcode&0xf)
	if len(query) >= 4 {
		flags |= binary.BigEndian.Uint16(query[2:4]) & 0x0100
	}
	flags |= 0x0080
	binary.BigEndian.PutUint16(out[2:4], flags)
	binary.BigEndian.PutUint16(out[4:6], 1)
	if answer == nil {
		binary.BigEndian.PutUint16(out[6:8], 0)
		binary.BigEndian.PutUint16(out[8:10], 0)
		binary.BigEndian.PutUint16(out[10:12], 0)
		return out
	}
	binary.BigEndian.PutUint16(out[6:8], 1)
	binary.BigEndian.PutUint16(out[8:10], 0)
	binary.BigEndian.PutUint16(out[10:12], 0)

	record := make([]byte, 16)
	binary.BigEndian.PutUint16(record[0:2], 0xc00c)
	binary.BigEndian.PutUint16(record[2:4], dnsTypeA)
	binary.BigEndian.PutUint16(record[4:6], dnsClassIN)
	binary.BigEndian.PutUint32(record[6:10], ttl)
	binary.BigEndian.PutUint16(record[10:12], 4)
	copy(record[12:16], answer.To4())
	return append(out, record...)
}

func servfail(query []byte) []byte {
	if len(query) < 12 {
		return nil
	}
	out := append([]byte(nil), query...)
	flags := uint16(0x8002)
	flags |= binary.BigEndian.Uint16(query[2:4]) & 0x0100
	flags |= 0x0080
	binary.BigEndian.PutUint16(out[2:4], flags)
	binary.BigEndian.PutUint16(out[6:8], 0)
	binary.BigEndian.PutUint16(out[8:10], 0)
	binary.BigEndian.PutUint16(out[10:12], 0)
	return out
}

func dnsAddress(value string) string {
	value = strings.TrimSpace(value)
	if host, port, err := net.SplitHostPort(value); err == nil && host != "" && port != "" {
		return value
	}
	return net.JoinHostPort(value, "53")
}

func SystemUpstreams(path, localIP string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	localIP = strings.TrimSpace(localIP)
	seen := map[string]bool{}
	out := make([]string, 0)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "nameserver" {
			continue
		}
		ip := net.ParseIP(fields[1])
		if ip == nil || fields[1] == localIP || seen[fields[1]] {
			continue
		}
		seen[fields[1]] = true
		out = append(out, fields[1])
	}
	return out
}

type Server struct {
	Resolver *Resolver
	Address  string

	mu  sync.Mutex
	udp *net.UDPConn
	tcp net.Listener
}

func (s *Server) Run(ctx context.Context) error {
	if s.Resolver == nil {
		return errors.New("Titanus DNS resolver is required")
	}
	address := strings.TrimSpace(s.Address)
	if address == "" {
		return errors.New("Titanus DNS listen address is required")
	}
	udpAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return err
	}
	udp, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("listen Titanus DNS UDP %s: %w", address, err)
	}
	tcp, err := net.Listen("tcp", address)
	if err != nil {
		_ = udp.Close()
		return fmt.Errorf("listen Titanus DNS TCP %s: %w", address, err)
	}
	s.mu.Lock()
	s.udp = udp
	s.tcp = tcp
	s.mu.Unlock()

	errCh := make(chan error, 2)
	go s.serveUDP(ctx, udp, errCh)
	go s.serveTCP(ctx, tcp, errCh)
	go func() {
		<-ctx.Done()
		_ = udp.Close()
		_ = tcp.Close()
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-errCh:
		_ = udp.Close()
		_ = tcp.Close()
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) serveUDP(ctx context.Context, conn *net.UDPConn, errCh chan<- error) {
	buf := make([]byte, 65535)
	for {
		n, peer, err := conn.ReadFromUDP(buf)
		if err != nil {
			errCh <- err
			return
		}
		query := append([]byte(nil), buf[:n]...)
		go func() {
			reply, _ := s.Resolver.HandleUDP(query)
			if len(reply) > 0 {
				_, _ = conn.WriteToUDP(reply, peer)
			}
		}()
	}
}

func (s *Server) serveTCP(ctx context.Context, listener net.Listener, errCh chan<- error) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			errCh <- err
			return
		}
		go s.handleTCPConn(ctx, conn)
	}
}

func (s *Server) handleTCPConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	for {
		var length [2]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(length[:]))
		if n < 12 || n > 65535 {
			return
		}
		query := make([]byte, n)
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}
		reply, _ := s.Resolver.HandleTCP(query)
		if len(reply) == 0 {
			return
		}
		frame := make([]byte, 2+len(reply))
		binary.BigEndian.PutUint16(frame[:2], uint16(len(reply)))
		copy(frame[2:], reply)
		if _, err := conn.Write(frame); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

func GatewayListenAddress(gatewayCIDR string) (string, error) {
	ip, _, err := net.ParseCIDR(strings.TrimSpace(gatewayCIDR))
	if err != nil || ip.To4() == nil {
		return "", fmt.Errorf("invalid IPv4 Fabric gateway %q", gatewayCIDR)
	}
	return net.JoinHostPort(ip.To4().String(), strconv.Itoa(53)), nil
}
