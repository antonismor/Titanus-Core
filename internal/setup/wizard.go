package setup

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/ansi"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/version"
)

type Wizard struct {
	in  *bufio.Reader
	out io.Writer
}

type Result struct {
	Plan     model.RealmPlan
	PlanPath string
}

func New(in io.Reader, out io.Writer) *Wizard {
	return &Wizard{in: bufio.NewReader(in), out: out}
}

func (w *Wizard) Run() (Result, error) {
	ansi.Clear()
	ansi.Banner()
	fmt.Fprintln(w.out, ansi.Paint(ansi.Bold+ansi.White, "Create a Titanus Realm"))
	fmt.Fprintln(w.out, ansi.Paint(ansi.Dim, "The wizard will build and validate a deployment Plan before changing any host."))
	fmt.Fprintln(w.out)
	fmt.Fprintln(w.out, "  "+ansi.Paint(ansi.Cyan, "1)")+" Single-node Titanus")
	fmt.Fprintln(w.out, "  "+ansi.Paint(ansi.Cyan, "2)")+" Multi-node Titanus Realm")
	fmt.Fprintln(w.out, "  "+ansi.Paint(ansi.Cyan, "3)")+" Titanus Realm + Ceph")
	fmt.Fprintln(w.out)

	mode, err := w.askInt("Deployment mode", 3, 1, 3)
	if err != nil {
		return Result{}, err
	}

	operation, err := w.askInt("Operation: 1 initial install, 2 upgrade, 3 binary rollback", 1, 1, 3)
	if err != nil {
		return Result{}, err
	}
	install := &model.Installation{Operation: []string{"install", "upgrade", "rollback"}[operation-1], APIPort: 9443, RaftPort: 9444, NodePrefix: 24, VXLANID: 4242, Start: true}
	plan := model.RealmPlan{
		Installation: install,
		Version:      "titanus-plan/v2",
		CreatedAt:    time.Now().UTC(),
		FabricCIDR:   "10.210.0.0/16",
		ServiceCIDR:  "10.220.0.0/16",
	}

	plan.RealmName, err = w.ask("Realm name", "TITANUS-REALM", true)
	if err != nil {
		return Result{}, err
	}
	plan.FabricCIDR, err = w.ask("Titanus Fabric CIDR", plan.FabricCIDR, true)
	if err != nil {
		return Result{}, err
	}
	plan.ServiceCIDR, err = w.ask("Titanus service CIDR", plan.ServiceCIDR, true)
	if err != nil {
		return Result{}, err
	}

	fmt.Fprintln(w.out, "Controllers use the configured HTTPS origins; register Gateway VIPs separately after exclusion policy is configured.")

	defaultNodes := 1
	if mode != 1 {
		defaultNodes = 3
	}
	nodeCount, err := w.askInt("Number of Titanus nodes", defaultNodes, 1, 256)
	if err != nil {
		return Result{}, err
	}

	controlNodes := 1
	if mode != 1 {
		defaultControl := 3
		if nodeCount < defaultControl {
			defaultControl = nodeCount
		}
		controlNodes, err = w.askInt("How many nodes should be CONTROL-capable", defaultControl, 1, nodeCount)
		if err != nil {
			return Result{}, err
		}
	}
	storageNodesWanted := 0
	if mode == 3 {
		defaultStorage := 3
		if nodeCount < defaultStorage {
			defaultStorage = nodeCount
		}
		storageNodesWanted, err = w.askInt("How many nodes should provide Ceph storage", defaultStorage, 1, nodeCount)
		if err != nil {
			return Result{}, err
		}
	}

	fmt.Fprintln(w.out)
	fmt.Fprintln(w.out, ansi.Paint(ansi.Bold+ansi.Magenta, "Node configuration"))
	fmt.Fprintln(w.out, ansi.Paint(ansi.Dim, "Capabilities: CONTROL, EXECUTION, STORAGE, GATEWAY, GPU, BACKUP"))
	fmt.Fprintln(w.out)

	for i := 0; i < nodeCount; i++ {
		fmt.Fprintln(w.out, ansi.Paint(ansi.Bold+ansi.Cyan, fmt.Sprintf("Node %d/%d", i+1, nodeCount)))
		n := model.NodeSpec{SSHPort: 22}
		n.Name, err = w.ask("  Hostname", fmt.Sprintf("titanus%02d", i+1), true)
		if err != nil {
			return Result{}, err
		}
		n.ManagementIP, err = w.ask("  Management IP", "", true)
		if err != nil {
			return Result{}, err
		}
		n.FabricIP, err = w.ask("  Fabric IP", "", true)
		if err != nil {
			return Result{}, err
		}
		n.SSHUser, err = w.ask("  SSH user", "root", true)
		if err != nil {
			return Result{}, err
		}
		n.SSHPort, err = w.askInt("  SSH port", 22, 1, 65535)
		if err != nil {
			return Result{}, err
		}

		defaultCaps := "EXECUTION"
		if mode == 1 || i < controlNodes {
			defaultCaps = "CONTROL,EXECUTION"
		}
		if mode == 3 && i < storageNodesWanted {
			defaultCaps += ",STORAGE"
		}
		capsRaw, askErr := w.ask("  Capabilities", defaultCaps, true)
		if askErr != nil {
			return Result{}, askErr
		}
		n.Capabilities, err = model.ParseCapabilities(capsRaw)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", n.Name, err)
		}

		if mode == 3 && hasCapability(n, model.CapabilityStorage) {
			n.CephPublicIP, err = w.ask("  Ceph public IP", "", true)
			if err != nil {
				return Result{}, err
			}
			n.CephClusterIP, err = w.ask("  Ceph cluster IP", "", true)
			if err != nil {
				return Result{}, err
			}
		}
		plan.Nodes = append(plan.Nodes, n)
		fmt.Fprintln(w.out)
	}

	if mode == 3 {
		plan.Ceph.Enabled = true
		fmt.Fprintln(w.out, ansi.Paint(ansi.Bold+ansi.Magenta, "Ceph storage"))
		if install.Operation == "install" {
			plan.Ceph.Provision, err = w.askBool("Provision a new Ceph cluster automatically", false)
		}
		if err != nil {
			return Result{}, err
		}
		plan.Ceph.PublicCIDR, err = w.ask("Ceph public network CIDR", "10.230.0.0/24", true)
		if err != nil {
			return Result{}, err
		}
		plan.Ceph.ClusterCIDR, err = w.ask("Ceph cluster network CIDR", "10.231.0.0/24", true)
		if err != nil {
			return Result{}, err
		}
		storageNodes := 0
		for _, n := range plan.Nodes {
			if hasCapability(n, model.CapabilityStorage) {
				storageNodes++
			}
		}
		defaultReplication := 3
		if storageNodes < defaultReplication {
			defaultReplication = storageNodes
		}
		if defaultReplication < 1 {
			defaultReplication = 1
		}
		plan.Ceph.Replication, err = w.askInt("Ceph replication copies", defaultReplication, 1, 9)
		if err != nil {
			return Result{}, err
		}
		plan.Ceph.RequestedTB, err = w.askInt("Requested usable capacity in TB", 10, 1, 1000000)
		if err != nil {
			return Result{}, err
		}
		plan.Ceph.EnableRBD, err = w.askBool("Enable RBD block storage", true)
		if err != nil {
			return Result{}, err
		}
		plan.Ceph.EnableCephFS, err = w.askBool("Enable CephFS shared storage", true)
		if err != nil {
			return Result{}, err
		}
		plan.Ceph.EnableRGW, err = w.askBool("Enable RGW object storage", false)
		if err != nil {
			return Result{}, err
		}

		if plan.Ceph.Provision {
			fmt.Fprintln(w.out)
			fmt.Fprintln(w.out, ansi.Paint(ansi.Bold+ansi.Yellow, "Ceph OSD device selection"))
			fmt.Fprintln(w.out, ansi.Paint(ansi.Yellow, "Only devices explicitly selected here may be erased by Titanus."))
			for i := range plan.Nodes {
				if !hasCapability(plan.Nodes[i], model.CapabilityStorage) {
					continue
				}
				node := &plan.Nodes[i]
				fmt.Fprintln(w.out)
				fmt.Fprintln(w.out, ansi.Paint(ansi.Bold+ansi.Cyan, node.Name+" ("+node.ManagementIP+")"))

				raw, askErr := w.ask("  Ceph data devices (comma separated, e.g. /dev/sdb,/dev/sdc)", "", true)
				if askErr != nil {
					return Result{}, askErr
				}
				for _, value := range strings.Split(raw, ",") {
					device := strings.TrimSpace(value)
					if device == "" {
						continue
					}
					node.CephDevices = append(node.CephDevices, device)
				}
			}

			confirmed, confirmErr := w.askBool("I understand the selected Ceph devices WILL BE ERASED during provisioning", false)
			if confirmErr != nil {
				return Result{}, confirmErr
			}
			if !confirmed {
				return Result{}, fmt.Errorf("Ceph provisioning cancelled because destructive device use was not confirmed")
			}
		}
	}

	install.ReleaseVersion, err = w.ask("Target release version", version.Version, true)
	if err != nil {
		return Result{}, err
	}
	revision := version.Revision
	if revision == "unknown" {
		revision = ""
	}
	install.Revision, err = w.ask("Exact tested main SHA for target release", revision, true)
	if err != nil {
		return Result{}, err
	}
	if install.Operation != "rollback" {
		install.Archives = map[string]model.Archive{}
		for _, arch := range []string{"amd64", "arm64"} {
			file, e := w.ask(arch+" downloaded native archive", "", true)
			if e != nil {
				return Result{}, e
			}
			sum, e := w.ask(arch+" published archive SHA256", "", true)
			if e != nil {
				return Result{}, e
			}
			install.Archives[arch] = model.Archive{Path: file, SHA256: sum}
		}
	}
	install.PKIDir, err = w.ask("Persistent private Realm PKI directory", ".titanus-pki/"+plan.RealmName, true)
	if err != nil {
		return Result{}, err
	}
	install.APIPort, err = w.askInt("mTLS API port", 9443, 1, 65535)
	if err != nil {
		return Result{}, err
	}
	install.RaftPort, err = w.askInt("mTLS Raft port", install.APIPort+1, 1, 65535)
	if err != nil {
		return Result{}, err
	}
	install.NodePrefix, err = w.askInt("Per-node Unit subnet prefix", 24, 16, 30)
	if err != nil {
		return Result{}, err
	}
	install.VXLANID, err = w.askInt("Fabric VXLAN ID", 4242, 1, 16777215)
	if err != nil {
		return Result{}, err
	}
	if install.Operation == "install" {
		install.Start, err = w.askBool("Start services when the prepared installation is applied", true)
		if err != nil {
			return Result{}, err
		}
	}
	plan.AutoDeploy, err = w.askBool("Apply the verified deployment to hosts after saving and preparing the Plan", false)
	if err != nil {
		return Result{}, err
	}
	plan.Normalize()
	if err := plan.Validate(); err != nil {
		return Result{}, fmt.Errorf("plan validation failed: %w", err)
	}

	path, err := w.ask("Plan output file", "titanus-plan.json", true)
	if err != nil {
		return Result{}, err
	}
	if err := plan.Save(path); err != nil {
		return Result{}, err
	}

	w.printSummary(plan, path)
	return Result{Plan: plan, PlanPath: path}, nil
}

func (w *Wizard) printSummary(plan model.RealmPlan, path string) {
	fmt.Fprintln(w.out)
	fmt.Fprintln(w.out, ansi.Paint(ansi.Bold+ansi.Green, "Plan validated successfully"))
	fmt.Fprintf(w.out, "  Realm:          %s\n", plan.RealmName)
	fmt.Fprintf(w.out, "  Nodes:          %d\n", len(plan.Nodes))
	fmt.Fprintf(w.out, "  Control nodes:  %d\n", len(plan.ControlNodes()))
	fmt.Fprintf(w.out, "  Storage nodes:  %d\n", len(plan.StorageNodes()))
	fmt.Fprintf(w.out, "  Fabric:         %s\n", plan.FabricCIDR)
	fmt.Fprintf(w.out, "  Services:       %s\n", plan.ServiceCIDR)
	if plan.Ceph.Enabled {
		fmt.Fprintf(w.out, "  Ceph:           enabled, replication=%d, requested=%d TB usable\n", plan.Ceph.Replication, plan.Ceph.RequestedTB)
	}
	fmt.Fprintf(w.out, "  Saved Plan:     %s\n", path)
	fmt.Fprintln(w.out)
}

func (w *Wizard) ask(label, def string, required bool) (string, error) {
	for {
		if def == "" {
			fmt.Fprintf(w.out, "%s: ", ansi.Paint(ansi.Cyan, label))
		} else {
			fmt.Fprintf(w.out, "%s [%s]: ", ansi.Paint(ansi.Cyan, label), def)
		}
		line, err := w.in.ReadString('\n')
		if err != nil && len(line) == 0 {
			return "", err
		}
		value := strings.TrimSpace(line)
		if value == "" {
			value = def
		}
		if value == "" && required {
			fmt.Fprintln(w.out, ansi.Paint(ansi.Yellow, "A value is required."))
			continue
		}
		return value, nil
	}
}

func (w *Wizard) askInt(label string, def, min, max int) (int, error) {
	for {
		raw, err := w.ask(label, strconv.Itoa(def), true)
		if err != nil {
			return 0, err
		}
		value, err := strconv.Atoi(raw)
		if err == nil && value >= min && value <= max {
			return value, nil
		}
		fmt.Fprintf(w.out, "%s\n", ansi.Paint(ansi.Yellow, fmt.Sprintf("Enter a number between %d and %d.", min, max)))
	}
}

func (w *Wizard) askBool(label string, def bool) (bool, error) {
	defText := "y"
	if !def {
		defText = "n"
	}
	for {
		raw, err := w.ask(label+" (y/n)", defText, true)
		if err != nil {
			return false, err
		}
		switch strings.ToLower(raw) {
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(w.out, ansi.Paint(ansi.Yellow, "Please answer y or n."))
		}
	}
}

func humanBytes(value uint64) string {
	const (
		kiB = 1024
		miB = 1024 * kiB
		giB = 1024 * miB
		tiB = 1024 * giB
	)
	switch {
	case value >= tiB:
		return fmt.Sprintf("%.2f TiB", float64(value)/float64(tiB))
	case value >= giB:
		return fmt.Sprintf("%.2f GiB", float64(value)/float64(giB))
	case value >= miB:
		return fmt.Sprintf("%.2f MiB", float64(value)/float64(miB))
	default:
		return fmt.Sprintf("%d B", value)
	}
}

func hasCapability(n model.NodeSpec, wanted model.Capability) bool {
	for _, c := range n.Capabilities {
		if c == wanted {
			return true
		}
	}
	return false
}

func RunDefault() (Result, error) {
	return New(os.Stdin, os.Stdout).Run()
}
