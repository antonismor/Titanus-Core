package offline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/version"
)

type SchemaFloor struct {
	Format      string `json:"format"`
	Realm       string `json:"realm"`
	MigrationID string `json:"migration_id"`
	Schema      int    `json:"schema"`
}

func NewSchemaFloor(realm, id string, schema int) SchemaFloor {
	return SchemaFloor{"titanus-schema-floor/v1", realm, id, schema}
}
func (f SchemaFloor) Validate() error {
	if f.Format != "titanus-schema-floor/v1" || f.Realm == "" || !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{15,127}$`).MatchString(f.MigrationID) || (f.Schema < 1 || f.Schema > version.MaxSchema) || !version.Compatible().Supports(f.Schema) {
		return fmt.Errorf("unsupported schema rollback floor")
	}
	return nil
}
func floorPath(root string) string { return filepath.Join(root, "compatibility", "schema-floor.json") }
func readFloor(path string) (SchemaFloor, error) {
	var f SchemaFloor
	st, e := os.Lstat(path)
	if e != nil {
		return f, e
	}
	if !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) || st.Size() > 4096 {
		return f, fmt.Errorf("unsafe schema floor")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return f, e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&f); e != nil {
		return f, e
	}
	if d.Decode(new(any)) != io.EOF {
		return f, fmt.Errorf("extra schema floor document")
	}
	return f, f.Validate()
}

// Prepare is idempotent only for the same exact migration. Both records are
// durable before acknowledging: the archived floor survives DR, and the old
// binary's existing maintenance blocker prevents destructive binary rollback.
func PrepareSchemaFloor(root string, f SchemaFloor) error {
	lock, e := SchemaMaintenance()
	if e != nil {
		return e
	}
	if lock != nil {
		defer lock.Close()
	}
	if e := f.Validate(); e != nil {
		return e
	}
	// Validate both old records before publishing either one. A conflicting
	// retry must not overwrite the earlier record and make an interrupted
	// higher-schema preparation impossible to resume with its original ID.
	publish := []string{}
	for _, p := range []string{floorPath(root), root + ".recovery-pending"} {
		old, e := readFloor(p)
		if e == nil {
			if old != f && (old.Realm != f.Realm || old.Schema >= f.Schema) {
				return fmt.Errorf("different schema floor already prepared")
			}
			if old == f {
				continue
			}
		}
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		publish = append(publish, p)
	}
	for _, p := range publish {
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return e
		}
		if e = durable.WriteJSON(p, f, 0600); e != nil {
			return e
		}
	}
	return nil
}
func CheckSchemaFloor(root string) error {
	a, e := readFloor(floorPath(root))
	if os.IsNotExist(e) {
		return fmt.Errorf("schema floor missing")
	}
	if e != nil {
		return e
	}
	b, e := readFloor(root + ".recovery-pending")
	if e != nil {
		return e
	}
	if a != b {
		return fmt.Errorf("schema floors disagree")
	}
	return nil
}

// FinishRecovery atomically replaces the legacy recovery blocker with the
// restored schema floor, never permitting an incompatible old binary to start.
func FinishRecovery(root string) error {
	f, e := readFloor(floorPath(root))
	if os.IsNotExist(e) {
		return os.Remove(root + ".recovery-pending")
	}
	if e != nil {
		return e
	}
	return durable.WriteJSON(root+".recovery-pending", f, 0600)
}

func SchemaMaintenance() (*os.File, error) {
	if os.Geteuid() != 0 {
		return nil, nil
	}
	return acquireMode("/run/titanus-schema.lock", true, false)
}

func ReadSchemaFloor(root string) (SchemaFloor, error) { return readFloor(floorPath(root)) }

// CheckCommittedFloor binds the local rollback guard to the committed data,
// rather than merely accepting two mutually agreeing documents for another Realm.
func CheckCommittedFloor(root, realm, migration string, schema int) error {
	if e := CheckSchemaFloor(root); e != nil {
		return e
	}
	f, e := ReadSchemaFloor(root)
	if e != nil {
		return e
	}
	if f.Realm != realm || f.Schema < schema || (f.Schema == schema && f.MigrationID != migration) {
		return fmt.Errorf("schema floor differs from committed Realm migration")
	}
	return nil
}
