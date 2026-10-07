package deploy

import (
	"fmt"
	"strings"

	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/preflight"
	"github.com/antonismor/Titanus-Core/internal/remote"
)

type NodeResult struct {
	Node    string
	Changed bool
	Message string
}

type Engine struct {
	SSH *remote.SSHExecutor
}

func NewEngine() *Engine {
	return &Engine{SSH: remote.NewSSHExecutor()}
}

// Bootstrap performs only the non-destructive host bootstrap stage.
// Runtime installation, Fabric creation and Ceph provisioning are separate
// stages so that each can be planned, audited and retried idempotently.
func (e *Engine) Bootstrap(plan model.RealmPlan) ([]NodeResult, error) {
	if err := plan.Validate(); err != nil {
		return nil, err
	}

	reports := preflight.NewRunner().Run(plan)
	if err := preflight.Summary(reports); err != nil {
		return nil, fmt.Errorf("deployment blocked: %w", err)
	}

	results := make([]NodeResult, 0, len(plan.Nodes))
	for _, node := range plan.Nodes {
		cmd := strings.Join([]string{
			"set -eu",
			"if [ \"$(id -u)\" = 0 ]; then SUDO=''; else SUDO='sudo -n'; fi",
			"$SUDO install -d -m 0755 /etc/titanus",
			"$SUDO install -d -m 0755 /var/lib/titanus",
			"$SUDO install -d -m 0755 /var/lib/titanus/runtime",
			"$SUDO install -d -m 0755 /var/lib/titanus/sources",
			"$SUDO install -d -m 0755 /var/lib/titanus/disks",
			"$SUDO install -d -m 0755 /var/log/titanus",
			"$SUDO install -d -m 0755 /run/titanus",
			"printf TITANUS_BOOTSTRAP_OK",
		}, "; ")

		res, err := e.SSH.Run(node, cmd)
		if err != nil {
			return results, fmt.Errorf("bootstrap %s: %w", node.Name, err)
		}
		results = append(results, NodeResult{
			Node: node.Name, Changed: true, Message: res.Stdout,
		})
	}
	return results, nil
}
