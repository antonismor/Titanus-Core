package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/antonismor/Titanus-Core/internal/controlapi"
	"github.com/antonismor/Titanus-Core/internal/hatransition"
	"github.com/antonismor/Titanus-Core/internal/localclient"
	"github.com/antonismor/Titanus-Core/internal/model"
)

func runHA(args []string) error {
	if len(args) == 0 {
		return haMenu(bufio.NewReader(os.Stdin))
	}
	fs := flag.NewFlagSet("ha "+args[0], flag.ContinueOnError)
	plan := fs.String("plan", "", "Native HA target plan; never a fresh-install command")
	prepared := fs.String("prepared", "", "private transition.json")
	set := fs.String("backup-set", "", "authenticated complete recovery set")
	key := fs.String("key", "", "private recovery-set authentication key")
	output := fs.String("output", "", "new private output directory")
	node := fs.String("node", "", "exact host identity")
	id := fs.String("id", "", "transition identity for readiness/rollback")
	evidence := fs.String("evidence", "", "independent pre-activation exclusion evidence")
	socket := fs.String("socket", "/run/titanus/titanus.sock", "local controller management socket")
	if e := fs.Parse(args[1:]); e != nil {
		return e
	}
	switch args[0] {
	case "prepare":
		if *plan == "" || *set == "" || *key == "" || *output == "" {
			return fmt.Errorf("prepare requires --plan --backup-set --key --output")
		}
		p, e := model.LoadPlan(*plan)
		if e != nil {
			return e
		}
		out, e := hatransition.Prepare(p, *set, *key, *output)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"id": out.ID, "prepared": filepath.Join(*output, "transition.json"), "hosts": len(out.Hosts), "host_changes": false})
	case "stage":
		if *prepared == "" || *set == "" || *key == "" || *node == "" {
			return fmt.Errorf("stage requires --prepared --backup-set --key --node on the stopped target host")
		}
		return hatransition.Stage(*prepared, *set, *key, *node)
	case "readiness":
		r, e := hatransition.ReadReady(stateRoot(), *id)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"id": r.ID, "node": r.Node, "phase": r.Phase, "backup_set_id": r.BackupSetID})
	case "activate":
		p, e := hatransition.Load(*prepared)
		if e != nil {
			return e
		}
		client := localclient.New(*socket)
		// Prepare floors through the existing authenticated, original-membership
		// protocol. Every target retains one stable migration identity across retries.
		for target := 1; target <= 4; target++ {
			state, e := client.RealmState()
			if e != nil {
				return e
			}
			if state.SchemaVersion >= target {
				continue
			}
			migrationID := "ha-transition-" + p.ID + fmt.Sprintf("-schema-%d", target)
			if _, e = client.TransitionSchemaTo(migrationID, state.Revision, target); e != nil {
				return fmt.Errorf("schema preparation interrupted; retry this same prepared transition: %w", e)
			}
		}
		state, e := client.RealmState()
		if e != nil {
			return e
		}
		receipt, e := client.ActivateNativeHA(controlapi.HAActivateRequest{Prepared: p, ExpectedRevision: state.Revision})
		if e != nil {
			return e
		}
		// A lost CLI response is resolved by the quorum-stored transition identity.
		return json.NewEncoder(os.Stdout).Encode(receipt)
	case "rollback-staging":
		return hatransition.RollbackStaging(stateRoot(), *id, *evidence)
	default:
		return fmt.Errorf("unknown HA transition operation")
	}
}

func haMenu(reader *bufio.Reader) error {
	fmt.Println("Titanus Standard → Native HA transition")
	fmt.Println("  1) Prepare from verified complete backup")
	fmt.Println("  2) Stage configuration on this stopped host")
	fmt.Println("  3) Verify staged host readiness")
	fmt.Println("  4) Activate through the existing quorum")
	fmt.Println("  5) Roll back pre-activation configuration")
	fmt.Println("  0) Exit")
	choice, e := prompt(reader, "Select")
	if e != nil {
		return e
	}
	prompts := map[string][]string{"1": {"--plan", "--backup-set", "--key", "--output"}, "2": {"--prepared", "--backup-set", "--key", "--node"}, "3": {"--id"}, "4": {"--prepared"}, "5": {"--id", "--evidence"}}
	operations := map[string]string{"1": "prepare", "2": "stage", "3": "readiness", "4": "activate", "5": "rollback-staging"}
	if choice == "0" {
		return nil
	}
	operation, ok := operations[choice]
	if !ok {
		return fmt.Errorf("unknown selection")
	}
	args := []string{operation}
	for _, name := range prompts[choice] {
		value, e := prompt(reader, strings.TrimPrefix(name, "--"))
		if e != nil {
			return e
		}
		args = append(args, name, value)
	}
	return runHA(args)
}
