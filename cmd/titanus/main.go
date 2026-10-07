package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/ansi"
	"github.com/antonismor/Titanus-Core/internal/deploy"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/planner"
	"github.com/antonismor/Titanus-Core/internal/preflight"
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
	case "source":
		return runSource(args[1:])
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
			if err := unitMenu(reader); err != nil {
				ansi.Error(err.Error())
				pause(reader)
			}
		case "7":
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
		spec := unitruntime.Spec{
			ID:          id,
			Source:      *sourceName,
			Hostname:    *hostname,
			Command:     command,
			MemoryBytes: memoryBytes,
			CPUPercent:  *cpu,
			PidsMax:     *pids,
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
		fmt.Printf("%-24s %-12s %-10s\n", "UNIT", "STATUS", "PID")
		for _, state := range states {
			pid := "-"
			if state.PID > 0 {
				pid = strconv.Itoa(state.PID)
			}
			fmt.Printf("%-24s %-12s %-10s\n", state.ID, state.Status, pid)
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

  titanus source list                          List Sources
  titanus source import NAME ROOTFS            Import a rootfs directory

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

Development overrides:
  TITANUS_STATE_ROOT
  TITANUS_CGROUP_ROOT
  TITANUS_INIT_BINARY
`)
}
