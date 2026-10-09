package offline

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/durable"
)

func TestSchemaFloorSurvivesRecoveryAndFailsClosed(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	f := NewSchemaFloor("LAB", "schema-floor-test-001", 1)
	if e := PrepareSchemaFloor(root, f); e != nil {
		t.Fatal(e)
	}
	if e := PrepareSchemaFloor(root, f); e != nil {
		t.Fatal("idempotent floor", e)
	}
	if e := CheckStartup(root); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(root+".recovery-pending", []byte("actual restore in progress"), 0600); e != nil {
		t.Fatal(e)
	}
	if CheckStartup(root) == nil {
		t.Fatal("recovery blocker bypassed by archived floor")
	}
	if e := FinishRecovery(root); e != nil {
		t.Fatal(e)
	}
	if e := CheckSchemaFloor(root); e != nil {
		t.Fatal("restored floor lost", e)
	}
	if e := os.Remove(root + ".recovery-pending"); e != nil {
		t.Fatal(e)
	}
	if CheckStartup(root) == nil {
		t.Fatal("missing legacy-binary blocker admitted")
	}
}

func TestConflictingUpgradeCannotRewriteAnInterruptedFloor(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	one := NewSchemaFloor("LAB", "schema-floor-one-proof-001", 1)
	two := NewSchemaFloor("LAB", "schema-floor-two-proof-001", 2)
	if e := PrepareSchemaFloor(root, one); e != nil {
		t.Fatal(e)
	}
	// Simulate the second record having been durably published first. Resume
	// must handle either ordering without allowing another migration to wedge it.
	if e := durable.WriteJSON(root+".recovery-pending", two, 0600); e != nil {
		t.Fatal(e)
	}
	before, e := os.ReadFile(floorPath(root))
	if e != nil {
		t.Fatal(e)
	}
	wrong := NewSchemaFloor("LAB", "different-schema-two-proof", 2)
	if e := PrepareSchemaFloor(root, wrong); e == nil {
		t.Fatal("conflicting interrupted preparation admitted")
	}
	after, e := os.ReadFile(floorPath(root))
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("rejected preparation rewrote archived floor", e)
	}
	if CheckStartup(root) == nil {
		t.Fatal("disagreeing interrupted floors allowed startup")
	}
	if e := PrepareSchemaFloor(root, two); e != nil {
		t.Fatal("original migration cannot resume", e)
	}
	if e := CheckCommittedFloor(root, two.Realm, two.MigrationID, 2); e != nil {
		t.Fatal(e)
	}
}

func TestCommittedFloorRejectsDifferentMigrationOrRealm(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	f := NewSchemaFloor("LAB", "floor-committed-proof-001", 1)
	if e := PrepareSchemaFloor(root, f); e != nil {
		t.Fatal(e)
	}
	for _, pair := range [][2]string{{"OTHER", f.MigrationID}, {f.Realm, "different-migration-001"}} {
		if CheckCommittedFloor(root, pair[0], pair[1], 1) == nil {
			t.Fatal("unrelated floor admitted")
		}
	}
}
