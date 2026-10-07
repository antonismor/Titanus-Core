package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/antonismor/Titanus-Core/internal/ansi"
	"github.com/antonismor/Titanus-Core/internal/deploy"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/planner"
	"github.com/antonismor/Titanus-Core/internal/preflight"
	"github.com/antonismor/Titanus-Core/internal/setup"
)

const version = "0.1.0-dev"

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
		fmt.Println("  " + ansi.Paint(ansi.Cyan, "5)") + " Show version")
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
				return err
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
	fmt.Print("Plan path [titanus-plan.json]: ")
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(line)
	if path == "" {
		path = "titanus-plan.json"
	}
	return path, nil
}

func pause(reader *bufio.Reader) {
	fmt.Print("\nPress ENTER to continue...")
	_, _ = reader.ReadString('\n')
}

func printHelp() {
	fmt.Print(`Titanus Core CLI

Usage:
  titanus                         Interactive ANSI menu
  titanus setup                   Guided Realm setup
  titanus plan validate FILE      Validate a Titanus Plan
  titanus plan show FILE          Display a Titanus Plan
  titanus preflight FILE          Test all configured nodes
  titanus deploy bootstrap FILE   Bootstrap validated nodes
  titanus version                 Show version
`)
}
