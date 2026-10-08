package disk

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type cephSession struct {
	ID       uint64 `json:"id"`
	Inst     string `json:"inst"`
	Metadata struct {
		Root string `json:"root"`
	} `json:"client_metadata"`
}

func blocklistAddress(address string) string {
	return strings.TrimPrefix(strings.TrimPrefix(address, "v1:"), "v2:")
}

// An MDS flock grant alone does not prove every OSD has applied the eviction.
// Check each live daemon's actual applied blocklist before exposing writable data.
// This first profile fails closed if any registered OSD is down/unreachable.
func (m *Manager) waitCephFSBlocklists(cfg CephConfig, fenced string) error {
	out, err := commandOutput("ceph", append(m.cephBaseArgs(cfg), "osd", "dump", "--format", "json")...)
	if err != nil {
		return err
	}
	var state struct {
		Blocklist map[string]json.RawMessage `json:"blocklist"`
		OSDs      []struct {
			ID int `json:"osd"`
			Up int `json:"up"`
		} `json:"osds"`
	}
	if err = json.Unmarshal([]byte(out), &state); err != nil {
		return err
	}
	required := map[string]bool{}
	for address := range state.Blocklist {
		required[blocklistAddress(address)] = true
	}
	if fenced != "" && !required[blocklistAddress(fenced)] {
		return fmt.Errorf("evicted client is absent from committed OSD blocklist")
	}
	if len(state.OSDs) == 0 {
		return fmt.Errorf("CephFS requires reachable OSDs")
	}
	for _, osd := range state.OSDs {
		if osd.Up != 1 {
			return fmt.Errorf("cannot confirm CephFS fence on down OSD %d", osd.ID)
		}
	}
	if len(required) == 0 {
		return nil
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		complete := true
		for _, osd := range state.OSDs {
			out, err = commandOutput("ceph", append(m.cephBaseArgs(cfg), "tell", "osd."+strconv.Itoa(osd.ID), "dump_blocklist", "--format", "json")...)
			if err != nil {
				return fmt.Errorf("cannot confirm OSD %d fence: %w", osd.ID, err)
			}
			var applied struct {
				Blocklist []struct {
					Address struct {
						Addr  string `json:"addr"`
						Nonce uint64 `json:"nonce"`
					} `json:"entity_addr_t"`
				} `json:"blocklist"`
			}
			if err = json.Unmarshal([]byte(out), &applied); err != nil {
				return err
			}
			present := map[string]bool{}
			for _, entry := range applied.Blocklist {
				present[entry.Address.Addr+"/"+strconv.FormatUint(entry.Address.Nonce, 10)] = true
			}
			for address := range required {
				if !present[address] {
					complete = false
				}
			}
		}
		if complete {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("OSDs have not applied committed CephFS blocklists")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (m *Manager) verifyCephFSFencing(cfg CephConfig) error {
	out, err := commandOutput("ceph", append(m.cephBaseArgs(cfg), "fs", "get", cfg.FSName, "--format", "json")...)
	var fs struct {
		MDSMap struct {
			MaxMDS int `json:"max_mds"`
		} `json:"mdsmap"`
	}
	if err != nil || json.Unmarshal([]byte(out), &fs) != nil || fs.MDSMap.MaxMDS != 1 {
		return fmt.Errorf("CephFS fencing profile requires one active MDS rank")
	}
	for _, option := range []string{"mds_session_blocklist_on_evict", "mds_session_blocklist_on_timeout"} {
		// Ask the active daemon: monitor defaults omit local and runtime overrides.
		out, err := commandOutput("ceph", append(m.cephBaseArgs(cfg), "tell", "mds."+cfg.FSName+":0", "config", "get", option, "--format", "json")...)
		var values map[string]json.RawMessage
		if err != nil || json.Unmarshal([]byte(out), &values) != nil || (string(values[option]) != "true" && string(values[option]) != `"true"`) {
			return fmt.Errorf("CephFS requires active %s=true (query failure also denies attachment): %v: %s", option, err, out)
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
	var clients []cephSession
	if err = json.Unmarshal([]byte(out), &clients); err != nil {
		return err
	}
	match := false
	for _, client := range clients {
		if client.ID == session && client.Inst == "client."+strconv.FormatUint(session, 10)+" "+address && client.Metadata.Root == spec.RemotePath {
			match = true
		}
	}
	if !match {
		return fmt.Errorf("fencing session/address/subvolume no longer match; inspect current clients")
	}
	_, err = commandOutput("ceph", append(m.cephBaseArgs(cfg), "tell", "mds."+cfg.FSName+":0", "client", "evict", "id="+strconv.FormatUint(session, 10))...)
	if err != nil {
		return err
	}
	return m.waitCephFSBlocklists(cfg, address)
}
