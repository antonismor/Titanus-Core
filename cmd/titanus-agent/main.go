package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/antonismor/Titanus-Core/internal/controlapi"
	"github.com/antonismor/Titanus-Core/internal/fabric"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/pulse"
	"github.com/antonismor/Titanus-Core/internal/realm"
)

type config struct {
	NodeID       string
	Address      string
	Controller   string
	CA           string
	Cert         string
	Key          string
	StateRoot    string
	Interval     time.Duration
	Capabilities []model.Capability
	Labels       map[string]string
}

func main() {
	var cfg config
	var caps string
	flag.StringVar(&cfg.NodeID, "node", "", "Titanus Node ID")
	flag.StringVar(&cfg.Address, "address", "", "Node management address")
	flag.StringVar(&cfg.Controller, "controller", "", "Realm controller URL, e.g. https://10.0.0.10:9443")
	flag.StringVar(&cfg.CA, "ca", "/etc/titanus/pki/ca.crt", "Realm CA certificate")
	flag.StringVar(&cfg.Cert, "cert", "/etc/titanus/pki/node.crt", "Node certificate")
	flag.StringVar(&cfg.Key, "key", "/etc/titanus/pki/node.key", "Node private key")
	flag.StringVar(&cfg.StateRoot, "state-root", "/var/lib/titanus", "Titanus state root")
	flag.DurationVar(&cfg.Interval, "interval", 10*time.Second, "Pulse interval")
	flag.StringVar(&caps, "capabilities", "EXECUTION", "comma-separated Titanus capabilities")
	flag.Parse()

	if cfg.NodeID == "" || cfg.Controller == "" {
		log.Fatal("--node and --controller are required")
	}
	parsedCaps, err := model.ParseCapabilities(caps)
	if err != nil {
		log.Fatal(err)
	}
	cfg.Capabilities = parsedCaps
	cfg.Labels = parseLabels(flag.Args())

	tlsConfig, err := identity.TLSConfig(cfg.CA, cfg.Cert, cfg.Key, false)
	if err != nil {
		log.Fatal(err)
	}
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsConfig},
		Timeout:   10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	registered, err := register(client, cfg)
	if err != nil {
		log.Fatalf("Realm registration failed: %v", err)
	}
	log.Printf("Titanus Agent registered Node %s with %s, Fabric=%s", cfg.NodeID, cfg.Controller, registered.FabricCIDR)
	if err := syncFabric(client, cfg, registered); err != nil {
		log.Printf("initial Fabric sync failed: %v", err)
	}

	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		if err := sendPulse(client, cfg); err != nil {
			log.Printf("Pulse failed: %v", err)
		}
		if err := syncFabric(client, cfg, registered); err != nil {
			log.Printf("Fabric sync failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func register(client *http.Client, cfg config) (realm.Node, error) {
	node := realm.Node{
		ID: cfg.NodeID, Address: cfg.Address,
		Capabilities: cfg.Capabilities, Labels: cfg.Labels,
		Resources: pulse.Discover(cfg.StateRoot),
		State: realm.NodeReady,
	}
	var registered realm.Node
	err := requestJSON(client, http.MethodPost, strings.TrimRight(cfg.Controller, "/")+"/v1/realm/nodes", node, &registered)
	return registered, err
}

func sendPulse(client *http.Client, cfg config) error {
	req := controlapi.PulseRequest{NodeID: cfg.NodeID, Resources: pulse.Discover(cfg.StateRoot)}
	return requestJSON(client, http.MethodPost, strings.TrimRight(cfg.Controller, "/")+"/v1/realm/pulse", req, nil)
}

func syncFabric(client *http.Client, cfg config, registered realm.Node) error {
	if registered.FabricCIDR == "" {
		return nil
	}
	var state realm.State
	if err := requestJSON(client, http.MethodGet, strings.TrimRight(cfg.Controller, "/")+"/v1/realm/state", nil, &state); err != nil {
		return err
	}
	manager := fabric.NewManager(cfg.StateRoot)
	current, configErr := manager.Config()
	if configErr != nil || current.CIDR != registered.FabricCIDR || current.Bridge != "titanus0" {
		if _, err := manager.Init(registered.FabricCIDR, "titanus0"); err != nil {
			return err
		}
	}
	peers := make([]fabric.Peer, 0)
	for _, node := range state.Nodes {
		if node.ID == cfg.NodeID || node.Address == "" || node.FabricCIDR == "" || node.State == realm.NodeDisabled {
			continue
		}
		peers = append(peers, fabric.Peer{NodeID: node.ID, VTEP: node.Address, CIDR: node.FabricCIDR})
	}
	_, err := manager.ConfigureMesh(cfg.Address, state.Network.VXLANID, peers)
	return err
}

func requestJSON(client *http.Client, method, url string, payload any, response any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if response != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(response)
	}
	return nil
}

func parseLabels(args []string) map[string]string {
	labels := map[string]string{}
	for _, arg := range args {
		if !strings.HasPrefix(arg, "label=") {
			continue
		}
		pair := strings.TrimPrefix(arg, "label=")
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			labels[parts[0]] = parts[1]
		}
	}
	return labels
}
