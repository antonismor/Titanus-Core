package preflight

import (
	"fmt"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/prerequisites"
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
	SSH interface {
		Run(model.NodeSpec, string) (remote.Result, error)
	}
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

// Ensure is used only by explicit deployment. Run remains read-only. Recognized
// failures get one fixed remedy followed by a new full pre-flight; unsupported
// failures block repair admission across the whole Plan.
func (r *Runner) Ensure(plan model.RealmPlan, logf func(string, ...any)) error {
	reports := r.Run(plan)
	initial := Summary(reports)
	if initial == nil || !prerequisites.RepairEnabled(plan) {
		return initial
	}
	for _, report := range reports {
		for _, check := range report.Checks {
			if !check.Passed {
				if _, ok := prerequisites.RepairScript(check.Name); !ok {
					return fmt.Errorf("%w; automatic repair has no safe remedy for %s on %s", initial, check.Name, report.Node)
				}
			}
		}
	}
	for index, report := range reports {
		for _, check := range report.Checks {
			if check.Passed {
				continue
			}
			command, _ := prerequisites.RepairScript(check.Name)
			if logf != nil {
				logf("[%s] Auto repair: %s; applying targeted remedy, attempt 1/1", report.Node, check.Name)
			}
			if _, err := r.SSH.Run(plan.Nodes[index], command); err != nil {
				return fmt.Errorf("automatic repair of %s on %s failed: %w; original failure: %v", check.Name, report.Node, err, initial)
			}
		}
	}
	if logf != nil {
		logf("Auto repair: re-running every pre-flight check on all %d nodes", len(plan.Nodes))
	}
	if err := Summary(r.Run(plan)); err != nil {
		return fmt.Errorf("automatic repair exhausted; verification failed: %w", err)
	}
	if logf != nil {
		logf("Auto repair verified: all pre-flight checks passed; continuing installation")
	}
	return nil
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
		{"nftables", "command -v nft"},
		{"nsenter", "command -v nsenter"},
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
	var failures []string
	for _, r := range reports {
		if !r.Successful {
			var checks []string
			for _, check := range r.Checks {
				if !check.Passed {
					checks = append(checks, check.Name)
				}
			}
			if len(checks) == 0 {
				checks = append(checks, "unspecified check")
			}
			failures = append(failures, fmt.Sprintf("%s [%s]", r.Node, strings.Join(checks, ", ")))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("pre-flight failed: %s", strings.Join(failures, "; "))
	}
	return nil
}
