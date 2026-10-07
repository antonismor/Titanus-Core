package consensus

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/hashicorp/raft"
)

var peerID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

func validatePeer(p Peer) error {
	if !peerID.MatchString(p.ID) {
		return fmt.Errorf("invalid controller identity")
	}
	host, port, err := net.SplitHostPort(p.Address)
	if err != nil || host == "" || port == "" {
		return fmt.Errorf("Raft address requires host:port")
	}
	u, err := url.Parse(p.API)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("controller API must be an HTTPS origin")
	}
	return nil
}

type tlsStream struct {
	listener       net.Listener
	server, client *tls.Config
	cfg            Config
	ca             string
	closed         atomic.Bool
	mu             sync.Mutex
	connections    map[net.Conn]bool
}

func newTLSStream(cfg Config, ca, cert, key string) (*tlsStream, error) {
	server, err := identity.TLSConfig(ca, cert, key, true)
	if err != nil {
		return nil, err
	}
	client, err := identity.TLSConfig(ca, cert, key, false)
	if err != nil {
		return nil, err
	}
	localCertPointer, err := client.GetClientCertificate(&tls.CertificateRequestInfo{})
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(localCertPointer.Certificate[0])
	if err != nil {
		return nil, err
	}
	p, err := identity.CertificatePrincipal(leaf)
	if err != nil || p.Role != identity.RoleController || p.ID != cfg.ID {
		return nil, fmt.Errorf("Raft local certificate must identify configured controller")
	}
	address := ""
	for _, p := range cfg.Peers {
		if p.ID == cfg.ID {
			address = p.Address
		}
	}
	binding, _ := json.Marshal(struct {
		Realm string
		Peers []Peer
		Seed  string
	}{cfg.Realm, cfg.Peers, cfg.SeedHash})
	protocol := fmt.Sprintf("titanus-raft-%x", sha256.Sum256(binding))
	server.NextProtos = []string{protocol}
	client.NextProtos = []string{protocol}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	return &tlsStream{listener: listener, server: server, client: client, cfg: cfg, ca: ca, connections: map[net.Conn]bool{}}, nil
}
func (s *tlsStream) authorize(cert *x509.Certificate, wanted string) error {
	if s.closed.Load() {
		return net.ErrClosed
	}
	if err := identity.ValidatePeer(cert, s.ca); err != nil {
		return err
	}
	p, err := identity.CertificatePrincipal(cert)
	if err != nil || p.Role != identity.RoleController {
		return fmt.Errorf("Raft requires controller role")
	}
	for _, peer := range s.cfg.Peers {
		if peer.ID == p.ID && peer.ID != s.cfg.ID && (wanted == "" || wanted == p.ID) {
			return nil
		}
	}
	return fmt.Errorf("Raft peer identity is not in configured voter set")
}
func (s *tlsStream) Accept() (net.Conn, error) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return nil, err
		}
		tc := tls.Server(conn, s.server)
		_ = tc.SetDeadline(time.Now().Add(2 * time.Second))
		err = tc.Handshake()
		if err == nil {
			if tc.ConnectionState().NegotiatedProtocol != s.server.NextProtos[0] {
				err = fmt.Errorf("Raft membership/Realm/seed binding mismatch")
			} else {
				err = s.authorize(tc.ConnectionState().PeerCertificates[0], "")
			}
		}
		if err != nil {
			_ = tc.Close()
			continue
		}
		_ = tc.SetDeadline(time.Time{})
		return s.track(tc, tc.ConnectionState().PeerCertificates[0])
	}
}
func (s *tlsStream) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	if s.closed.Load() {
		return nil, net.ErrClosed
	}
	wanted := ""
	for _, peer := range s.cfg.Peers {
		if peer.Address == string(address) {
			wanted = peer.ID
		}
	}
	if wanted == "" || wanted == s.cfg.ID {
		return nil, fmt.Errorf("unconfigured Raft target")
	}
	cfg := s.client.Clone()
	host, _, _ := net.SplitHostPort(string(address))
	cfg.ServerName = host
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", string(address), cfg)
	if err != nil {
		return nil, err
	}
	if conn.ConnectionState().NegotiatedProtocol != s.client.NextProtos[0] {
		_ = conn.Close()
		return nil, fmt.Errorf("Raft membership/Realm/seed binding mismatch")
	}
	cert := conn.ConnectionState().PeerCertificates[0]
	if err = s.authorize(cert, wanted); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return s.track(conn, cert)
}
func (s *tlsStream) Addr() net.Addr { return s.listener.Addr() }
func (s *tlsStream) Close() error {
	s.closed.Store(true)
	s.mu.Lock()
	for c := range s.connections {
		_ = c.Close()
		delete(s.connections, c)
	}
	s.mu.Unlock()
	return s.listener.Close()
}

// Recheck CRLs on established consensus streams, including heartbeat traffic.
// A revoked controller cannot retain a long-lived authenticated connection.
type checkedConn struct {
	net.Conn
	stream *tlsStream
	cert   *x509.Certificate
}

func (c *checkedConn) Read(p []byte) (int, error) {
	if err := c.stream.authorize(c.cert, ""); err != nil {
		_ = c.Conn.Close()
		return 0, err
	}
	return c.Conn.Read(p)
}
func (c *checkedConn) Write(p []byte) (int, error) {
	if err := c.stream.authorize(c.cert, ""); err != nil {
		_ = c.Conn.Close()
		return 0, err
	}
	return c.Conn.Write(p)
}

func (s *tlsStream) track(c net.Conn, cert *x509.Certificate) (net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		_ = c.Close()
		return nil, net.ErrClosed
	}
	s.connections[c] = true
	return &checkedConn{Conn: c, stream: s, cert: cert}, nil
}
func (c *checkedConn) Close() error {
	c.stream.mu.Lock()
	delete(c.stream.connections, c.Conn)
	c.stream.mu.Unlock()
	return c.Conn.Close()
}
