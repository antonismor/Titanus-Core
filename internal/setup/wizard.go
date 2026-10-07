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

	plan := model.RealmPlan{
		Version:     "titanus-plan/v1",
		CreatedAt:   time.Now().UTC(),
		FabricCIDR:  "10.210.0.0/16",
		ServiceCIDR: "10.220.0.0/16",
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

	if mode != 1 {
		plan.ControlVIP, err = w.ask("Control virtual IP (optional)", "", false)
		if err != nil {
			return Result{}, err
		}
	}

	defaultNodes := 1
	if mode != 1 {
		defaultNodes = 3
	}
	nodeCount, err := w.askInt("Number of Titanus nodes", defaultNodes, 1, 256)
	if err != nil {
		return Result{}, err
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
		if mode == 1 {
			defaultCaps = "CONTROL,EXECUTION"
		} else if i < 3 {
			defaultCaps = "CONTROL,EXECUTION"
		}
		if mode == 3 && i < 3 {
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
	}

	plan.AutoDeploy, err = w.askBool("Run pre-flight and bootstrap automatically after saving the Plan", false)
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
