package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/ansi"
	"github.com/antonismor/Titanus-Core/internal/deploy"
	"github.com/antonismor/Titanus-Core/internal/disk"
	"github.com/antonismor/Titanus-Core/internal/fabric"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/localclient"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/planner"
	"github.com/antonismor/Titanus-Core/internal/preflight"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/setup"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
)

const version = "0.2.0-dev"

func main() {
	if len(os.Args) == 1 {
		if err := menu(); err != nil {
			ansi.Error(err.Error())
			os.Exit(1)
		}
		return
	}

	if err := dispatch(os.Args[1:]); err != nil {
		ansi.Error(err.Error())
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	switch args[0] {
	case "setup":
		return runSetup()
	case "realm":
		return runRealm(args[1:])
	case "source":
		return runSource(args[1:])
	case "fabric":
		return runFabric(args[1:])
	case "fleet":
		return runFleet(args[1:])
	case "route":
		return runRoute(args[1:])
	case "disk":
		return runDisk(args[1:])
	case "unit":
		return runUnit(args[1:])
	case "version", "--version", "-v":
		fmt.Println("Titanus Core", version)
		return nil
	case "plan":
		if len(args) < 3 {
			return fmt.Errorf("usage: titanus plan <show|validate> <plan.json>")
		}
		return runPlan(args[1], args[2])
	case "preflight":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus preflight <plan.json>")
		}
		return runPreflightFile(args[1])
	case "deploy":
		if len(args) != 3 || args[1] != "bootstrap" {
			return fmt.Errorf("usage: titanus deploy bootstrap <plan.json>")
		}
		return runBootstrapFile(args[2])
	case "help", "--help", "-h":
		printHelp()
		return nil
	default:
		return fmt.Errorf("unknown command %q; run titanus help", args[0])
	}
}

func menu() error {
	reader := bufio.NewReader(os.Stdin)
	for {
		ansi.Clear()
		ansi.Banner()
		fmt.Println(ansi.Paint(ansi.Bold+ansi.White, "Main Menu"))
		fmt.Println()
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "1)") + " Create / configure Realm")
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "2)") + " Validate deployment Plan")
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "3)") + " Run cluster pre-flight")
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "4)") + " Bootstrap Realm nodes")
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "5)") + " Manage Sources")
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "6)") + " Manage Units")
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "7)") + " Show version")
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "0)") + " Exit")
		fmt.Println()
		fmt.Print(ansi.Paint(ansi.Cyan, "Select") + ": ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		switch strings.TrimSpace(line) {
		case "1":
			if err := runSetup(); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "2":
			path, err := askPath(reader)
			if err != nil {
				return err
			}
			if err := runPlan("validate", path); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "3":
			path, err := askPath(reader)
			if err != nil {
				return err
			}
			if err := runPreflightFile(path); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "4":
			path, err := askPath(reader)
			if err != nil {
				return err
			}
			if err := runBootstrapFile(path); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "5":
			if err := sourceMenu(reader); err != nil {
				ansi.Error(err.Error())
				pause(reader)
			}
		case "6":
			if err := fabricMenu(reader); err != nil {
				ansi.Error(err.Error())
				pause(reader)
			}
		case "7":
			if err := diskMenu(reader); err != nil {
				ansi.Error(err.Error())
				pause(reader)
			}
		case "8":
			if err := realmMenu(reader); err != nil {
				ansi.Error(err.Error())
				pause(reader)
			}
		case "9":
			if err := fleetMenu(reader); err != nil {
				ansi.Error(err.Error())
				pause(reader)
			}
		case "10":
			if err := routeMenu(reader); err != nil {
				ansi.Error(err.Error())
				pause(reader)
			}
		case "11":
			if err := unitMenu(reader); err != nil {
				ansi.Error(err.Error())
				pause(reader)
			}
		case "12":
			fmt.Println("Titanus Core", version)
			pause(reader)
		case "0":
			return nil
		default:
			ansi.Warn("Unknown selection")
			pause(reader)
		}
	}
}

func sourceMenu(reader *bufio.Reader) error {
	for {
		ansi.Clear()
		ansi.Banner()
		fmt.Println(ansi.Paint(ansi.Bold+ansi.White, "Source Manager"))
		fmt.Println()
		fmt.Println("  1) List Sources")
		fmt.Println("  2) Import rootfs directory")
		fmt.Println("  0) Back")
		fmt.Print("\nSelect: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		switch strings.TrimSpace(line) {
		case "1":
			if err := runSource([]string{"list"}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "2":
			name, err := prompt(reader, "Source name")
			if err != nil {
				return err
			}
			rootfs, err := prompt(reader, "Rootfs directory")
			if err != nil {
				return err
			}
			if err := runSource([]string{"import", name, rootfs}); err != nil {
				ansi.Error(err.Error())
			} else {
				ansi.OK("Source imported")
			}
			pause(reader)
		case "0":
			return nil
		default:
			ansi.Warn("Unknown selection")
			pause(reader)
		}
	}
}

func fabricMenu(reader *bufio.Reader) error {
	for {
		ansi.Clear()
		ansi.Banner()
		fmt.Println(ansi.Paint(ansi.Bold+ansi.White, "Titanus Fabric"))
		fmt.Println()
		fmt.Println("  1) Initialize / update local Fabric")
		fmt.Println("  2) Show Fabric status")
		fmt.Println("  3) Show Unit allocations")
		fmt.Println("  0) Back")
		fmt.Print("\nSelect: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		switch strings.TrimSpace(line) {
		case "1":
			cidr, err := promptDefault(reader, "Unit CIDR", "10.240.0.0/16")
			if err != nil {
				return err
			}
			bridge, err := promptDefault(reader, "Bridge name", "titanus0")
			if err != nil {
				return err
			}
			if err := runFabric([]string{"init", "--cidr", cidr, "--bridge", bridge}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "2":
			if err := runFabric([]string{"status"}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "3":
			if err := runFabric([]string{"allocations"}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "0":
			return nil
		default:
			ansi.Warn("Unknown selection")
			pause(reader)
		}
	}
}

func runFabric(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: titanus fabric <init|status|allocations>")
	}
	manager := fabric.NewManager(stateRoot())
	switch args[0] {
	case "init":
		fs := flag.NewFlagSet("fabric init", flag.ContinueOnError)
		cidr := fs.String("cidr", "10.240.0.0/16", "Unit address range")
		bridge := fs.String("bridge", "titanus0", "Linux bridge name")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		cfg, err := manager.Init(*cidr, *bridge)
		if err != nil {
			return err
		}
		ansi.OK(fmt.Sprintf("Fabric ready: bridge=%s cidr=%s gateway=%s", cfg.Bridge, cfg.CIDR, cfg.Gateway))
		return nil
	case "status":
		cfg, err := manager.Config()
		if err != nil {
			return err
		}
		fmt.Printf("Bridge:  %s\nCIDR:    %s\nGateway: %s\n", cfg.Bridge, cfg.CIDR, cfg.Gateway)
		return nil
	case "allocations":
		items, err := manager.Allocations()
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println("No Fabric allocations.")
			return nil
		}
		fmt.Printf("%-24s %-16s %-12s %-8s\n", "UNIT", "ADDRESS", "HOST-IF", "ACTIVE")
		for _, item := range items {
			fmt.Printf("%-24s %-16s %-12s %-8t\n", item.UnitID, item.Address, item.HostIf, item.Active)
		}
		return nil
	default:
		return fmt.Errorf("unknown Fabric action %q", args[0])
	}
}

func diskMenu(reader *bufio.Reader) error {
	for {
		ansi.Clear()
		ansi.Banner()
		fmt.Println(ansi.Paint(ansi.Bold+ansi.White, "Titanus Disks"))
		fmt.Println()
		fmt.Println("  1) List Disks")
		fmt.Println("  2) Create local Disk")
		fmt.Println("  3) Configure Ceph adapter")
		fmt.Println("  4) Create Ceph RBD Disk")
		fmt.Println("  5) Create CephFS Disk")
		fmt.Println("  6) Inspect Disk")
		fmt.Println("  0) Back")
		fmt.Print("\nSelect: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		switch strings.TrimSpace(line) {
		case "1":
			if err := runDisk([]string{"list"}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "2", "4", "5":
			name, err := prompt(reader, "Disk name")
			if err != nil {
				return err
			}
			size, err := promptDefault(reader, "Capacity", "10G")
			if err != nil {
				return err
			}
			provider := "local"
			if strings.TrimSpace(line) == "4" {
				provider = "ceph-rbd"
			} else if strings.TrimSpace(line) == "5" {
				provider = "cephfs"
			}
			if err := runDisk([]string{"create", name, "--provider", provider, "--size", size}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "3":
			pool, err := promptDefault(reader, "RBD pool", "titanus")
			if err != nil {
				return err
			}
			fsName, err := promptDefault(reader, "CephFS name", "cephfs")
			if err != nil {
				return err
			}
			client, err := promptDefault(reader, "Ceph client", "client.titanus")
			if err != nil {
				return err
			}
			conf, err := promptDefault(reader, "Ceph config", "/etc/ceph/ceph.conf")
			if err != nil {
				return err
			}
			if err := runDisk([]string{"ceph-config", "--pool", pool, "--fs", fsName, "--client", client, "--conf", conf}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "6":
			name, err := prompt(reader, "Disk name")
			if err != nil {
				return err
			}
			if err := runDisk([]string{"inspect", name}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "0":
			return nil
		default:
			ansi.Warn("Unknown selection")
			pause(reader)
		}
	}
}

func runDisk(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: titanus disk <create|list|inspect|delete|ceph-config>")
	}
	manager := disk.NewManager(stateRoot())
	switch args[0] {
	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: titanus disk create NAME --provider local|ceph-rbd|cephfs --size SIZE")
		}
		name := args[1]
		fs := flag.NewFlagSet("disk create", flag.ContinueOnError)
		provider := fs.String("provider", "local", "Disk provider")
		sizeText := fs.String("size", "10G", "Disk capacity")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		size, err := disk.ParseBytes(*sizeText)
		if err != nil {
			return err
		}
		spec, err := manager.Create(disk.Spec{Name: name, Provider: disk.Provider(*provider), SizeBytes: size})
		if err != nil {
			return err
		}
		ansi.OK(fmt.Sprintf("Disk %s created: provider=%s size=%d bytes", spec.Name, spec.Provider, spec.SizeBytes))
		return nil
	case "list":
		items, err := manager.List()
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println("No Titanus Disks.")
			return nil
		}
		fmt.Printf("%-24s %-12s %-14s %-12s\n", "DISK", "PROVIDER", "SIZE-BYTES", "INITIALIZED")
		for _, item := range items {
			fmt.Printf("%-24s %-12s %-14d %-12t\n", item.Name, item.Provider, item.SizeBytes, item.Initialized)
		}
		return nil
	case "inspect":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus disk inspect NAME")
		}
		spec, err := manager.Inspect(args[1])
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(spec, "", "  ")
		fmt.Println(string(data))
		return nil
	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: titanus disk delete NAME [--destroy-data]")
		}
		fs := flag.NewFlagSet("disk delete", flag.ContinueOnError)
		destroy := fs.Bool("destroy-data", false, "destroy remote Ceph data")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if err := manager.Delete(args[1], *destroy); err != nil {
			return err
		}
		ansi.OK("Disk deleted: " + args[1])
		return nil
	case "ceph-config":
		fs := flag.NewFlagSet("disk ceph-config", flag.ContinueOnError)
		cluster := fs.String("cluster", "ceph", "Ceph cluster name")
		pool := fs.String("pool", "titanus", "RBD pool")
		fsName := fs.String("fs", "cephfs", "CephFS filesystem name")
		client := fs.String("client", "client.titanus", "Ceph client identity")
		conf := fs.String("conf", "/etc/ceph/ceph.conf", "Ceph configuration file")
		keyring := fs.String("keyring", "", "optional Ceph keyring")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		cfg := disk.CephConfig{
			Cluster: *cluster, Pool: *pool, FSName: *fsName,
			Client: *client, Conf: *conf, Keyring: *keyring,
		}
		if err := manager.ConfigureCeph(cfg); err != nil {
			return err
		}
		ansi.OK("Titanus Ceph adapter configured")
		return nil
	default:
		return fmt.Errorf("unknown Disk action %q", args[0])
	}
}

func realmMenu(reader *bufio.Reader) error {
	for {
		ansi.Clear()
		ansi.Banner()
		fmt.Println(ansi.Paint(ansi.Bold+ansi.White, "Titanus Realm"))
		fmt.Println()
		fmt.Println("  1) Initialize local Realm controller")
		fmt.Println("  2) Show Realm state")
		fmt.Println("  3) Issue certificate for another Node")
		fmt.Println("  0) Back")
		fmt.Print("\nSelect: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		switch strings.TrimSpace(line) {
		case "1":
			name, err := promptDefault(reader, "Realm name", "TITANUS-REALM")
			if err != nil {
				return err
			}
			node, err := promptDefault(reader, "This Node ID", "titanus01")
			if err != nil {
				return err
			}
			address, err := prompt(reader, "This Node management IP")
			if err != nil {
				return err
			}
			if err := runRealm([]string{"init", "--name", name, "--node", node, "--address", address}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "2":
			if err := runRealm([]string{"status"}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "3":
			node, err := prompt(reader, "New Node ID")
			if err != nil {
				return err
			}
			address, err := prompt(reader, "New Node management IP")
			if err != nil {
				return err
			}
			if err := runRealm([]string{"issue-node", "--node", node, "--address", address}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "0":
			return nil
		default:
			ansi.Warn("Unknown selection")
			pause(reader)
		}
	}
}

func runRealm(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: titanus realm <init|seed|status|issue-node>")
	}
	switch args[0] {
	case "init":
		fs := flag.NewFlagSet("realm init", flag.ContinueOnError)
		name := fs.String("name", "TITANUS-REALM", "Realm name")
		nodeID := fs.String("node", "", "local Node ID")
		address := fs.String("address", "", "local management address")
		listen := fs.String("listen", "0.0.0.0:9443", "mTLS Realm listen address")
		pkiDir := fs.String("pki-dir", "/etc/titanus/pki", "Titanus PKI directory")
		capText := fs.String("capabilities", "CONTROL,EXECUTION", "Node capabilities")
		fabricCIDR := fs.String("fabric-cidr", "10.240.0.0/16", "Realm Unit Fabric CIDR")
		serviceCIDR := fs.String("service-cidr", "10.250.0.0/16", "Realm service CIDR")
		nodePrefix := fs.Int("node-prefix", 24, "per-Node Fabric prefix")
		vxlanID := fs.Int("vxlan-id", 4242, "Titanus VXLAN ID")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if os.Geteuid() != 0 {
			return fmt.Errorf("Realm initialization requires root")
		}
		if strings.TrimSpace(*nodeID) == "" || strings.TrimSpace(*address) == "" {
			return fmt.Errorf("--node and --address are required")
		}
		caps, err := model.ParseCapabilities(*capText)
		if err != nil {
			return err
		}
		auth, err := identity.InitAuthority(*pkiDir, *name)
		if err != nil {
			return err
		}
		cert, key, err := auth.IssueNode(*nodeID, []string{*address})
		if err != nil {
			return err
		}
		store, err := realm.Open(stateRoot(), *name)
		if err != nil {
			return err
		}
		if err := store.ConfigureNetwork(realm.RealmNetwork{
			FabricCIDR: *fabricCIDR, ServiceCIDR: *serviceCIDR,
			NodePrefix: *nodePrefix, VXLANID: *vxlanID,
		}); err != nil {
			return err
		}
		if err := store.UpsertNode(realm.Node{
			ID: *nodeID, Address: *address, Capabilities: caps,
			State: realm.NodeReady, Labels: map[string]string{},
		}); err != nil {
			return err
		}
		if err := os.MkdirAll("/etc/titanus", 0755); err != nil {
			return err
		}
		daemonEnv := fmt.Sprintf("TITANUS_STATE_ROOT=%s\nTITANUS_REALM_NAME=%s\nTITANUS_CONTROLLER_MODE=true\nTITANUS_GATEWAY_MODE=true\nTITANUS_CLUSTER_LISTEN=%s\nTITANUS_CA=%s\nTITANUS_CERT=%s\nTITANUS_KEY=%s\n",
			stateRoot(), *name, *listen, auth.CertPath, cert, key)
		if err := os.WriteFile("/etc/titanus/daemon.env", []byte(daemonEnv), 0600); err != nil {
			return err
		}
		agentEnv := fmt.Sprintf("TITANUS_NODE_ID=%s\nTITANUS_NODE_ADDRESS=%s\nTITANUS_CONTROLLER=https://%s:9443\nTITANUS_CA=%s\nTITANUS_CERT=%s\nTITANUS_KEY=%s\nTITANUS_CAPABILITIES=%s\n",
			*nodeID, *address, *address, auth.CertPath, cert, key, *capText)
		if err := os.WriteFile("/etc/titanus/agent.env", []byte(agentEnv), 0600); err != nil {
			return err
		}
		ansi.OK(fmt.Sprintf("Realm %s initialized. Node=%s CA=%s", *name, *nodeID, auth.CertPath))
		return nil

	case "seed":
		fs := flag.NewFlagSet("realm seed", flag.ContinueOnError)
		name := fs.String("name", "TITANUS-REALM", "Realm name")
		fabricCIDR := fs.String("fabric-cidr", "10.240.0.0/16", "Realm Unit Fabric CIDR")
		serviceCIDR := fs.String("service-cidr", "10.250.0.0/16", "Realm service CIDR")
		nodePrefix := fs.Int("node-prefix", 24, "per-Node Fabric prefix")
		vxlanID := fs.Int("vxlan-id", 4242, "Titanus VXLAN ID")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if os.Geteuid() != 0 {
			return fmt.Errorf("Realm seed requires root")
		}
		store, err := realm.Open(stateRoot(), *name)
		if err != nil {
			return err
		}
		if err := store.ConfigureNetwork(realm.RealmNetwork{
			FabricCIDR: *fabricCIDR, ServiceCIDR: *serviceCIDR,
			NodePrefix: *nodePrefix, VXLANID: *vxlanID,
		}); err != nil {
			return err
		}
		ansi.OK(fmt.Sprintf("Realm %s network seeded: Fabric=%s Services=%s", *name, *fabricCIDR, *serviceCIDR))
		return nil

	case "status":
		store, err := realm.Open(stateRoot(), "TITANUS-REALM")
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(store.Snapshot(), "", "  ")
		fmt.Println(string(data))
		return nil

	case "issue-node":
		fs := flag.NewFlagSet("realm issue-node", flag.ContinueOnError)
		nodeID := fs.String("node", "", "Node ID")
		address := fs.String("address", "", "Node address")
		realmName := fs.String("realm", "TITANUS-REALM", "Realm name")
		pkiDir := fs.String("pki-dir", "/etc/titanus/pki", "Titanus PKI directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if os.Geteuid() != 0 {
			return fmt.Errorf("certificate issuance requires root")
		}
		if *nodeID == "" || *address == "" {
			return fmt.Errorf("--node and --address are required")
		}
		auth, err := identity.InitAuthority(*pkiDir, *realmName)
		if err != nil {
			return err
		}
		cert, key, err := auth.IssueNode(*nodeID, []string{*address})
		if err != nil {
			return err
		}
		fmt.Printf("CA:   %s\nCert: %s\nKey:  %s\n", auth.CertPath, cert, key)
		return nil
	default:
		return fmt.Errorf("unknown Realm action %q", args[0])
	}
}

func fleetMenu(reader *bufio.Reader) error {
	for {
		ansi.Clear()
		ansi.Banner()
		fmt.Println(ansi.Paint(ansi.Bold+ansi.White, "Titanus Fleets"))
		fmt.Println()
		fmt.Println("  1) List Fleets")
		fmt.Println("  2) Create Fleet")
		fmt.Println("  3) Fleet status")
		fmt.Println("  4) Scale Fleet")
		fmt.Println("  5) Delete Fleet")
		fmt.Println("  0) Back")
		fmt.Print("\nSelect: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		switch strings.TrimSpace(line) {
		case "1":
			if err := runFleet([]string{"list"}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "2":
			if err := interactiveFleetCreate(reader); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "3":
			name, err := prompt(reader, "Fleet name")
			if err != nil {
				return err
			}
			if err := runFleet([]string{"status", name}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "4":
			name, err := prompt(reader, "Fleet name")
			if err != nil {
				return err
			}
			count, err := promptDefault(reader, "Instances", "3")
			if err != nil {
				return err
			}
			if err := runFleet([]string{"scale", name, count}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "5":
			name, err := prompt(reader, "Fleet name")
			if err != nil {
				return err
			}
			if err := runFleet([]string{"delete", name}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "0":
			return nil
		default:
			ansi.Warn("Unknown selection")
			pause(reader)
		}
	}
}

func interactiveFleetCreate(reader *bufio.Reader) error {
	name, err := prompt(reader, "Fleet name")
	if err != nil {
		return err
	}
	src, err := prompt(reader, "Source")
	if err != nil {
		return err
	}
	instances, err := promptDefault(reader, "Instances", "3")
	if err != nil {
		return err
	}
	memory, err := promptDefault(reader, "Memory per Unit", "512M")
	if err != nil {
		return err
	}
	cpu, err := promptDefault(reader, "CPU per Unit (%)", "100")
	if err != nil {
		return err
	}
	command, err := promptDefault(reader, "Executable", "/bin/sh")
	if err != nil {
		return err
	}
	arguments, err := promptDefault(reader, "Arguments", "")
	if err != nil {
		return err
	}
	spread, err := promptDefault(reader, "Spread label (blank = none)", "")
	if err != nil {
		return err
	}
	args := []string{
		"create", name, "--source", src, "--instances", instances,
		"--memory", memory, "--cpu", cpu, "--fabric",
	}
	if spread != "" {
		args = append(args, "--spread", spread)
	}
	args = append(args, "--", command)
	if arguments != "" {
		args = append(args, strings.Fields(arguments)...)
	}
	return runFleet(args)
}

func runFleet(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: titanus fleet <create|list|status|scale|delete>")
	}
	client := localclient.New("/run/titanus/titanus.sock")
	switch args[0] {
	case "list":
		fleets, err := client.ListFleets()
		if err != nil {
			return err
		}
		if len(fleets) == 0 {
			fmt.Println("No Titanus Fleets.")
			return nil
		}
		fmt.Printf("%-24s %-10s %-16s %-12s\n", "FLEET", "INSTANCES", "SOURCE", "GENERATION")
		for _, fleet := range fleets {
			fmt.Printf("%-24s %-10d %-16s %-12d\n", fleet.Name, fleet.Instances, fleet.Template.Source, fleet.Generation)
		}
		return nil

	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: titanus fleet create NAME --source SOURCE [options] -- COMMAND [ARGS...]")
		}
		name := args[1]
		fs := flag.NewFlagSet("fleet create", flag.ContinueOnError)
		sourceName := fs.String("source", "", "Titanus Source")
		instances := fs.Int("instances", 1, "desired Unit count")
		minAvailable := fs.Int("minimum", 1, "minimum desired availability")
		memory := fs.String("memory", "512M", "memory per Unit")
		cpu := fs.Int("cpu", 100, "CPU percentage per Unit")
		pids := fs.Int("pids", 256, "maximum processes per Unit")
		fabricEnabled := fs.Bool("fabric", false, "attach Units to Titanus Fabric")
		publish := fs.String("publish", "", "comma-separated HOST:UNIT[/tcp|udp]")
		mountText := fs.String("mount", "", "comma-separated DISK:/path[:ro]")
		spread := fs.String("spread", "", "label key used to spread replicas")
		require := fs.String("require", "", "comma-separated label=value placement requirements")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if *sourceName == "" || len(fs.Args()) == 0 {
			return fmt.Errorf("--source and Unit command after -- are required")
		}
		memBytes, err := unitruntime.ParseBytes(*memory)
		if err != nil {
			return err
		}
		var ports []fabric.Port
		if strings.TrimSpace(*publish) != "" {
			for _, raw := range strings.Split(*publish, ",") {
				port, err := fabric.ParsePort(strings.TrimSpace(raw))
				if err != nil {
					return err
				}
				ports = append(ports, port)
			}
		}
		var mounts []disk.Mount
		if strings.TrimSpace(*mountText) != "" {
			for _, raw := range strings.Split(*mountText, ",") {
				mount, err := disk.ParseMount(strings.TrimSpace(raw))
				if err != nil {
					return err
				}
				mounts = append(mounts, mount)
			}
		}
		labels := map[string]string{}
		if strings.TrimSpace(*require) != "" {
			for _, raw := range strings.Split(*require, ",") {
				parts := strings.SplitN(strings.TrimSpace(raw), "=", 2)
				if len(parts) != 2 || parts[0] == "" {
					return fmt.Errorf("invalid required label %q", raw)
				}
				labels[parts[0]] = parts[1]
			}
		}
		fleet := realm.Fleet{
			Name: name, Instances: *instances, MinimumAvailable: *minAvailable,
			RequiredLabels: labels, SpreadLabel: strings.TrimSpace(*spread),
			Template: realm.UnitTemplate{
				Source: *sourceName, Command: fs.Args(),
				MemoryBytes: memBytes, CPUPercent: *cpu, PidsMax: *pids,
				Fabric: *fabricEnabled || len(ports) > 0,
				Ports: ports, Mounts: mounts,
			},
		}
		result, err := client.CreateFleet(fleet)
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(data))
		ansi.OK("Fleet accepted by Titanus Realm")
		return nil

	case "status":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus fleet status NAME")
		}
		result, err := client.FleetStatus(args[1])
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(data))
		return nil

	case "scale":
		if len(args) != 3 {
			return fmt.Errorf("usage: titanus fleet scale NAME INSTANCES")
		}
		count, err := strconv.Atoi(args[2])
		if err != nil || count < 0 {
			return fmt.Errorf("invalid instance count %q", args[2])
		}
		fleet, err := client.ScaleFleet(args[1], count)
		if err != nil {
			return err
		}
		ansi.OK(fmt.Sprintf("Fleet %s desired instances = %d", fleet.Name, fleet.Instances))
		return nil

	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus fleet delete NAME")
		}
		if err := client.DeleteFleet(args[1]); err != nil {
			return err
		}
		ansi.OK("Fleet deleted; Titanus will retire its Units")
		return nil

	default:
		return fmt.Errorf("unknown Fleet action %q", args[0])
	}
}

func routeMenu(reader *bufio.Reader) error {
	for {
		ansi.Clear()
		ansi.Banner()
		fmt.Println(ansi.Paint(ansi.Bold+ansi.White, "Titanus Routes"))
		fmt.Println()
		fmt.Println("  1) List Routes")
		fmt.Println("  2) Create Route")
		fmt.Println("  3) Inspect Route")
		fmt.Println("  4) Delete Route")
		fmt.Println("  0) Back")
		fmt.Print("\nSelect: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		switch strings.TrimSpace(line) {
		case "1":
			if err := runRoute([]string{"list"}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "2":
			name, err := prompt(reader, "Route name")
			if err != nil {
				return err
			}
			fleet, err := prompt(reader, "Target Fleet")
			if err != nil {
				return err
			}
			listen, err := promptDefault(reader, "Listen IP", "0.0.0.0")
			if err != nil {
				return err
			}
			listenPort, err := promptDefault(reader, "Listen port", "8080")
			if err != nil {
				return err
			}
			targetPort, err := promptDefault(reader, "Fleet target port", "80")
			if err != nil {
				return err
			}
			if err := runRoute([]string{"create", name, "--fleet", fleet, "--listen", listen, "--port", listenPort, "--target-port", targetPort}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "3":
			name, err := prompt(reader, "Route name")
			if err != nil {
				return err
			}
			if err := runRoute([]string{"status", name}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "4":
			name, err := prompt(reader, "Route name")
			if err != nil {
				return err
			}
			if err := runRoute([]string{"delete", name}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "0":
			return nil
		default:
			ansi.Warn("Unknown selection")
			pause(reader)
		}
	}
}

func runRoute(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: titanus route <create|list|status|delete>")
	}
	client := localclient.New("/run/titanus/titanus.sock")
	switch args[0] {
	case "list":
		routes, err := client.ListRoutes()
		if err != nil {
			return err
		}
		if len(routes) == 0 {
			fmt.Println("No Titanus Routes.")
			return nil
		}
		fmt.Printf("%-20s %-20s %-20s %-12s\n", "ROUTE", "FLEET", "LISTEN", "TARGET")
		for _, route := range routes {
			fmt.Printf("%-20s %-20s %-20s %-12s\n",
				route.Name, route.Fleet,
				net.JoinHostPort(route.ListenIP, strconv.Itoa(route.ListenPort)),
				strconv.Itoa(route.TargetPort))
		}
		return nil
	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: titanus route create NAME --fleet FLEET --port PORT --target-port PORT")
		}
		name := args[1]
		fs := flag.NewFlagSet("route create", flag.ContinueOnError)
		fleetName := fs.String("fleet", "", "target Fleet")
		listenIP := fs.String("listen", "0.0.0.0", "listen IP")
		listenPort := fs.Int("port", 0, "listen port")
		targetPort := fs.Int("target-port", 0, "target Unit port")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		stored, err := client.CreateRoute(realm.Route{
			Name: name, Fleet: *fleetName, ListenIP: *listenIP,
			ListenPort: *listenPort, TargetPort: *targetPort, Protocol: "tcp",
		})
		if err != nil {
			return err
		}
		ansi.OK(fmt.Sprintf("Route %s: %s:%d -> Fleet %s:%d",
			stored.Name, stored.ListenIP, stored.ListenPort, stored.Fleet, stored.TargetPort))
		return nil
	case "status":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus route status NAME")
		}
		route, err := client.RouteStatus(args[1])
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(route, "", "  ")
		fmt.Println(string(data))
		return nil
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus route delete NAME")
		}
		if err := client.DeleteRoute(args[1]); err != nil {
			return err
		}
		ansi.OK("Route deleted: " + args[1])
		return nil
	default:
		return fmt.Errorf("unknown Route action %q", args[0])
	}
}

func unitMenu(reader *bufio.Reader) error {
	for {
		ansi.Clear()
		ansi.Banner()
		fmt.Println(ansi.Paint(ansi.Bold+ansi.White, "Unit Runtime"))
		fmt.Println()
		fmt.Println("  1) List Units")
		fmt.Println("  2) Create Unit")
		fmt.Println("  3) Start Unit")
		fmt.Println("  4) Stop Unit")
		fmt.Println("  5) Inspect Unit")
		fmt.Println("  6) Show Unit logs")
		fmt.Println("  7) Delete Unit")
		fmt.Println("  0) Back")
		fmt.Print("\nSelect: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}

		switch strings.TrimSpace(line) {
		case "1":
			if err := runUnit([]string{"list"}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "2":
			if err := interactiveUnitCreate(reader); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "3", "4", "5", "6", "7":
			id, err := prompt(reader, "Unit ID")
			if err != nil {
				return err
			}
			action := map[string]string{
				"3": "start",
				"4": "stop",
				"5": "inspect",
				"6": "logs",
				"7": "delete",
			}[strings.TrimSpace(line)]
			if err := runUnit([]string{action, id}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "0":
			return nil
		default:
			ansi.Warn("Unknown selection")
			pause(reader)
		}
	}
}

func interactiveUnitCreate(reader *bufio.Reader) error {
	id, err := prompt(reader, "Unit ID")
	if err != nil {
		return err
	}
	src, err := prompt(reader, "Source")
	if err != nil {
		return err
	}
	hostname, err := promptDefault(reader, "Hostname", id)
	if err != nil {
		return err
	}
	memory, err := promptDefault(reader, "Memory limit", "512M")
	if err != nil {
		return err
	}
	cpu, err := promptDefault(reader, "CPU limit (%)", "100")
	if err != nil {
		return err
	}
	pids, err := promptDefault(reader, "Maximum processes", "256")
	if err != nil {
		return err
	}
	command, err := promptDefault(reader, "Executable", "/bin/sh")
	if err != nil {
		return err
	}
	arguments, err := promptDefault(reader, "Arguments (space separated)", "")
	if err != nil {
		return err
	}

	args := []string{"create", id, "--source", src, "--hostname", hostname, "--memory", memory, "--cpu", cpu, "--pids", pids, "--", command}
	if strings.TrimSpace(arguments) != "" {
		args = append(args, strings.Fields(arguments)...)
	}
	return runUnit(args)
}

func runSource(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: titanus source <list|import>")
	}
	manager := source.NewManager(stateRoot())
	switch args[0] {
	case "list":
		items, err := manager.List()
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println("No Titanus Sources installed.")
			return nil
		}
		for _, item := range items {
			fmt.Println(item)
		}
		return nil
	case "import":
		if len(args) != 3 {
			return fmt.Errorf("usage: titanus source import <name> <rootfs-directory>")
		}
		ansi.Info(fmt.Sprintf("Importing Source %s from %s", args[1], args[2]))
		if err := manager.ImportDirectory(args[1], args[2]); err != nil {
			return err
		}
		ansi.OK("Source imported: " + args[1])
		return nil
	default:
		return fmt.Errorf("unknown source action %q", args[0])
	}
}

func runUnit(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: titanus unit <create|start|stop|inspect|list|logs|delete>")
	}
	manager := unitManager()

	switch args[0] {
	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: titanus unit create ID --source SOURCE [options] -- COMMAND [ARGS...]")
		}
		id := args[1]
		fs := flag.NewFlagSet("unit create", flag.ContinueOnError)
		fs.SetOutput(os.Stderr)
		sourceName := fs.String("source", "", "Titanus Source name")
		hostname := fs.String("hostname", id, "Unit hostname")
		memory := fs.String("memory", "512M", "memory limit")
		cpu := fs.Int("cpu", 100, "CPU percentage, 100 = one logical CPU")
		pids := fs.Int("pids", 256, "maximum process count")
		fabricEnabled := fs.Bool("fabric", false, "attach Unit to Titanus Fabric")
		publish := fs.String("publish", "", "comma-separated HOST:UNIT[/tcp|udp] mappings")
		mountText := fs.String("mount", "", "comma-separated DISK:/path[:ro] mounts")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		command := fs.Args()
		if *sourceName == "" {
			return fmt.Errorf("--source is required")
		}
		if len(command) == 0 {
			return fmt.Errorf("Unit command is required after --")
		}
		memoryBytes, err := unitruntime.ParseBytes(*memory)
		if err != nil {
			return err
		}
		var ports []fabric.Port
		if strings.TrimSpace(*publish) != "" {
			for _, raw := range strings.Split(*publish, ",") {
				port, err := fabric.ParsePort(strings.TrimSpace(raw))
				if err != nil {
					return err
				}
				ports = append(ports, port)
			}
		}
		var mounts []disk.Mount
		if strings.TrimSpace(*mountText) != "" {
			for _, raw := range strings.Split(*mountText, ",") {
				mount, err := disk.ParseMount(strings.TrimSpace(raw))
				if err != nil {
					return err
				}
				mounts = append(mounts, mount)
			}
		}
		spec := unitruntime.Spec{
			ID:          id,
			Source:      *sourceName,
			Hostname:    *hostname,
			Command:     command,
			MemoryBytes: memoryBytes,
			CPUPercent:  *cpu,
			PidsMax:     *pids,
			Network: unitruntime.NetworkSpec{
				Fabric: *fabricEnabled || len(ports) > 0,
				Ports:  ports,
			},
			Mounts: mounts,
		}
		state, err := manager.Create(spec)
		if err != nil {
			return err
		}
		ansi.OK(fmt.Sprintf("Unit %s created (%s)", state.ID, state.Status))
		return nil

	case "start":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus unit start ID")
		}
		state, err := manager.Start(args[1])
		if err != nil {
			return err
		}
		ansi.OK(fmt.Sprintf("Unit %s is %s (host PID %d)", state.ID, state.Status, state.PID))
		return nil

	case "stop":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus unit stop ID")
		}
		state, err := manager.Stop(args[1], 5*time.Second)
		if err != nil {
			return err
		}
		ansi.OK(fmt.Sprintf("Unit %s is %s", state.ID, state.Status))
		return nil

	case "inspect":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus unit inspect ID")
		}
		spec, state, err := manager.Inspect(args[1])
		if err != nil {
			return err
		}
		payload := struct {
			Spec  unitruntime.Spec  `json:"spec"`
			State unitruntime.State `json:"state"`
		}{Spec: spec, State: state}
		data, _ := json.MarshalIndent(payload, "", "  ")
		fmt.Println(string(data))
		return nil

	case "list":
		states, err := manager.List()
		if err != nil {
			return err
		}
		if len(states) == 0 {
			fmt.Println("No Titanus Units.")
			return nil
		}
		fmt.Printf("%-24s %-12s %-10s %-16s\n", "UNIT", "STATUS", "PID", "ADDRESS")
		for _, state := range states {
			pid := "-"
			if state.PID > 0 {
				pid = strconv.Itoa(state.PID)
			}
			fmt.Printf("%-24s %-12s %-10s %-16s\n", state.ID, state.Status, pid, state.NetworkAddress)
		}
		return nil

	case "logs":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus unit logs ID")
		}
		path, err := manager.LogsPath(args[1])
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Println("No logs yet.")
				return nil
			}
			return err
		}
		fmt.Print(string(data))
		return nil

	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus unit delete ID")
		}
		if err := manager.Delete(args[1]); err != nil {
			return err
		}
		ansi.OK("Unit deleted: " + args[1])
		return nil

	default:
		return fmt.Errorf("unknown Unit action %q", args[0])
	}
}

func unitManager() *unitruntime.Manager {
	cfg := unitruntime.DefaultConfig()
	cfg.StateRoot = stateRoot()
	if value := strings.TrimSpace(os.Getenv("TITANUS_CGROUP_ROOT")); value != "" {
		cfg.CgroupRoot = value
	}
	if value := strings.TrimSpace(os.Getenv("TITANUS_INIT_BINARY")); value != "" {
		cfg.InitBinary = value
	}
	return unitruntime.NewManager(cfg)
}

func stateRoot() string {
	if value := strings.TrimSpace(os.Getenv("TITANUS_STATE_ROOT")); value != "" {
		return filepath.Clean(value)
	}
	return "/var/lib/titanus"
}

func runSetup() error {
	result, err := setup.RunDefault()
	if err != nil {
		return err
	}

	actions, err := planner.Build(result.Plan)
	if err != nil {
		return err
	}
	fmt.Println(ansi.Paint(ansi.Bold+ansi.Magenta, "Titanus execution Plan"))
	for _, action := range actions {
		fmt.Printf("  %02d  %-9s %-16s %s\n", action.Order, action.Type, action.Target, action.Description)
	}

	if result.Plan.AutoDeploy {
		fmt.Println()
		ansi.Info("Auto-deploy requested: running pre-flight before bootstrap.")
		if err := runPreflight(result.Plan); err != nil {
			return err
		}
		return runBootstrap(result.Plan)
	}
	return nil
}

func runPlan(action, path string) error {
	plan, err := model.LoadPlan(path)
	if err != nil {
		return err
	}
	switch action {
	case "validate":
		ansi.OK("Plan is valid: " + filepath.Clean(path))
		return nil
	case "show":
		data, _ := json.MarshalIndent(plan, "", "  ")
		fmt.Println(string(data))
		return nil
	default:
		return fmt.Errorf("unknown plan action %q", action)
	}
}

func runPreflightFile(path string) error {
	plan, err := model.LoadPlan(path)
	if err != nil {
		return err
	}
	return runPreflight(plan)
}

func runPreflight(plan model.RealmPlan) error {
	ansi.Info("Running Titanus pre-flight checks...")
	reports := preflight.NewRunner().Run(plan)
	for _, report := range reports {
		fmt.Println()
		fmt.Printf("%s %s (%s)\n", ansi.Paint(ansi.Bold, "Node"), report.Node, report.Address)
		for _, check := range report.Checks {
			if check.Passed {
				fmt.Printf("  %s %-24s %s\n", ansi.Paint(ansi.Green, "✔"), check.Name, check.Details)
			} else {
				fmt.Printf("  %s %-24s %s\n", ansi.Paint(ansi.Red, "✖"), check.Name, check.Details)
			}
		}
	}
	if err := preflight.Summary(reports); err != nil {
		return err
	}
	ansi.OK("All nodes passed Titanus pre-flight.")
	return nil
}

func runBootstrapFile(path string) error {
	plan, err := model.LoadPlan(path)
	if err != nil {
		return err
	}
	return runBootstrap(plan)
}

func runBootstrap(plan model.RealmPlan) error {
	ansi.Warn("Bootstrap changes remote hosts by creating Titanus system directories.")
	results, err := deploy.NewEngine().Bootstrap(plan)
	if err != nil {
		return err
	}
	for _, result := range results {
		ansi.OK(fmt.Sprintf("%s: %s", result.Node, result.Message))
	}
	ansi.OK("Realm node bootstrap completed.")
	return nil
}

func askPath(reader *bufio.Reader) (string, error) {
	return promptDefault(reader, "Plan path", "titanus-plan.json")
}

func prompt(reader *bufio.Reader, label string) (string, error) {
	for {
		fmt.Printf("%s: ", label)
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		value := strings.TrimSpace(line)
		if value != "" {
			return value, nil
		}
		ansi.Warn("A value is required.")
	}
}

func promptDefault(reader *bufio.Reader, label, defaultValue string) (string, error) {
	if defaultValue == "" {
		fmt.Printf("%s: ", label)
	} else {
		fmt.Printf("%s [%s]: ", label, defaultValue)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(line)
	if value == "" {
		return defaultValue, nil
	}
	return value, nil
}

func pause(reader *bufio.Reader) {
	fmt.Print("\nPress ENTER to continue...")
	_, _ = reader.ReadString('\n')
}

func printHelp() {
	fmt.Print(`Titanus Core CLI

Usage:
  titanus                                      Interactive ANSI menu
  titanus setup                                Guided Realm setup
  titanus realm init --name NAME --node ID --address IP
  titanus realm status
  titanus realm issue-node --node ID --address IP

  titanus source list                          List Sources
  titanus source import NAME ROOTFS            Import a rootfs directory

  titanus fabric init [--cidr CIDR] [--bridge NAME]
  titanus fabric status
  titanus fabric allocations

  titanus fleet create NAME --source SOURCE [options] -- COMMAND [ARGS...]
  titanus fleet list
  titanus fleet status NAME
  titanus fleet scale NAME INSTANCES
  titanus fleet delete NAME

  titanus route create NAME --fleet FLEET --port PORT --target-port PORT
  titanus route list
  titanus route status NAME
  titanus route delete NAME

  titanus disk create NAME --provider local|ceph-rbd|cephfs --size SIZE
  titanus disk list
  titanus disk inspect NAME
  titanus disk delete NAME [--destroy-data]
  titanus disk ceph-config [options]

  titanus unit create ID --source SOURCE [options] -- COMMAND [ARGS...]
  titanus unit start ID
  titanus unit stop ID
  titanus unit inspect ID
  titanus unit list
  titanus unit logs ID
  titanus unit delete ID

  titanus plan validate FILE                   Validate a Titanus Plan
  titanus plan show FILE                       Display a Titanus Plan
  titanus preflight FILE                       Test all configured nodes
  titanus deploy bootstrap FILE                Bootstrap validated nodes
  titanus version                              Show version

Unit create options:
  --hostname NAME      Unit hostname (default: Unit ID)
  --memory SIZE        Memory limit, e.g. 512M, 2G (default: 512M)
  --cpu PERCENT        100 = one logical CPU (default: 100)
  --pids COUNT         Maximum process count (default: 256)
  --fabric             Attach Unit to Titanus Fabric
  --publish MAPS       Comma-separated HOST:UNIT[/tcp|udp] mappings
  --mount MOUNTS       Comma-separated DISK:/path[:ro] mounts

Development overrides:
  TITANUS_STATE_ROOT
  TITANUS_CGROUP_ROOT
  TITANUS_INIT_BINARY
`)
}
