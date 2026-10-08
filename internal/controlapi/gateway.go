package controlapi

import (
	"context"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"log"
	"net/http"
	"strings"
	"time"
)

func (s *Server) MaintainGateways(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if s.Store.CheckLeader() == nil {
			if err := s.ReconcileGateways(); err != nil {
				log.Printf("Gateway failover pending: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type GatewayRuntime interface{ Withdraw(realm.Gateway) error }
type GatewayClient interface {
	WithdrawGateway(string, realm.Gateway) error
}
type PowerFencer interface {
	Fence(string, func() error) error
}

func (s *Server) gateway(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	p, ok := identity.RequestPrincipal(r)
	if !ok || p.Role != identity.RoleAdmin {
		writeError(w, 403, fmt.Errorf("Gateway management requires admin"))
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/v1/realm/gateways/")
	if strings.HasSuffix(name, "/transfer") {
		name = strings.TrimSuffix(name, "/transfer")
		var req struct {
			Destination string `json:"destination"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, 400, err)
			return
		}
		if err := s.TransferGateway(name, req.Destination); err != nil {
			writeError(w, 503, err)
			return
		}
		writeJSON(w, 200, s.Store.Snapshot().Gateways[name])
		return
	}
	var g realm.Gateway
	if err := decodeJSON(r, &g); err != nil {
		writeError(w, 400, err)
		return
	}
	if name != g.Name {
		writeError(w, 400, fmt.Errorf("Gateway name differs from URL"))
		return
	}
	if err := s.Store.PutGateway(g); err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 201, s.Store.Snapshot().Gateways[name])
}
func (s *Server) withdrawGateway(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if s.Gateway == nil {
		writeError(w, 503, fmt.Errorf("managed Gateway unavailable"))
		return
	}
	var g realm.Gateway
	if err := decodeJSON(r, &g); err != nil {
		writeError(w, 400, err)
		return
	}
	if err := s.Gateway.Withdraw(g); err != nil {
		writeError(w, 409, err)
		return
	}
	writeJSON(w, 200, g)
}
func (s *Server) TransferGateway(name, destination string) error {
	if err := s.Store.CheckLeader(); err != nil {
		return err
	}
	intent, err := s.Store.BeginGatewayTransfer(name, destination)
	if err != nil {
		return err
	}
	node, ok := s.Store.Snapshot().Nodes[intent.Gateway.Owner]
	if !ok {
		return fmt.Errorf("old Gateway node disappeared")
	}
	if s.GatewayClient == nil {
		return fmt.Errorf("Gateway client unavailable")
	}
	if err = s.Store.CheckLeader(); err != nil {
		return err
	}
	withdrawn := s.GatewayClient.WithdrawGateway(node.Address, intent.Gateway) == nil
	powerFenced := false
	if !withdrawn {
		if node.State != realm.NodeUnreachable || s.PowerFencer == nil {
			return fmt.Errorf("old VIP not withdrawn; failover waits for verified out-of-band fence")
		}
		if err = s.PowerFencer.Fence(node.ID, s.Store.CheckLeader); err != nil {
			return err
		}
		powerFenced = true
	}
	if err = s.Store.CheckLeader(); err != nil {
		return err
	}
	return s.Store.CompleteGatewayTransfer(intent, powerFenced)
}
func (s *Server) ReconcileGateways() error {
	for name, g := range s.Store.Snapshot().Gateways {
		if g.Standby != "" && s.Store.Snapshot().Nodes[g.Owner].State == realm.NodeUnreachable {
			if err := s.TransferGateway(name, g.Standby); err != nil {
				return err
			}
		}
	}
	return nil
}
