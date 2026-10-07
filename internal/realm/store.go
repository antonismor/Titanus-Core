package realm

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/fabric"
	"github.com/antonismor/Titanus-Core/internal/model"
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

type Node struct {
	ID           string             `json:"id"`
	Address      string             `json:"address"`
	Capabilities []model.Capability `json:"capabilities"`
	Labels       map[string]string  `json:"labels,omitempty"`
	Resources    Resources          `json:"resources"`
	State        NodeState          `json:"state"`
	LastPulse    time.Time          `json:"last_pulse"`
	JoinedAt     time.Time          `json:"joined_at"`
	Failures     uint64             `json:"failures"`
	Successes    uint64             `json:"successes"`
}

type UnitTemplate struct {
	Source      string        `json:"source"`
	Command     []string      `json:"command"`
	Environment []string      `json:"environment,omitempty"`
	MemoryBytes int64         `json:"memory_bytes"`
	CPUPercent  int           `json:"cpu_percent"`
	PidsMax     int           `json:"pids_max"`
	Fabric      bool          `json:"fabric"`
	Ports       []fabric.Port `json:"ports,omitempty"`
	Mounts      []disk.Mount  `json:"mounts,omitempty"`
}

type Fleet struct {
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

type Assignment struct {
	ID         string          `json:"id"`
	Fleet      string          `json:"fleet"`
	NodeID     string          `json:"node_id"`
	State      AssignmentState `json:"state"`
	Generation uint64          `json:"generation"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type State struct {
	Name        string                `json:"name"`
	Revision    uint64                `json:"revision"`
	Nodes       map[string]Node       `json:"nodes"`
	Fleets      map[string]Fleet      `json:"fleets"`
	Assignments map[string]Assignment `json:"assignments"`
	UpdatedAt   time.Time             `json:"updated_at"`
}

type Store struct {
	path string
	mu   sync.Mutex
	data State
}

func Open(stateRoot, realmName string) (*Store, error) {
	if strings.TrimSpace(stateRoot) == "" {
		stateRoot = "/var/lib/titanus"
	}
	path := filepath.Join(stateRoot, "realm", "state.json")
	store := &Store{path: path}
	if err := store.load(realmName); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Snapshot() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneState(s.data)
}

func (s *Store) UpsertNode(node Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(node.ID) == "" {
		return fmt.Errorf("node ID is required")
	}
	if node.JoinedAt.IsZero() {
		if existing, ok := s.data.Nodes[node.ID]; ok {
			node.JoinedAt = existing.JoinedAt
		} else {
			node.JoinedAt = time.Now().UTC()
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
	s.data.Nodes[node.ID] = node
	return s.commitLocked()
}

func (s *Store) Pulse(nodeID string, resources Resources) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(fleet.Name) == "" {
		return fmt.Errorf("Fleet name is required")
	}
	if fleet.Instances < 0 {
		return fmt.Errorf("Fleet instances cannot be negative")
	}
	if fleet.MinimumAvailable < 0 || fleet.MinimumAvailable > fleet.Instances {
		return fmt.Errorf("Fleet minimum_available must be between 0 and instances")
	}
	if len(fleet.Template.Command) == 0 || strings.TrimSpace(fleet.Template.Source) == "" {
		return fmt.Errorf("Fleet requires Source and command")
	}
	if err := fabric.ValidatePorts(fleet.Template.Ports); err != nil {
		return err
	}
	if err := disk.ValidateMounts(fleet.Template.Mounts); err != nil {
		return err
	}
	if existing, ok := s.data.Fleets[fleet.Name]; ok {
		fleet.Generation = existing.Generation + 1
	} else {
		fleet.Generation = 1
	}
	fleet.UpdatedAt = time.Now().UTC()
	s.data.Fleets[fleet.Name] = fleet
	return s.commitLocked()
}

func (s *Store) SetAssignments(fleetName string, assignments []Assignment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *Store) UpdateAssignmentState(id string, next AssignmentState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
			Fleets: map[string]Fleet{}, Assignments: map[string]Assignment{},
			UpdatedAt: time.Now().UTC(),
		}
		return s.commitLocked()
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &s.data); err != nil {
		return err
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
	if s.data.Name == "" {
		s.data.Name = realmName
	}
	return nil
}

func (s *Store) commitLocked() error {
	s.data.Revision++
	s.data.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
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
