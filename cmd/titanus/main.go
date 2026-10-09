package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	buildversion "github.com/antonismor/Titanus-Core/internal/version"
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
	"github.com/antonismor/Titanus-Core/internal/offline"
	"github.com/antonismor/Titanus-Core/internal/planner"
	"github.com/antonismor/Titanus-Core/internal/preflight"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/security"
	"github.com/antonismor/Titanus-Core/internal/setup"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
)

func main() {
	if len(os.Args) < 2 || (os.Args[1] != "--capabilities-json" && os.Args[1] != "backup" && os.Args[1] != "version" && os.Args[1] != "--version" && os.Args[1] != "-v" && os.Args[1] != "help" && os.Args[1] != "--help" && os.Args[1] != "-h") {
		lock, err := offline.Shared()
		if err != nil {
			ansi.Error(err.Error())
			os.Exit(1)
		}
		if lock != nil {
			defer lock.Close()
		}
		root := os.Getenv("TITANUS_STATE_ROOT")
		if root == "" {
			root = "/var/lib/titanus"
		}
		if err := offline.CheckStartup(filepath.Clean(root)); err != nil {
			ansi.Error(err.Error())
			os.Exit(1)
		}
	}
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
	case "schema":
		return runSchema(args[1:])
	case "--capabilities-json":
		buildversion.PrintCapabilities()
		return nil
	case "backup":
		return runBackup(args[1:])
	case "task", "autoscale", "secret":
		return runOrchestration(args[0], args[1:])
	case "setup":
		return runSetup()
	case "identity":
		return runIdentity(args[1:])
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
	case "policy":
		return runPolicy(args[1:])
	case "disk":
		return runDisk(args[1:])
	case "unit":
		return runUnit(args[1:])
	case "diagnostics", "metrics", "events":
		if len(args) != 1 {
			return fmt.Errorf("usage: titanus %s", args[0])
		}
		data, err := localclient.New(os.Getenv("TITANUS_SOCKET")).Observe(args[0])
		if err != nil {
			return err
		}
		fmt.Print(string(data))
		return nil
	case "version", "--version", "-v":
		buildversion.Print(len(args) > 1 && args[1] == "--json")
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
		if len(args) >= 2 && args[1] == "release" {
			return runReleaseDeploy(args[2:])
		}
		if len(args) >= 2 && args[1] == "bootstrap" {
			if len(args) != 3 {
				return fmt.Errorf("usage: titanus deploy bootstrap <plan.json>")
			}
			return runBootstrapFile(args[2])
		}
		if len(args) >= 2 && args[1] == "realm" {
			return runRealmDeploy(args[2:])
		}
		return fmt.Errorf("usage: titanus deploy <bootstrap|realm> ...")
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
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "6)") + " Manage Fabric")
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "7)") + " Manage Disks")
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "8)") + " Realm status / identity")
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "9)") + " Manage Fleets")
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "10)") + " Manage Routes")
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "11)") + " Manage Network Policies")
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "12)") + " Manage Units")
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "13)") + " Show version")
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
			if err := policyMenu(reader); err != nil {
				ansi.Error(err.Error())
				pause(reader)
			}
		case "12":
			if err := unitMenu(reader); err != nil {
				ansi.Error(err.Error())
				pause(reader)
			}
		case "13":
			buildversion.Print(false)
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
		fmt.Println("  4) Show Service Fabric endpoints")
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
		case "4":
			if err := runFabric([]string{"services"}); err != nil {
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
		return fmt.Errorf("usage: titanus fabric <init|status|allocations|services>")
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
		fmt.Printf("Bridge:      %s\nCIDR:        %s\nGateway:     %s\nServiceCIDR: %s\n", cfg.Bridge, cfg.CIDR, cfg.Gateway, cfg.ServiceCIDR)
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
	case "services":
		items, err := manager.Services()
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println("No Fabric Services.")
			return nil
		}
		fmt.Printf("%-20s %-22s %-10s %-8s\n", "SERVICE", "ENDPOINT", "BACKENDS", "PROTOCOL")
		for _, item := range items {
			fmt.Printf("%-20s %-22s %-10d %-8s\n",
				item.Name, net.JoinHostPort(item.Address, strconv.Itoa(item.Port)), len(item.Backends), item.Protocol)
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
		return fmt.Errorf("usage: titanus disk <create|list|inspect|delete|ceph-config|snapshot|snapshots|restore|owner|detach|fence>")
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

	case "snapshot":
		if len(args) != 3 {
			return fmt.Errorf("usage: titanus disk snapshot DISK SNAPSHOT")
		}
		snapshot, err := manager.Snapshot(args[1], args[2])
		if err != nil {
			return err
		}
		return printDiskJSON(snapshot)
	case "snapshots":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus disk snapshots DISK")
		}
		items, err := manager.Snapshots(args[1])
		if err != nil {
			return err
		}
		return printDiskJSON(items)
	case "restore":
		if len(args) != 4 {
			return fmt.Errorf("usage: titanus disk restore DISK SNAPSHOT NEW_DISK")
		}
		spec, err := manager.Restore(args[1], args[2], args[3])
		if err != nil {
			return err
		}
		return printDiskJSON(spec)
	case "owner":
		if len(args) != 4 {
			return fmt.Errorf("usage: titanus disk owner DISK HOST_UID HOST_GID (empty, offline Disk only)")
		}
		uid, err := strconv.Atoi(args[2])
		if err != nil {
			return err
		}
		gid, err := strconv.Atoi(args[3])
		if err != nil {
			return err
		}
		return manager.ProvisionOwnership(args[1], uid, gid)
	case "detach":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus disk detach DISK")
		}
		return manager.Detach(args[1])
	case "fence":
		if len(args) != 4 {
			return fmt.Errorf("usage: titanus disk fence DISK CEPHFS_SESSION OBSERVED_ADDRESS")
		}
		session, err := strconv.ParseUint(args[2], 10, 64)
		if err != nil {
			return err
		}
		return manager.FenceCephFS(args[1], session, args[3])
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
		cert, key, err := auth.Issue(*nodeID, []string{*address}, identity.RoleController, 24*time.Hour)
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
			State: realm.NodeReady, Labels: map[string]string{}, Compatibility: func() *buildversion.Capabilities { c := buildversion.Compatible(); return &c }(),
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

	case "consensus":
		result, err := localclient.New("/run/titanus/titanus.sock").ConsensusStatus()
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(data))
		return nil
	case "status":
		if _, e := os.Stat(filepath.Join(stateRoot(), "realm", "consensus", "membership.json")); e == nil {
			result, err := localclient.New("/run/titanus/titanus.sock").RealmState()
			if err != nil {
				return err
			}
			data, _ := json.MarshalIndent(result, "", "  ")
			fmt.Println(string(data))
			return nil
		}
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
		role := fs.String("role", "node", "node or controller certificate role")
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
		cert, key, err := auth.Issue(*nodeID, []string{*address}, identity.Role(*role), 24*time.Hour)
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
		return fmt.Errorf("usage: titanus fleet <create|apply|rollback|list|status|scale|delete>")
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
		maxSurge := fs.Int("max-surge", 1, "maximum extra Units during rollout")
		minAvailable := fs.Int("minimum", 1, "minimum desired availability")
		memory := fs.String("memory", "512M", "memory per Unit")
		cpu := fs.Int("cpu", 100, "CPU percentage per Unit")
		pids := fs.Int("pids", 256, "maximum processes per Unit")
		fabricEnabled := fs.Bool("fabric", false, "attach Units to Titanus Fabric")
		publish := fs.String("publish", "", "comma-separated HOST:UNIT[/tcp|udp]")
		mountText := fs.String("mount", "", "comma-separated DISK:/path[:ro]")
		capabilities := fs.String("capabilities", "", "comma-separated application capabilities (default: none)")
		spread := fs.String("spread", "", "label key used to spread replicas")
		profile := fs.String("security", security.ProfileRestricted, "Unit Security Profile (restricted)")
		uid := fs.Int("uid", 0, "Unit process UID")
		gid := fs.Int("gid", 0, "Unit process GID")
		readOnly := fs.Bool("read-only-rootfs", false, "remount Unit root filesystem read-only")
		healthFile := fs.String("health-config", "", "JSON health configuration file")
		readiness := fs.String("readiness", "", "HTTP/TCP readiness URL inside the Unit")
		liveness := fs.String("liveness", "", "HTTP/TCP liveness URL inside the Unit")
		restart := fs.String("restart", "always", "never, on-failure or always")
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
			MaxSurge: *maxSurge, Name: name, Instances: *instances, MinimumAvailable: *minAvailable,
			RequiredLabels: labels, SpreadLabel: strings.TrimSpace(*spread),
			Template: realm.UnitTemplate{
				Source: *sourceName, Command: fs.Args(),
				MemoryBytes: memBytes, CPUPercent: *cpu, PidsMax: *pids,
				Fabric: *fabricEnabled || len(ports) > 0,
				Ports:  ports, Mounts: mounts,
			},
		}
		fleet.Template.Health, err = healthFlags(*readiness, *liveness, *restart, *healthFile)
		if err != nil {
			return err
		}
		fleet.Template.Security = security.Policy{Profile: *profile, RunAsUID: *uid, RunAsGID: *gid, ReadOnlyRootFS: *readOnly}
		if strings.TrimSpace(*capabilities) != "" {
			fleet.Template.Security.Capabilities = strings.Split(*capabilities, ",")
		}
		fleet.Template.Security.Normalize()
		if err := fleet.Template.Security.Validate(); err != nil {
			return err
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

	case "apply":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus fleet apply FLEET.json")
		}
		data, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		var fleet realm.Fleet
		if err := json.Unmarshal(data, &fleet); err != nil {
			return err
		}
		result, err := client.CreateFleet(fleet)
		if err != nil {
			return err
		}
		data, _ = json.MarshalIndent(result, "", "  ")
		fmt.Println(string(data))
		return nil
	case "rollback":
		if len(args) < 2 || len(args) > 3 {
			return fmt.Errorf("usage: titanus fleet rollback NAME [GENERATION]")
		}
		var generation uint64
		if len(args) == 3 {
			var err error
			generation, err = strconv.ParseUint(args[2], 10, 64)
			if err != nil {
				return err
			}
		}
		fleet, err := client.RollbackFleet(args[1], generation)
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(fleet, "", "  ")
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

func policyMenu(reader *bufio.Reader) error {
	for {
		ansi.Clear()
		ansi.Banner()
		fmt.Println(ansi.Paint(ansi.Bold+ansi.White, "Titanus Network Policies"))
		fmt.Println()
		fmt.Println("  1) List Policies")
		fmt.Println("  2) Create Policy")
		fmt.Println("  3) Inspect Policy")
		fmt.Println("  4) Delete Policy")
		fmt.Println("  0) Back")
		fmt.Print("\nSelect: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		switch strings.TrimSpace(line) {
		case "1":
			if err := runPolicy([]string{"list"}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "2":
			name, err := prompt(reader, "Policy name")
			if err != nil {
				return err
			}
			fleet, err := prompt(reader, "Protected Fleet")
			if err != nil {
				return err
			}
			direction, err := promptDefault(reader, "Direction (ingress/egress)", "ingress")
			if err != nil {
				return err
			}
			direction = strings.ToLower(strings.TrimSpace(direction))
			command := []string{"create", name, "--fleet", fleet, "--direction", direction}
			switch direction {
			case "ingress":
				fromFleet, err := promptDefault(reader, "Allow source Fleet (blank for none)", "")
				if err != nil {
					return err
				}
				fromCIDR, err := promptDefault(reader, "Allow source CIDR (blank for none)", "")
				if err != nil {
					return err
				}
				if strings.TrimSpace(fromFleet) != "" {
					command = append(command, "--from-fleet", fromFleet)
				}
				if strings.TrimSpace(fromCIDR) != "" {
					command = append(command, "--from-cidr", fromCIDR)
				}
				if strings.TrimSpace(fromFleet) == "" && strings.TrimSpace(fromCIDR) == "" {
					command = append(command, "--allow-any")
				}
			case "egress":
				toFleet, err := promptDefault(reader, "Allow destination Fleet (blank for none)", "")
				if err != nil {
					return err
				}
				toCIDR, err := promptDefault(reader, "Allow destination CIDR (blank for none)", "")
				if err != nil {
					return err
				}
				if strings.TrimSpace(toFleet) != "" {
					command = append(command, "--to-fleet", toFleet)
				}
				if strings.TrimSpace(toCIDR) != "" {
					command = append(command, "--to-cidr", toCIDR)
				}
				if strings.TrimSpace(toFleet) == "" && strings.TrimSpace(toCIDR) == "" {
					command = append(command, "--allow-any")
				}
			default:
				ansi.Error("Direction must be ingress or egress")
				pause(reader)
				continue
			}
			protocol, err := promptDefault(reader, "Protocol (tcp/udp/any)", "tcp")
			if err != nil {
				return err
			}
			ports, err := promptDefault(reader, "Ports, comma-separated (blank = all for protocol)", "")
			if err != nil {
				return err
			}
			command = append(command, "--protocol", protocol)
			if strings.TrimSpace(ports) != "" {
				command = append(command, "--ports", ports)
			}
			if err := runPolicy(command); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "3":
			name, err := prompt(reader, "Policy name")
			if err != nil {
				return err
			}
			if err := runPolicy([]string{"status", name}); err != nil {
				ansi.Error(err.Error())
			}
			pause(reader)
		case "4":
			name, err := prompt(reader, "Policy name")
			if err != nil {
				return err
			}
			if err := runPolicy([]string{"delete", name}); err != nil {
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

func runPolicy(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: titanus policy <create|list|status|delete>")
	}
	client := localclient.New("/run/titanus/titanus.sock")
	switch args[0] {
	case "list":
		policies, err := client.ListPolicies()
		if err != nil {
			return err
		}
		if len(policies) == 0 {
			fmt.Println("No Titanus Network Policies.")
			return nil
		}
		fmt.Printf("%-24s %-20s %-8s %-8s %-12s %-12s\n", "POLICY", "FLEET", "INGRESS", "EGRESS", "INGRESS-DEF", "EGRESS-DEF")
		for _, policy := range policies {
			ingressDefault := "ALLOW"
			if policy.DefaultDeny {
				ingressDefault = "DENY"
			}
			egressDefault := "ALLOW"
			if policy.DefaultDenyEgress {
				egressDefault = "DENY"
			}
			fmt.Printf("%-24s %-20s %-8d %-8d %-12s %-12s\n",
				policy.Name, policy.Fleet, len(policy.Ingress), len(policy.Egress), ingressDefault, egressDefault)
		}
		return nil
	case "create":
		if len(args) < 2 {
			return fmt.Errorf("usage: titanus policy create NAME --fleet FLEET --direction ingress|egress [selectors] [--protocol tcp|udp|any] [--ports LIST]")
		}
		name := args[1]
		fs := flag.NewFlagSet("policy create", flag.ContinueOnError)
		fleetName := fs.String("fleet", "", "protected Fleet")
		direction := fs.String("direction", "ingress", "policy direction: ingress or egress")
		fromFleet := fs.String("from-fleet", "", "allowed source Fleet for ingress")
		fromCIDR := fs.String("from-cidr", "", "allowed source IPv4 CIDR for ingress")
		toFleet := fs.String("to-fleet", "", "allowed destination Fleet for egress")
		toCIDR := fs.String("to-cidr", "", "allowed destination IPv4 CIDR for egress")
		allowAny := fs.Bool("allow-any", false, "allow any source/destination subject to protocol and ports")
		protocol := fs.String("protocol", "tcp", "tcp, udp or any")
		portsText := fs.String("ports", "", "comma-separated destination ports; blank means all ports for protocol")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if strings.TrimSpace(*fleetName) == "" {
			return fmt.Errorf("--fleet is required")
		}
		ports := make([]int, 0)
		if strings.TrimSpace(*portsText) != "" {
			for _, raw := range strings.Split(*portsText, ",") {
				port, err := strconv.Atoi(strings.TrimSpace(raw))
				if err != nil {
					return fmt.Errorf("invalid policy port %q", raw)
				}
				ports = append(ports, port)
			}
		}
		policy := realm.NetworkPolicy{Name: name, Fleet: strings.TrimSpace(*fleetName)}
		switch strings.ToLower(strings.TrimSpace(*direction)) {
		case "ingress":
			if strings.TrimSpace(*toFleet) != "" || strings.TrimSpace(*toCIDR) != "" {
				return fmt.Errorf("--to-fleet/--to-cidr are only valid for egress policies")
			}
			if *allowAny && (strings.TrimSpace(*fromFleet) != "" || strings.TrimSpace(*fromCIDR) != "") {
				return fmt.Errorf("--allow-any cannot be combined with --from-fleet or --from-cidr")
			}
			if !*allowAny && strings.TrimSpace(*fromFleet) == "" && strings.TrimSpace(*fromCIDR) == "" {
				return fmt.Errorf("ingress requires --from-fleet, --from-cidr or --allow-any")
			}
			rule := realm.NetworkPolicyRule{
				FromFleet: strings.TrimSpace(*fromFleet),
				FromCIDR:  strings.TrimSpace(*fromCIDR),
				Protocol:  strings.TrimSpace(*protocol),
				Ports:     ports,
			}
			if *allowAny {
				rule.FromFleet = ""
				rule.FromCIDR = ""
			}
			policy.DefaultDeny = true
			policy.Ingress = []realm.NetworkPolicyRule{rule}
		case "egress":
			if strings.TrimSpace(*fromFleet) != "" || strings.TrimSpace(*fromCIDR) != "" {
				return fmt.Errorf("--from-fleet/--from-cidr are only valid for ingress policies")
			}
			if *allowAny && (strings.TrimSpace(*toFleet) != "" || strings.TrimSpace(*toCIDR) != "") {
				return fmt.Errorf("--allow-any cannot be combined with --to-fleet or --to-cidr")
			}
			if !*allowAny && strings.TrimSpace(*toFleet) == "" && strings.TrimSpace(*toCIDR) == "" {
				return fmt.Errorf("egress requires --to-fleet, --to-cidr or --allow-any")
			}
			rule := realm.NetworkPolicyEgressRule{
				ToFleet:  strings.TrimSpace(*toFleet),
				ToCIDR:   strings.TrimSpace(*toCIDR),
				Protocol: strings.TrimSpace(*protocol),
				Ports:    ports,
			}
			if *allowAny {
				rule.ToFleet = ""
				rule.ToCIDR = ""
			}
			policy.DefaultDenyEgress = true
			policy.Egress = []realm.NetworkPolicyEgressRule{rule}
		default:
			return fmt.Errorf("--direction must be ingress or egress")
		}
		stored, err := client.CreatePolicy(policy)
		if err != nil {
			return err
		}
		ansi.OK(fmt.Sprintf("Network Policy %s protects Fleet %s", stored.Name, stored.Fleet))
		return nil
	case "status":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus policy status NAME")
		}
		policy, err := client.PolicyStatus(args[1])
		if err != nil {
			return err
		}
		data, _ := json.MarshalIndent(policy, "", "  ")
		fmt.Println(string(data))
		return nil
	case "delete":
		if len(args) != 2 {
			return fmt.Errorf("usage: titanus policy delete NAME")
		}
		if err := client.DeletePolicy(args[1]); err != nil {
			return err
		}
		ansi.OK("Network Policy deleted: " + args[1])
		return nil
	default:
		return fmt.Errorf("unknown Network Policy action %q", args[0])
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
		fmt.Printf("%-20s %-20s %-22s %-20s %-12s\n", "ROUTE", "FLEET", "SERVICE", "LISTEN", "TARGET")
		for _, route := range routes {
			fmt.Printf("%-20s %-20s %-22s %-20s %-12s\n",
				route.Name, route.Fleet,
				net.JoinHostPort(route.ServiceIP, strconv.Itoa(route.ListenPort)),
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
		listenIP := fs.String("listen", "0.0.0.0", "gateway listen IP")
		serviceIP := fs.String("service-ip", "", "optional stable Service CIDR IP; auto-allocated by default")
		listenPort := fs.Int("port", 0, "service/gateway listen port")
		targetPort := fs.Int("target-port", 0, "target Unit port")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		stored, err := client.CreateRoute(realm.Route{
			Name: name, Fleet: *fleetName, ServiceIP: *serviceIP, ListenIP: *listenIP,
			ListenPort: *listenPort, TargetPort: *targetPort, Protocol: "tcp",
		})
		if err != nil {
			return err
		}
		ansi.OK(fmt.Sprintf("Route %s: service=%s:%d gateway=%s:%d -> Fleet %s:%d",
			stored.Name, stored.ServiceIP, stored.ListenPort,
			stored.ListenIP, stored.ListenPort, stored.Fleet, stored.TargetPort))
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
		capabilities := fs.String("capabilities", "", "comma-separated application capabilities (default: none)")
		profile := fs.String("security", security.ProfileRestricted, "Security Profile (restricted)")
		uid := fs.Int("uid", 0, "process UID inside the Unit")
		gid := fs.Int("gid", 0, "process GID inside the Unit")
		readOnly := fs.Bool("read-only-rootfs", false, "remount Unit root filesystem read-only")
		healthFile := fs.String("health-config", "", "JSON health configuration file")
		readiness := fs.String("readiness", "", "HTTP/TCP readiness URL inside the Unit")
		liveness := fs.String("liveness", "", "HTTP/TCP liveness URL inside the Unit")
		restart := fs.String("restart", "never", "never, on-failure or always")
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
		spec.Security = security.Policy{Profile: *profile, RunAsUID: *uid, RunAsGID: *gid, ReadOnlyRootFS: *readOnly}
		if strings.TrimSpace(*capabilities) != "" {
			spec.Security.Capabilities = strings.Split(*capabilities, ",")
		}
		spec.Health, err = healthFlags(*readiness, *liveness, *restart, *healthFile)
		if err != nil {
			return err
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
		data, err := manager.ReadLogs(args[1])
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

func runRealmDeploy(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: titanus deploy realm PLAN [--bin-dir DIR] [--pki-dir DIR] [--no-start]")
	}
	planPath := args[0]
	fs := flag.NewFlagSet("deploy realm", flag.ContinueOnError)
	binDir := fs.String("bin-dir", "", "directory containing Titanus binaries")
	pkiDir := fs.String("pki-dir", "", "local persistent Realm PKI directory")
	nodePrefix := fs.Int("node-prefix", 24, "per-Node Unit subnet prefix")
	vxlanID := fs.Int("vxlan-id", 4242, "Titanus Realm VXLAN ID")
	clusterPort := fs.Int("cluster-port", 9443, "mTLS Realm API port")
	raftPort := fs.Int("raft-port", 9444, "mTLS Raft port for 3/5 CONTROL nodes")
	noStart := fs.Bool("no-start", false, "install but do not start services")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	plan, err := model.LoadPlan(planPath)
	if err != nil {
		return err
	}
	if *binDir == "" {
		*binDir, err = discoverBinDir()
		if err != nil {
			return err
		}
	}
	return deployRealm(plan, deploy.RealmDeployOptions{
		BinDir: *binDir, PKIDir: *pkiDir,
		NodePrefix: *nodePrefix, VXLANID: *vxlanID,
		ClusterPort: *clusterPort, RaftPort: *raftPort, StartServices: !*noStart,
	})
}

func deployRealm(plan model.RealmPlan, options deploy.RealmDeployOptions) error {
	ansi.Info("Deploying Titanus Realm " + plan.RealmName)
	results, err := deploy.NewRealmDeployer().Deploy(plan, options)
	for _, result := range results {
		if result.Message == "installed" {
			ansi.OK(fmt.Sprintf("%s (%s) %s", result.Node, result.Address, result.Role))
		}
	}
	if err != nil {
		return err
	}
	ansi.OK("Titanus Realm deployment completed")
	return nil
}

func discoverBinDir() (string, error) {
	candidates := []string{"./bin"}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(executable))
	}
	for _, candidate := range candidates {
		required := []string{"titanus", "titanusd", "titanus-agent", "titanus-init"}
		ok := true
		for _, name := range required {
			info, err := os.Stat(filepath.Join(candidate, name))
			if err != nil || !info.Mode().IsRegular() {
				ok = false
				break
			}
		}
		if ok {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("cannot locate all Titanus binaries; build with 'make build' or use 'titanus deploy realm PLAN --bin-dir DIR'")
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

	prepared, err := deploy.PrepareRelease(result.Plan, result.PlanPath+".deployment")
	if err != nil {
		return err
	}
	fmt.Println("Prepared private deployment:", result.PlanPath+".deployment")
	if result.Plan.AutoDeploy {
		_, err = deploy.NewRealmDeployer().ApplyPrepared(result.Plan, prepared)
		return err
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
  titanus backup <keygen|create|verify|attest-fence|restore> [flags]
  titanus realm init --name NAME --node ID --address IP
  titanus realm status
  titanus realm issue-node --node ID --address IP

  titanus source list                          List Sources
  titanus source import NAME ROOTFS            Import a rootfs directory

  titanus fabric init [--cidr CIDR] [--bridge NAME]
  titanus fabric status
  titanus fabric allocations
  titanus fabric services

  titanus fleet create NAME --source SOURCE [options] -- COMMAND [ARGS...]
  titanus fleet list
  titanus fleet status NAME
  titanus fleet scale NAME INSTANCES
  titanus fleet delete NAME

  titanus task submit task.json
  titanus task list|status NAME|cancel NAME
  titanus autoscale apply policy.json
  titanus autoscale list|delete FLEET
  titanus secret keygen NEW_PRIVATE_FILE
  titanus secret put NAME < PRIVATE_INPUT_FILE
  titanus secret list|delete NAME

  titanus route create NAME --fleet FLEET --port PORT --target-port PORT
  titanus route list
  titanus route status NAME
  titanus route delete NAME

  titanus policy create NAME --fleet FLEET --direction ingress|egress [selectors]
  titanus policy list
  titanus policy status NAME
  titanus policy delete NAME

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
  titanus deploy release FILE [--output DIR] [--apply] Verified versioned preparation/application
  titanus deploy bootstrap FILE                Bootstrap validated nodes
  titanus deploy realm FILE [options]          Install and start a complete Realm
  titanus schema status                       Show active schema and committed migration
  titanus schema migrate ID REVISION           Prepare all hosts and commit schema 0 to 1
  titanus version                              Show version

Unit create options:
  --hostname NAME      Unit hostname (default: Unit ID)
  --memory SIZE        Memory limit, e.g. 512M, 2G (default: 512M)
  --cpu PERCENT        100 = one logical CPU (default: 100)
  --pids COUNT         Maximum process count (default: 256)
  --fabric             Attach Unit to Titanus Fabric
  --publish MAPS       Comma-separated HOST:UNIT[/tcp|udp] mappings
  --mount MOUNTS       Comma-separated DISK:/path[:ro] mounts
  --security PROFILE   restricted (default)
  --capabilities CAPS  Explicit application capabilities (default: none)
  --uid UID            Numeric workload UID (default: 0)
  --gid GID            Numeric workload GID (default: 0)
  --read-only-rootfs   Remount Unit root filesystem read-only

Development overrides:
  TITANUS_STATE_ROOT
  TITANUS_CGROUP_ROOT
  TITANUS_INIT_BINARY
`)
}

func healthFlags(readiness, liveness, restart, file string) (unitruntime.Health, error) {
	ready, err := unitruntime.ParseProbe(readiness)
	if err != nil {
		return unitruntime.Health{}, err
	}
	live, err := unitruntime.ParseProbe(liveness)
	if err != nil {
		return unitruntime.Health{}, err
	}
	health := unitruntime.Health{Readiness: ready, Liveness: live, Restart: restart}
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return health, err
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&health); err != nil {
			return health, err
		}
	}
	health.Normalize("never")
	return health, health.Validate()
}

func printDiskJSON(value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
}

func runReleaseDeploy(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: titanus deploy release PLAN [--output DIR] [--apply]")
	}
	fs := flag.NewFlagSet("deploy release", flag.ContinueOnError)
	output := fs.String("output", args[0]+".deployment", "new private preparation directory")
	apply := fs.Bool("apply", false, "explicitly apply to configured SSH hosts")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	plan, err := model.LoadPlan(args[0])
	if err != nil {
		return err
	}
	prepared, err := deploy.PrepareRelease(plan, *output)
	if err != nil {
		return err
	}
	fmt.Println("Prepared verified deployment:", *output)
	if *apply {
		results, e := deploy.NewRealmDeployer().ApplyPrepared(plan, prepared)
		for _, r := range results {
			fmt.Printf("%s %s %s\n", r.Node, r.Role, r.Message)
		}
		return e
	}
	return nil
}

func runSchema(args []string) error {
	c := localclient.New(os.Getenv("TITANUS_SOCKET"))
	var out any
	if len(args) == 1 && args[0] == "status" {
		state, e := c.RealmState()
		if e != nil {
			return e
		}
		out = map[string]any{"schema": state.SchemaVersion, "revision": state.Revision, "migrations": state.SchemaMigrations}
	} else if len(args) == 3 && args[0] == "migrate" {
		rev, e := strconv.ParseUint(args[2], 10, 64)
		if e != nil {
			return e
		}
		m, e := c.TransitionSchema(args[1], rev)
		if e != nil {
			return e
		}
		out = m
	} else {
		return fmt.Errorf("usage: titanus schema status | migrate MIGRATION_ID EXPECTED_REVISION")
	}
	b, e := json.MarshalIndent(out, "", "  ")
	if e != nil {
		return e
	}
	fmt.Println(string(b))
	return nil
}
