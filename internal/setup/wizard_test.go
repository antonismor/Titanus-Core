package setup

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/model"
)

func TestWizardSavesOfflineVersionedLifecyclePlan(t *testing.T) {
	file := filepath.Join(t.TempDir(), "plan.json")
	// Single node rollback needs no archive upload, SSH discovery or new PKI.
	input := strings.Join([]string{"1", "3", "LAB", "", "", "1", "node", "192.0.2.1", "192.0.2.2", "", "", "", "0.4.0-rc.4", strings.Repeat("a", 40), "", "", "", "", "", "n", file, ""}, "\n")
	var out bytes.Buffer
	result, e := New(strings.NewReader(input), &out).Run()
	if e != nil {
		t.Fatalf("%v\n%s", e, out.String())
	}
	p, e := model.LoadPlan(file)
	if e != nil {
		t.Fatal(e)
	}
	if result.PlanPath != file || p.Installation == nil || p.Installation.Operation != "rollback" || p.AutoDeploy || p.Version != "titanus-plan/v2" {
		t.Fatal("wizard lost lifecycle configuration")
	}
}
