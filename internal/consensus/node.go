// Package consensus replicates Titanus Realm state inside titanusd. Raft is an
// embedded Go library, not a separate server or an external container platform.
package consensus

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/hashicorp/raft"
	raftbolt "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
)

const MaxStateBytes = 512 << 10

var ErrUnavailable = errors.New("Realm quorum/leader unavailable")
var ErrConflict = errors.New("Realm revision conflict; read committed state before retry")

type Peer struct {
	ID      string `json:"id"`
	Address string `json:"address"` // Raft host:port; must match certificate SAN
	API     string `json:"api"`     // HTTPS API endpoint
}
type Config struct {
	ID        string `json:"id"`
	Realm     string `json:"realm"`
	Peers     []Peer `json:"peers"`
	SeedHash  string `json:"seed_hash,omitempty"`
	Bootstrap bool   `json:"bootstrap"` // only the designated first controller
}

// Validate limits initial membership to 3 or 5 static voters. Membership changes
// require a coordinated configuration rollout and are not exposed over the API.
func (c Config) Validate() error {
	if c.Realm == "" || c.ID == "" {
		return fmt.Errorf("Realm and controller ID required")
	}
	if len(c.Peers) != 3 && len(c.Peers) != 5 {
		return fmt.Errorf("HA requires 3 or 5 CONTROL voters")
	}
	ids, addresses, apis := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, p := range c.Peers {
		if err := validatePeer(p); err != nil {
			return err
		}
		if ids[p.ID] || addresses[p.Address] || apis[p.API] {
			return fmt.Errorf("duplicate controller identity/address/API")
		}
		ids[p.ID] = true
		addresses[p.Address] = true
		apis[p.API] = true
	}
	if !ids[c.ID] {
		return fmt.Errorf("local controller absent from peers")
	}
	return nil
}

func LoadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 64<<10))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, fmt.Errorf("trailing HA configuration")
	}
	return c, c.Validate()
}

type Node struct {
	root      string
	raft      *raft.Raft
	fsm       *machine
	db        *raftbolt.BoltStore
	transport *raft.NetworkTransport
	cfg       Config
	seed      realm.State
	timeout   time.Duration
	closeOnce sync.Once
	closeErr  error
}

func Open(root string, cfg Config, seed realm.State, ca, cert, key string) (*Node, error) {
	if e := realm.ValidateSchema(seed); e != nil {
		return nil, e
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if seed.Name != cfg.Realm {
		return nil, fmt.Errorf("seed Realm does not match consensus Realm")
	}
	canonical := clone(seed)
	canonical.Revision = 0
	canonical.UpdatedAt = time.Time{}
	encoded, _ := json.Marshal(canonical)
	digest := fmt.Sprintf("%x", sha256.Sum256(encoded))
	if cfg.SeedHash != "" && cfg.SeedHash != digest {
		return nil, fmt.Errorf("HA seed differs from configured digest")
	}
	cfg.SeedHash = digest
	dir := filepath.Join(root, "realm", "consensus")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	// Persist membership/Realm binding: an existing Raft directory may never be
	// silently reused for a different identity or a changed initial voter set.
	binding := cfg
	binding.Bootstrap = false
	path := filepath.Join(dir, "membership.json")
	previous, err := os.ReadFile(path)
	if err == nil {
		data, _ := json.Marshal(binding)
		var old Config
		if json.Unmarshal(previous, &old) != nil {
			return nil, fmt.Errorf("corrupt HA membership")
		}
		oldData, _ := json.Marshal(old)
		if string(data) != string(oldData) {
			return nil, fmt.Errorf("persisted HA membership differs from config")
		}
	} else if os.IsNotExist(err) {
		if err = durable.WriteJSON(path, binding, 0600); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return nil, err
	}
	db, err := raftbolt.New(raftbolt.Options{Path: filepath.Join(dir, "raft.db"), BoltOptions: &bbolt.Options{Timeout: time.Second}})
	if err != nil {
		return nil, err
	}
	if err = syncDir(dir); err != nil {
		_ = db.Close()
		return nil, err
	}
	closeDB := true
	defer func() {
		if closeDB {
			_ = db.Close()
		}
	}()
	snapshots, err := raft.NewFileSnapshotStore(dir, 3, io.Discard)
	if err != nil {
		return nil, err
	}
	stream, err := newTLSStream(cfg, ca, cert, key)
	if err != nil {
		return nil, err
	}
	transport := raft.NewNetworkTransport(stream, 2, 2*time.Second, io.Discard)
	keepTransport := false
	defer func() {
		if !keepTransport {
			_ = transport.Close()
		}
	}()
	hasState, err := raft.HasExistingState(db, db, snapshots)
	if err != nil {
		return nil, err
	}
	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(cfg.ID)
	config.LogOutput = io.Discard
	config.HeartbeatTimeout = time.Second
	config.ElectionTimeout = time.Second
	config.LeaderLeaseTimeout = 500 * time.Millisecond
	config.SnapshotThreshold = 64
	config.TrailingLogs = 32
	if !hasState && cfg.Bootstrap {
		servers := []raft.Server{}
		for _, p := range cfg.Peers {
			servers = append(servers, raft.Server{ID: raft.ServerID(p.ID), Address: raft.ServerAddress(p.Address), Suffrage: raft.Voter})
		}
		if err := raft.BootstrapCluster(config, db, db, snapshots, transport, raft.Configuration{Servers: servers}); err != nil {
			return nil, err
		}
	}
	fsm := &machine{root: root, data: emptyState(cfg.Realm)}
	r, err := raft.NewRaft(config, fsm, db, db, snapshots, transport)
	if err != nil {
		return nil, err
	}
	closeDB = false
	keepTransport = true
	return &Node{root: root, raft: r, fsm: fsm, db: db, transport: transport, cfg: cfg, seed: clone(seed), timeout: 4 * time.Second}, nil
}

func emptyState(name string) realm.State {
	return realm.State{Name: name, Nodes: map[string]realm.Node{}, Fleets: map[string]realm.Fleet{}, Assignments: map[string]realm.Assignment{}, Routes: map[string]realm.Route{}, Policies: map[string]realm.NetworkPolicy{}}
}
func clone(s realm.State) realm.State {
	b, _ := json.Marshal(s)
	var c realm.State
	_ = json.Unmarshal(b, &c)
	return c
}
func (n *Node) Snapshot() realm.State { return n.fsm.state() }
func (n *Node) LeaderAPI() string {
	_, id := n.raft.LeaderWithID()
	for _, p := range n.cfg.Peers {
		if p.ID == string(id) {
			return p.API
		}
	}
	return ""
}
func (n *Node) Status() map[string]string {
	status := n.raft.Stats()
	status["id"] = n.cfg.ID
	status["realm"] = n.cfg.Realm
	status["leader_api"] = n.LeaderAPI()
	return status
}
func await(f raft.Future, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- f.Error() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("%w: outcome may be unknown", ErrUnavailable)
	}
}
func (n *Node) verify() error {
	if n.raft.State() != raft.Leader {
		return ErrUnavailable
	}
	if err := await(n.raft.VerifyLeader(), n.timeout); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if err := await(n.raft.Barrier(n.timeout), n.timeout); err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil
}
func (n *Node) CheckLeader() error {
	if err := n.verify(); err != nil {
		return err
	}
	if n.Snapshot().Revision == 0 {
		return fmt.Errorf("%w: Realm initialization pending", ErrUnavailable)
	}
	return nil
}

// Initialize is retried by the daemon before it starts serving Realm work. A
// fresh cluster must have the same exported seed on every controller. Once any
// state is committed, restart always replays Raft and ignores the legacy file.
func (n *Node) Initialize() error {
	if err := n.verify(); err != nil {
		return err
	}
	if n.Snapshot().Revision != 0 {
		return nil
	}
	candidate := clone(n.seed)
	candidate.Revision = 1
	return n.Apply(candidate)
}
func (n *Node) Apply(candidate realm.State) error {
	if n.raft.State() != raft.Leader {
		return ErrUnavailable
	}
	if candidate.Name != n.cfg.Realm {
		return fmt.Errorf("Realm identity mismatch")
	}
	b, err := json.Marshal(candidate)
	if err != nil {
		return err
	}
	if len(b) > MaxStateBytes {
		return fmt.Errorf("Realm state exceeds %d byte consensus limit", MaxStateBytes)
	}
	f := n.raft.Apply(b, n.timeout)
	if err = await(f, n.timeout); err != nil {
		return fmt.Errorf("%w: %v; write outcome may be unknown", ErrUnavailable, err)
	}
	if err, ok := f.Response().(error); ok {
		return err
	}
	return nil
}
func (n *Node) Close() error {
	n.closeOnce.Do(func() {
		n.closeErr = n.raft.Shutdown().Error()
		if e := n.transport.Close(); n.closeErr == nil {
			n.closeErr = e
		}
		if e := n.db.Close(); n.closeErr == nil {
			n.closeErr = e
		}
		if n.closeErr == nil {
			n.closeErr = n.writeOfflineCheckpoint()
		}
	})
	return n.closeErr
}

type machine struct {
	root string
	mu   sync.RWMutex
	data realm.State
}

func (m *machine) state() realm.State { m.mu.RLock(); defer m.mu.RUnlock(); return clone(m.data) }
func (m *machine) Apply(log *raft.Log) any {
	var next realm.State
	if len(log.Data) > MaxStateBytes || json.Unmarshal(log.Data, &next) != nil {
		return fmt.Errorf("invalid replicated Realm")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if next.Name != m.data.Name || next.Revision != m.data.Revision+1 {
		return ErrConflict
	}
	if next.Nodes == nil || next.Fleets == nil || next.Assignments == nil || next.Routes == nil || next.Policies == nil {
		return fmt.Errorf("incomplete replicated Realm")
	}
	if e := realm.ValidateSchemaChange(m.data, next); e != nil {
		return e
	}
	if next.SchemaVersion > 0 {
		if e := offline.CheckCommittedFloor(m.root, next.Name, next.SchemaMigrations[0].ID, next.SchemaVersion); e != nil {
			return e
		}
	}
	m.data = next
	return nil
}
func (m *machine) Snapshot() (raft.FSMSnapshot, error) { return &snapshot{data: m.state()}, nil }
func (m *machine) Restore(r io.ReadCloser) error {
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, MaxStateBytes+1))
	if err != nil {
		return err
	}
	var s realm.State
	if len(b) > MaxStateBytes || json.Unmarshal(b, &s) != nil {
		return fmt.Errorf("invalid Realm snapshot")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.Name != m.data.Name || s.Nodes == nil || s.Fleets == nil || s.Assignments == nil || s.Routes == nil || s.Policies == nil {
		return fmt.Errorf("snapshot Realm identity/state mismatch")
	}
	if e := realm.ValidateSchema(s); e != nil {
		return e
	}
	if s.SchemaVersion > 0 {
		if e := offline.CheckCommittedFloor(m.root, s.Name, s.SchemaMigrations[0].ID, s.SchemaVersion); e != nil {
			return e
		}
	}
	m.data = s
	return nil
}

type snapshot struct{ data realm.State }

func (s *snapshot) Persist(sink raft.SnapshotSink) error {
	b, err := json.Marshal(s.data)
	if err == nil {
		_, err = sink.Write(b)
	}
	if err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}
func (s *snapshot) Release() {}

func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
