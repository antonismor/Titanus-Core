package realm

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/model"
	"net"
	"reflect"
	"regexp"
)

type Gateway struct {
	Name    string `json:"name"`
	VIP     string `json:"vip"`
	Device  string `json:"device"`
	Owner   string `json:"owner"`
	Standby string `json:"standby,omitempty"`
	Epoch   uint64 `json:"epoch"`
}
type GatewayTransfer struct {
	ID          string  `json:"id"`
	Gateway     Gateway `json:"gateway"`
	Destination string  `json:"destination"`
	Complete    bool    `json:"complete"`
	PowerFenced bool    `json:"power_fenced,omitempty"`
}

func (g Gateway) Validate() error {
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`).MatchString(g.Name) || !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,14}$`).MatchString(g.Device) {
		return fmt.Errorf("invalid Gateway name/device")
	}
	ip, prefix, err := net.ParseCIDR(g.VIP)
	if err != nil || ip.To4() == nil {
		return fmt.Errorf("Gateway requires IPv4 /32 VIP")
	}
	ones, bits := prefix.Mask.Size()
	if ones != 32 || bits != 32 {
		return fmt.Errorf("Gateway VIP must use /32")
	}
	if g.Owner == "" || g.Epoch == 0 {
		return fmt.Errorf("Gateway owner/epoch required")
	}
	return nil
}
func gatewayNode(state State, id string) bool {
	n, ok := state.Nodes[id]
	if !ok || n.State != NodeReady || n.StorageQuarantined || n.PowerQuarantined {
		return false
	}
	for _, capability := range n.Capabilities {
		if capability == model.CapabilityGateway {
			return true
		}
	}
	return false
}
func (s *Store) PutGateway(g Gateway) error {
	s.lock()
	defer s.unlock()
	g.Epoch = 1
	if err := g.Validate(); err != nil {
		return err
	}
	if !gatewayNode(s.data, g.Owner) || g.Standby != "" && (!gatewayNode(s.data, g.Standby) || g.Standby == g.Owner) {
		return fmt.Errorf("Gateway owner/standby must be distinct ready GATEWAY nodes")
	}
	if s.data.Gateways == nil {
		s.data.Gateways = map[string]Gateway{}
	}
	if old, ok := s.data.Gateways[g.Name]; ok {
		if old != g {
			return fmt.Errorf("Gateway identity/ownership changes require transfer")
		}
		return nil
	}
	for _, old := range s.data.Gateways {
		if old.VIP == g.VIP {
			return fmt.Errorf("Gateway VIP already reserved")
		}
	}
	s.data.Gateways[g.Name] = g
	return s.commitLocked()
}
func (s *Store) BeginGatewayTransfer(name, destination string) (GatewayTransfer, error) {
	s.lock()
	defer s.unlock()
	g, ok := s.data.Gateways[name]
	if !ok {
		return GatewayTransfer{}, fmt.Errorf("unknown Gateway")
	}
	if g.Owner == destination || !gatewayNode(s.data, destination) || g.Epoch == ^uint64(0) {
		return GatewayTransfer{}, fmt.Errorf("invalid Gateway destination/epoch")
	}
	for _, intent := range s.data.GatewayTransfers {
		if intent.Gateway.Name == name && !intent.Complete {
			if intent.Destination == destination {
				return intent, nil
			}
			return GatewayTransfer{}, fmt.Errorf("Gateway transfer already pending")
		}
	}
	b, _ := json.Marshal(struct {
		Gateway     Gateway
		Destination string
	}{g, destination})
	id := fmt.Sprintf("%x", sha256.Sum256(b))
	intent := GatewayTransfer{ID: id, Gateway: g, Destination: destination}
	if s.data.GatewayTransfers == nil {
		s.data.GatewayTransfers = map[string]GatewayTransfer{}
	}
	if len(s.data.GatewayTransfers) >= 128 {
		return intent, fmt.Errorf("Gateway transfer retention exhausted")
	}
	s.data.GatewayTransfers[id] = intent
	return intent, s.commitLocked()
}
func (s *Store) CompleteGatewayTransfer(intent GatewayTransfer, powerFenced bool) error {
	s.lock()
	defer s.unlock()
	stored, ok := s.data.GatewayTransfers[intent.ID]
	if !ok || !reflect.DeepEqual(stored, intent) || s.data.Gateways[intent.Gateway.Name] != intent.Gateway {
		return fmt.Errorf("Gateway intent changed")
	}
	if !gatewayNode(s.data, intent.Destination) {
		return fmt.Errorf("Gateway destination no longer eligible")
	}
	g := intent.Gateway
	g.Owner = intent.Destination
	g.Standby = ""
	g.Epoch++
	if powerFenced {
		n := s.data.Nodes[intent.Gateway.Owner]
		n.PowerQuarantined = true
		s.data.Nodes[n.ID] = n
	}
	s.data.Gateways[g.Name] = g
	stored.Complete = true
	stored.PowerFenced = powerFenced
	s.data.GatewayTransfers[intent.ID] = stored
	return s.commitLocked()
}
