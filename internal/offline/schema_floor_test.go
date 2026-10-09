package offline

import (
	"os"
	"path/filepath"
	"testing"
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
