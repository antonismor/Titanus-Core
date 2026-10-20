package preflight

import (
	"fmt"
	"strings"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/remote"
)

func TestSummaryNamesEveryFailedNodeAndCheckWithoutCommandOutput(t *testing.T) {
	err := Summary([]NodeReport{{Node: "ctl01", Checks: []Check{{Name: "nftables", Details: "PRIVATE CANARY"}, {Name: "OverlayFS"}}}, {Node: "wrk01", Checks: []Check{{Name: "SSH connectivity"}}}, {Node: "healthy", Successful: true}})
	if err == nil {
		t.Fatal("failed checks accepted")
	}
	for _, want := range []string{"ctl01", "nftables", "OverlayFS", "wrk01", "SSH connectivity"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatal(err)
		}
	}
	if strings.Contains(err.Error(), "PRIVATE CANARY") || strings.Contains(err.Error(), "healthy") {
		t.Fatal("unexpected details in summary", err)
	}
}

type repairFixture struct {
	missing                  map[string]string
	checks, repairs          int
	stillBroken, repairFails bool
}

func (f *repairFixture) Run(node model.NodeSpec, command string) (remote.Result, error) {
	if strings.Contains(command, "TITANUS_DEPENDENCIES_OK") {
		f.repairs++
		if f.repairFails {
			return remote.Result{}, fmt.Errorf("repair command failed")
		}
		if !f.stillBroken {
			delete(f.missing, node.Name)
		}
		return remote.Result{}, nil
	}
	f.checks++
	if command == f.missing[node.Name] {
		return remote.Result{}, fmt.Errorf("original check failure")
	}
	return remote.Result{Stdout: "OK"}, nil
}

func TestRepairRequiresOptInAndFullSuccessfulRecheck(t *testing.T) {
	for _, scenario := range []struct {
		name, failure                                 string
		enabled, persistent, repairFails, unsafeOther bool
		wantOK                                        bool
		wantRepairs, wantChecks                       int
	}{
		{"missing-nft", "command -v nft", true, false, false, false, true, 1, 40},
		{"missing-ip", "command -v ip", true, false, false, false, true, 1, 40},
		{"missing-nsenter", "command -v nsenter", true, false, false, false, true, 1, 40},
		{"missing-overlay", "grep -qw overlay /proc/filesystems", true, false, false, false, true, 1, 40},
		{"disabled", "command -v nft", false, false, false, false, false, 0, 20},
		{"repair-did-not-fix-check", "command -v nft", true, true, false, false, false, 1, 40},
		{"repair-command-failed", "command -v nft", true, false, true, false, false, 1, 20},
		{"unrecognized-kernel-error", "uname -s", true, false, false, false, false, 0, 20},
		{"unsafe-later-node-blocks-all-repairs", "command -v nft", true, false, false, true, false, 0, 20},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			plan := model.RealmPlan{Installation: &model.Installation{Operation: "install", AutoRepair: scenario.enabled}, Nodes: []model.NodeSpec{{Name: "first"}, {Name: "second"}}}
			ssh := &repairFixture{missing: map[string]string{"first": scenario.failure}, stillBroken: scenario.persistent, repairFails: scenario.repairFails}
			if scenario.unsafeOther {
				ssh.missing["second"] = "printf TITANUS_SSH_OK"
			}
			var events []string
			err := (&Runner{SSH: ssh}).Ensure(plan, func(format string, args ...any) { events = append(events, fmt.Sprintf(format, args...)) })
			if (err == nil) != scenario.wantOK || ssh.repairs != scenario.wantRepairs || ssh.checks != scenario.wantChecks {
				t.Fatalf("err=%v repairs=%d checks=%d events=%v", err, ssh.repairs, ssh.checks, events)
			}
			if scenario.wantOK && !strings.Contains(strings.Join(events, "\n"), "Auto repair verified") {
				t.Fatal("verification not reported")
			}
		})
	}
}
