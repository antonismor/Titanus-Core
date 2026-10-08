package route

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type VIPBackend interface {
	Add(realm.Gateway) error
	Remove(realm.Gateway) error
	Has(realm.Gateway) (bool, error)
}
type LinuxVIP struct{}

func (LinuxVIP) run(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("VIP operation failed: %w", err)
	}
	return out, nil
}
func (l LinuxVIP) Has(g realm.Gateway) (bool, error) {
	out, err := l.run("-j", "address", "show", "dev", g.Device)
	if err != nil {
		return false, err
	}
	var devices []struct {
		Addresses []struct {
			Local string `json:"local"`
		} `json:"addr_info"`
	}
	if err = json.Unmarshal(out, &devices); err != nil {
		return false, err
	}
	ip, _, err := net.ParseCIDR(g.VIP)
	if err != nil {
		return false, err
	}
	for _, device := range devices {
		for _, address := range device.Addresses {
			if address.Local == ip.String() {
				return true, nil
			}
		}
	}
	return false, nil
}
func (l LinuxVIP) Add(g realm.Gateway) error {
	if err := g.Validate(); err != nil {
		return err
	}
	_, err := l.run("address", "add", g.VIP, "dev", g.Device)
	return err
}
func (l LinuxVIP) Remove(g realm.Gateway) error {
	if err := g.Validate(); err != nil {
		return err
	}
	present, err := l.Has(g)
	if err != nil || !present {
		return err
	}
	_, err = l.run("address", "del", g.VIP, "dev", g.Device)
	if err != nil {
		return err
	}
	present, err = l.Has(g)
	if err != nil {
		return err
	}
	if present {
		return fmt.Errorf("VIP remains installed")
	}
	return nil
}

type vipState struct {
	Node    string                   `json:"node"`
	Owned   map[string]realm.Gateway `json:"owned"`
	Retired map[string]realm.Gateway `json:"retired"`
}

func (m *Manager) ConfigureVIP(root, node string, backend VIPBackend) error {
	if node == "" || backend == nil {
		return fmt.Errorf("VIP node/backend required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodeID = node
	m.vip = backend
	m.vipPath = filepath.Join(root, "gateway", "ownership.json")
	if err := os.MkdirAll(filepath.Dir(m.vipPath), 0700); err != nil {
		return err
	}
	m.vipState = vipState{Node: node, Owned: map[string]realm.Gateway{}, Retired: map[string]realm.Gateway{}}
	data, err := os.ReadFile(m.vipPath)
	if err == nil {
		if err = json.Unmarshal(data, &m.vipState); err != nil {
			return err
		}
		if m.vipState.Node != node || m.vipState.Owned == nil || m.vipState.Retired == nil {
			return fmt.Errorf("invalid retained VIP identity")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// A restart begins with withdrawal, before reading quorum state. No stale
	// kernel address or cached Realm snapshot can authorize startup ownership.
	for name, g := range m.vipState.Owned {
		if err = backend.Remove(g); err != nil {
			return err
		}
		delete(m.vipState.Owned, name)
	}
	return m.saveVIPState()
}
func (m *Manager) saveVIPState() error {
	if m.vipPath == "" {
		return nil
	}
	return durable.WriteJSON(m.vipPath, m.vipState, 0600)
}
func (m *Manager) ownsGateway(state realm.State, name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ownsGatewayLocked(state, name)
}

func (m *Manager) ownsGatewayLocked(state realm.State, name string) bool {
	g, ok := state.Gateways[name]
	if !ok || m.vip == nil || g.Owner != m.nodeID {
		return false
	}
	old, retired := m.vipState.Retired[name]
	if retired && g.Epoch <= old.Epoch {
		return false
	}
	n, ok := state.Nodes[m.nodeID]
	return ok && n.State == realm.NodeReady && !n.PowerQuarantined && !n.StorageQuarantined
}
func (m *Manager) reconcileVIPs(state realm.State) error {
	if m.vip == nil {
		return nil
	}
	for name, g := range m.vipState.Owned {
		desired, ok := state.Gateways[name]
		if !ok || desired != g || !m.ownsGatewayLocked(state, name) {
			if err := m.vip.Remove(g); err != nil {
				return err
			}
			delete(m.vipState.Owned, name)
			if err := m.saveVIPState(); err != nil {
				return err
			}
		}
	}
	for name, g := range state.Gateways {
		if !m.ownsGatewayLocked(state, name) {
			continue
		}
		if _, ok := m.vipState.Owned[name]; ok {
			present, err := m.vip.Has(g)
			if err != nil {
				return err
			}
			if !present {
				if err = m.vip.Add(g); err != nil {
					return err
				}
			}
			continue
		}
		present, err := m.vip.Has(g)
		if err != nil {
			return err
		}
		if present {
			return fmt.Errorf("unrecorded VIP already exists")
		}
		m.vipState.Owned[name] = g
		if err = m.saveVIPState(); err != nil {
			delete(m.vipState.Owned, name)
			return err
		}
		if err = m.vip.Add(g); err != nil {
			return err
		}
		present, err = m.vip.Has(g)
		if err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("VIP acquisition not confirmed")
		}
	}
	return nil
}
func (m *Manager) Withdraw(g realm.Gateway) error {
	if err := g.Validate(); err != nil {
		return err
	}
	state, err := m.snapshot()
	if err != nil {
		return err
	}
	if state.Gateways[g.Name] != g {
		return fmt.Errorf("withdrawal does not match committed Gateway")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.vip == nil || g.Owner != m.nodeID {
		return fmt.Errorf("Gateway cannot withdraw on this node")
	}
	retired, ok := m.vipState.Retired[g.Name]
	if ok && retired.Epoch > g.Epoch {
		return fmt.Errorf("withdrawal epoch obsolete")
	}
	m.vipState.Retired[g.Name] = g
	if err = m.saveVIPState(); err != nil {
		return err
	}
	for name, listener := range m.listeners {
		if listener.spec.Gateway == g.Name {
			listener.close()
			delete(m.listeners, name)
		}
	}
	if err = m.vip.Remove(g); err != nil {
		return err
	}
	present, err := m.vip.Has(g)
	if err != nil {
		return err
	}
	if present {
		return fmt.Errorf("old VIP still present")
	}
	delete(m.vipState.Owned, g.Name)
	return m.saveVIPState()
}
