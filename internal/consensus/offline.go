package consensus

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/version"
	"github.com/hashicorp/raft"
	raftbolt "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
)

type offlineCheckpoint struct {
	Profile          string      `json:"state_profile"`
	DatabaseSHA256   string      `json:"database_sha256"`
	MembershipSHA256 string      `json:"membership_sha256"`
	State            realm.State `json:"state"`
}

func fileDigest(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// Called only after Raft shutdown and database close. Its hash becomes invalid
// on any subsequent Raft write, so a crash/stale export cannot authorize backup.
func (n *Node) writeOfflineCheckpoint() error {
	dir := filepath.Join(n.root, "realm", "consensus")
	db, e := fileDigest(filepath.Join(dir, "raft.db"))
	if e != nil {
		return e
	}
	membership, e := fileDigest(filepath.Join(dir, "membership.json"))
	if e != nil {
		return e
	}
	return durable.WriteJSON(filepath.Join(dir, "offline.json"), offlineCheckpoint{version.StateProfile, db, membership, n.Snapshot()}, 0600)
}

// ReadOfflineState never binds a port, starts Raft or force-bootstraps voters.
// It requires a matching graceful-shutdown checkpoint, not the legacy seed.
func ReadOfflineState(root string) (realm.State, error) {
	dir := filepath.Join(root, "realm", "consensus")
	data, e := os.ReadFile(filepath.Join(dir, "offline.json"))
	if e != nil || len(data) > MaxStateBytes+4096 {
		return realm.State{}, fmt.Errorf("fresh graceful HA shutdown checkpoint required")
	}
	var c offlineCheckpoint
	if e = json.Unmarshal(data, &c); e != nil || c.Profile != version.StateProfile || c.State.Revision == 0 || c.State.Nodes == nil || c.State.Fleets == nil || c.State.Assignments == nil || c.State.Routes == nil || c.State.Policies == nil {
		return realm.State{}, fmt.Errorf("invalid HA shutdown checkpoint")
	}
	for name, want := range map[string]string{"raft.db": c.DatabaseSHA256, "membership.json": c.MembershipSHA256} {
		got, e := fileDigest(filepath.Join(dir, name))
		if e != nil || got != want {
			return realm.State{}, fmt.Errorf("HA shutdown checkpoint differs from %s", name)
		}
	}
	db, e := raftbolt.New(raftbolt.Options{Path: filepath.Join(dir, "raft.db"), BoltOptions: &bbolt.Options{ReadOnly: true, Timeout: time.Second}})
	if e != nil {
		return realm.State{}, fmt.Errorf("offline Raft database unavailable: %w", e)
	}
	defer db.Close()
	first, e := db.FirstIndex()
	if e != nil {
		return realm.State{}, e
	}
	last, e := db.LastIndex()
	if e != nil || last < first || last-first > 500000 {
		return realm.State{}, fmt.Errorf("invalid/unbounded offline Raft log")
	}
	for index := first; index != 0 && index <= last; index++ {
		var log raft.Log
		if e := db.GetLog(index, &log); e != nil {
			return realm.State{}, e
		}
		if log.Type != raft.LogCommand {
			continue
		}
		var state realm.State
		if len(log.Data) > MaxStateBytes || json.Unmarshal(log.Data, &state) != nil || state.Name != c.State.Name || state.Revision > c.State.Revision {
			return realm.State{}, fmt.Errorf("unapplied/uncertain Raft command prevents offline backup")
		}
		if state.Revision == c.State.Revision {
			want, _ := json.Marshal(c.State)
			got, _ := json.Marshal(state)
			if string(want) != string(got) {
				return realm.State{}, fmt.Errorf("uncertain Raft state differs from exported checkpoint")
			}
		}
	}
	return c.State, nil
}
