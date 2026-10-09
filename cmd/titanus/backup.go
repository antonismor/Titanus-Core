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
	intent := fs.String("intent", "", "signed complete cluster backup intent")
	storage := fs.String("storage", "", "authenticated Ceph data bundle directory")
	setPath := fs.String("set", "", "sealed complete cluster recovery set")
	node := fs.String("node", "", "original host identity")
	root := fs.String("state-dir", "", "offline configured storage manager root")
	hosts := fs.String("hosts", "", "JSON array of all original host plans")
	proofs := fs.String("proofs", "", "JSON array of authenticated recovery proof paths")
	completion := fs.String("completion", "", "authenticated all-host plus storage completion")
	output := fs.String("output", "", "new absolute attestation path")
	if e := fs.Parse(args[1:]); e != nil {
		return e
	}
	if fs.NArg() != 0 || *key == "" {
		return fmt.Errorf("backup requires --key and named flags")
	}
	switch args[0] {
	case "cluster-intent":
		p, e := backup.LoadPlan(*planPath)
		if e != nil {
			return e
		}
		i, e := backup.CreateClusterIntent(p, *hosts, *key, *output)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"recovery_set_id": i.Binding.ID, "hosts": len(i.Hosts), "revision": i.Binding.Revision})
	case "ceph-export":
		m, e := backup.ExportStorage(*root, *intent, *key, *output)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"recovery_set_id": m.Binding.ID, "disks": len(m.Binding.Catalogs), "entries": len(m.Entries)})
	case "ceph-verify":
		m, e := backup.VerifyStorage(*storage, *key)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"recovery_set_id": m.Binding.ID, "verified": true})
	case "cluster-create-host":
		p, e := backup.LoadPlan(*planPath)
		if e != nil {
			return e
		}
		m, e := backup.CreateClusterHost(p, *archive, *key, *intent, *storage)
		if e != nil {
			return e
		}
		return printBackupSummary(m)
	case "cluster-seal":
		s, e := backup.SealClusterSet(*planPath, *key, *output)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"recovery_set_id": s.Intent.Binding.ID, "hosts": len(s.Hosts)})
	case "cluster-verify":
		s, e := backup.VerifyClusterSet(*setPath, *key)
		if e != nil {
			return e
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"recovery_set_id": s.Intent.Binding.ID, "verified": true})
	case "ceph-attest-fence":
		return backup.AttestStorageFence(*setPath, *key, *record, *output)
	case "ceph-import":
		return backup.ImportStorage(*root, *setPath, *key, *fence, *output)
	case "cluster-restore-host":
		m, e := backup.RestoreClusterHost(*setPath, *key, *node, *fence)
		if e != nil {
			return e
		}
		return printBackupSummary(m)
	case "cluster-prove-host":
		return backup.ProveClusterHost(*setPath, *key, *node, *output)
	case "cluster-complete":
		return backup.CompleteClusterRecovery(*setPath, *key, *proofs, *output)
	case "cluster-finalize-host":
		return backup.FinalizeClusterHost(*setPath, *key, *node, *completion)

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
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"format": m.Format, "backup_id": m.ID, "realm": m.Plan.Realm, "node": m.Plan.Node, "realm_revision": m.RealmRevision, "entries": len(m.Entries), "build": m.Build, "scope": func() string {
		if m.Cluster != nil {
			return "offline-cluster-set"
		}
		return "offline-host-local"
	}()})
}
