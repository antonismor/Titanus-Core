package disk

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func (m *Manager) verifyCephFSFencing(cfg CephConfig) error {
	for _, option := range []string{"mds_session_blocklist_on_evict", "mds_session_blocklist_on_timeout"} {
		out, err := commandOutput("ceph", append(m.cephBaseArgs(cfg), "config", "get", "mds", option)...)
		if err != nil || strings.TrimSpace(out) != "true" {
			return fmt.Errorf("CephFS requires %s=true (query failure also denies attachment)", option)
		}
	}
	return nil
}

// FenceCephFS requires an observed session ID AND address. Exact matching avoids
// evicting a reused ID or accidentally fencing unrelated mounts. MDS eviction
// blocklists that instance and advances the OSD-map barrier before releasing
// distributed locks. No lock is broken merely because a scheduling lease expired.
func (m *Manager) FenceCephFS(name string, session uint64, address string) error {
	spec, err := m.Inspect(name)
	if err != nil {
		return err
	}
	if spec.Provider != ProviderCephFS || session == 0 || address == "" {
		return fmt.Errorf("CephFS fencing requires exact observed session/address")
	}
	cfg, err := m.CephConfig()
	if err != nil {
		return err
	}
	if err = m.verifyCephFSFencing(cfg); err != nil {
		return err
	}
	out, err := commandOutput("ceph", append(m.cephBaseArgs(cfg), "tell", "mds."+cfg.FSName+":0", "client", "ls", "--format", "json")...)
	if err != nil {
		return err
	}
	var clients []struct {
		ID       uint64            `json:"id"`
		Inst     string            `json:"inst"`
		Metadata map[string]string `json:"client_metadata"`
	}
	if err = json.Unmarshal([]byte(out), &clients); err != nil {
		return err
	}
	match := false
	for _, client := range clients {
		if client.ID == session && client.Inst == "client."+strconv.FormatUint(session, 10)+" "+address && client.Metadata["root"] == spec.RemotePath {
			match = true
		}
	}
	if !match {
		return fmt.Errorf("fencing session/address/subvolume no longer match; inspect current clients")
	}
	_, err = commandOutput("ceph", append(m.cephBaseArgs(cfg), "tell", "mds."+cfg.FSName+":0", "client", "evict", "id="+strconv.FormatUint(session, 10))...)
	return err
}
