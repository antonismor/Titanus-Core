package haprofile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/fencing"
	"path/filepath"
	"strings"
)

type FileChange struct {
	Before  []byte `json:"before,omitempty"`
	After   []byte `json:"after"`
	Existed bool   `json:"existed"`
}
type HostReceipt struct {
	ID                 string                `json:"id"`
	Node               string                `json:"node"`
	Realm              string                `json:"realm"`
	ConfigRoot         string                `json:"config_root"`
	StateRoot          string                `json:"state_root"`
	BackupSetID        string                `json:"backup_set_id"`
	TargetCapabilities string                `json:"target_capabilities"`
	Phase              string                `json:"phase"`
	Files              map[string]FileChange `json:"files"`
}

func ReceiptPath(root, id string) (string, error) {
	b, e := hex.DecodeString(id)
	if e != nil || len(b) != 32 {
		return "", fmt.Errorf("invalid transition ID")
	}
	return filepath.Join(root, "ha-transitions", id, "receipt.json"), nil
}
func FileMatches(path string, want []byte) bool {
	b, e := fencing.ReadPrivateFile(path)
	return e == nil && sha256.Sum256(b) == sha256.Sum256(want)
}
func ReadReceipt(path string) (HostReceipt, error) {
	var r HostReceipt
	b, e := fencing.ReadPrivateFile(path)
	if e != nil {
		return r, e
	}
	e = json.Unmarshal(b, &r)
	return r, e
}
func ValidReceipt(r HostReceipt) error {
	if _, e := ReceiptPath(r.StateRoot, r.ID); e != nil {
		return e
	}
	if r.Node == "" || r.Realm == "" || r.BackupSetID == "" || r.ConfigRoot == "" || !filepath.IsAbs(r.ConfigRoot) || filepath.Clean(r.ConfigRoot) != r.ConfigRoot || len(r.Files) == 0 {
		return fmt.Errorf("invalid host transition receipt")
	}
	prefix := "native-ha/" + r.ID + "/"
	for name := range r.Files {
		if name != "daemon.env" && name != "agent.env" && (!strings.HasPrefix(name, prefix) || filepath.Clean(name) != name || strings.Contains(name, "..")) {
			return fmt.Errorf("unsafe transition configuration path")
		}
	}
	return nil
}
func ReadReady(root, id string) (HostReceipt, error) {
	path, e := ReceiptPath(root, id)
	if e != nil {
		return HostReceipt{}, e
	}
	r, e := ReadReceipt(path)
	if e != nil {
		return r, e
	}
	if e = ValidReceipt(r); e != nil {
		return r, e
	}
	if r.ID != id || r.StateRoot != root || r.Phase != "staged" {
		return r, fmt.Errorf("host transition is not staged")
	}
	for name, c := range r.Files {
		if !FileMatches(filepath.Join(r.ConfigRoot, name), c.After) {
			return r, fmt.Errorf("staged configuration changed: %s", name)
		}
	}
	return r, nil
}
