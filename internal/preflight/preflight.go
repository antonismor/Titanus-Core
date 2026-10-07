package preflight

import (
	"fmt"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/remote"
)

type Check struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Details string `json:"details"`
}

type NodeReport struct {
	Node       string        `json:"node"`
	Address    string        `json:"address"`
	Checks     []Check       `json:"checks"`
	Duration   time.Duration `json:"duration"`
	Successful bool          `json:"successful"`
}

type Runner struct {
	SSH *remote.SSHExecutor
}

func NewRunner() *Runner {
	return &Runner{SSH: remote.NewSSHExecutor()}
}

func (r *Runner) Run(plan model.RealmPlan) []NodeReport {
	reports := make([]NodeReport, 0, len(plan.Nodes))
	for _, node := range plan.Nodes {
		reports = append(reports, r.checkNode(node))
	}
	return reports
}

func (r *Runner) checkNode(node model.NodeSpec) NodeReport {
	started := time.Now()
	report := NodeReport{Node: node.Name, Address: node.ManagementIP}

	commands := []struct {
		name string
		cmd  string
	}{
		{"SSH connectivity", "printf TITANUS_SSH_OK"},
		{"Linux kernel", "uname -s"},
		{"Debian family", "test -r /etc/debian_version && cat /etc/debian_version"},
		{"64-bit architecture", "uname -m"},
		{"cgroups v2", "test -r /sys/fs/cgroup/cgroup.controllers && cat /sys/fs/cgroup/cgroup.controllers"},
		{"iproute2", "command -v ip"},
		{"nftables", "command -v nft || true"},
		{"OverlayFS", "grep -qw overlay /proc/filesystems"},
		{"passwordless sudo/root", "if [ \"$(id -u)\" = 0 ]; then echo root; else sudo -n true && echo sudo; fi"},
	}

	for _, item := range commands {
		res, err := r.SSH.Run(node, item.cmd)
		check := Check{Name: item.name}
		if err != nil {
			check.Passed = false
			check.Details = err.Error()
		} else {
			check.Passed = true
			check.Details = strings.TrimSpace(res.Stdout)
		}
		report.Checks = append(report.Checks, check)
	}

	report.Successful = true
	for _, c := range report.Checks {
		if !c.Passed {
			report.Successful = false
			break
		}
	}
	report.Duration = time.Since(started)
	return report
}

func Summary(reports []NodeReport) error {
	for _, r := range reports {
		if !r.Successful {
			return fmt.Errorf("pre-flight failed for node %s", r.Node)
		}
	}
	return nil
}
