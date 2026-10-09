package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/antonismor/Titanus-Core/internal/backup"
)

func runBackup(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: titanus backup <keygen|create|verify|attest-fence|restore> [flags]")
	}
	fs := flag.NewFlagSet("backup "+args[0], flag.ContinueOnError)
	planPath := fs.String("plan", "", "versioned host backup plan")
	archive := fs.String("archive", "", "absolute archive path")
	key := fs.String("key", "", "private external 32-byte authentication key")
	fence := fs.String("fence", "", "authenticated exclusion attestation for restore")
	record := fs.String("record", "", "operator's independently obtained exclusion record")
	output := fs.String("output", "", "new absolute attestation path")
	if e := fs.Parse(args[1:]); e != nil {
		return e
	}
	if fs.NArg() != 0 || *key == "" {
		return fmt.Errorf("backup requires --key and named flags")
	}
	switch args[0] {
	case "keygen":
		return backup.Keygen(*key)
	case "verify":
		m, e := backup.Verify(*archive, *key)
		if e != nil {
			return e
		}
		return printBackupSummary(m)
	case "attest-fence":
		f, e := backup.AttestFence(*archive, *key, *record, *output)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"backup_id": f.BackupID, "expires_at": f.ExpiresAt, "attestation": *output})
	case "create", "restore":
		if os.Geteuid() != 0 {
			return fmt.Errorf("offline host backup/recovery requires root")
		}
		p, e := backup.LoadPlan(*planPath)
		if e != nil {
			return e
		}
		var m backup.Manifest
		if args[0] == "create" {
			m, e = backup.Create(p, *archive, *key)
		} else {
			m, e = backup.Restore(*archive, *key, *fence, p)
		}
		if e != nil {
			return e
		}
		return printBackupSummary(m)
	default:
		return fmt.Errorf("unknown backup operation")
	}
}

func printBackupSummary(m backup.Manifest) error {
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"format": m.Format, "backup_id": m.ID, "realm": m.Plan.Realm, "node": m.Plan.Node, "realm_revision": m.RealmRevision, "entries": len(m.Entries), "build": m.Build, "scope": "offline-host-local"})
}
