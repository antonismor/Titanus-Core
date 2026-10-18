package setup

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/model"
)

func wizardInput(file string, count int, profile string) string {
	lines := []string{profile}
	if profile == "3" {
		lines = append(lines, "n")
	}
	lines = append(lines, "3", "LAB", "", "", fmt.Sprint(count), "", "22")
	if count >= 3 {
		lines = append(lines, "3")
	}
	for i := 0; i < count; i++ {
		lines = append(lines, fmt.Sprintf("node%d", i+1), fmt.Sprintf("192.0.2.%d", i+1), "", "")
	}
	lines = append(lines, "0.4.0-rc.4", strings.Repeat("a", 40), "", "", "", "", "", "n", file, "")
	return strings.Join(lines, "\n")
}
func TestWizardSavesOfflineVersionedLifecyclePlan(t *testing.T) {
	for _, count := range []int{1, 2, 3, 9} {
		for _, profile := range []string{"1", "3"} {
			t.Run(fmt.Sprintf("%s-%d", profile, count), func(t *testing.T) {
				file := filepath.Join(t.TempDir(), "plan.json")
				var out bytes.Buffer
				result, e := New(strings.NewReader(wizardInput(file, count, profile)), &out).Run()
				if e != nil {
					t.Fatalf("%v\n%s", e, out.String())
				}
				p, e := model.LoadPlan(file)
				if e != nil {
					t.Fatal(e)
				}
				if result.PlanPath != file || p.Installation == nil || p.Installation.Operation != "rollback" || p.AutoDeploy || p.Version != "titanus-plan/v2" || len(p.Nodes) != count {
					t.Fatal("wizard lost lifecycle configuration")
				}
				if !strings.Contains(out.String(), "Soon Available") {
					t.Fatal("Enterprise status missing")
				}
				for _, n := range p.Nodes {
					if n.SSHUser != "root" || n.SSHPort != 22 {
						t.Fatal("shared SSH configuration lost")
					}
				}
			})
		}
	}
}
func TestWizardRejectsNativeHAMigrationBeforeSaving(t *testing.T) {
	var out bytes.Buffer
	_, err := New(strings.NewReader("2\n2\n"), &out).Run()
	if err == nil || !strings.Contains(err.Error(), "fresh install") {
		t.Fatal(err)
	}
}
func TestWizardRepromptsOutOfRangeCount(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plan.json")
	in := strings.Replace(wizardInput(file, 1, "1"), "LAB\n\n\n1\n", "LAB\n\n\n10\n0\n1\n", 1)
	var out bytes.Buffer
	if _, err := New(strings.NewReader(in), &out).Run(); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if strings.Count(out.String(), "Enter a number between 1 and 9.") != 2 {
		t.Fatal(out.String())
	}
	raw, _ := os.ReadFile(file)
	if strings.Contains(string(raw), "password") {
		t.Fatal("plan contains credential field")
	}
}
func TestPasswordRefusesRedirectedInput(t *testing.T) {
	var out bytes.Buffer
	if _, err := New(strings.NewReader("secret\n"), &out).readPassword(); err == nil {
		t.Fatal("accepted redirected password")
	}
	if strings.Contains(out.String(), "secret") {
		t.Fatal("password exposed")
	}
}

func TestWizardSavesNativeHAWithSeparateWorkersAndFencing(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned fencing files: covered by dedicated isolated root CI step")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "plan.json")
	inventory := filepath.Join(dir, "fencing.json")
	data := `{"node4":{"provider":"libvirt","uuid":"00000000-0000-0000-0000-000000000004","address":"qemu+ssh://root@hypervisor.example/system"},"node5":{"provider":"libvirt","uuid":"00000000-0000-0000-0000-000000000005","address":"qemu+ssh://root@hypervisor.example/system"}}`
	if e := os.WriteFile(inventory, []byte(data), 0600); e != nil {
		t.Fatal(e)
	}
	lines := []string{"2", "1", "LAB", "", "", "5", "", "22", "3"}
	for i := 1; i <= 5; i++ {
		lines = append(lines, fmt.Sprintf("node%d", i), fmt.Sprintf("192.0.2.%d", i), "", "")
	}
	lines = append(lines, inventory, "0.4.0-rc.13", strings.Repeat("a", 40), "amd.tar.gz", strings.Repeat("b", 64), "arm.tar.gz", strings.Repeat("c", 64), "", "", "", "", "", "y", "n", file, "")
	var out bytes.Buffer
	result, e := New(strings.NewReader(strings.Join(lines, "\n")), &out).Run()
	if e != nil {
		t.Fatalf("%v\n%s", e, out.String())
	}
	plan, e := model.LoadPlan(result.PlanPath)
	if e != nil {
		t.Fatal(e)
	}
	if plan.Installation.Profile != model.ProfileNativeHA || len(plan.Installation.PowerFencing) != 2 || len(plan.ControlNodes()) != 3 {
		t.Fatal("HA profile lost policy")
	}
	for _, n := range plan.ControlNodes() {
		for _, c := range n.Capabilities {
			if c == model.CapabilityExecution {
				t.Fatal("voter also Worker")
			}
		}
	}
}
