package disk

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAppliedOSDBlocklistCephStream(t *testing.T) {
	// Squid OSD::dump_blocklist formats two consecutive top-level arrays.
	output := `[{"entity_addr_t":{"type":"v1","addr":"127.0.0.1:0","nonce":979770770},"expire_time":"2026-10-09"},{"entity_addr_t":{"type":"v2","addr":"[::1]:0","nonce":0}}]
[{"entity_addr_t":{"type":"v1","addr":"192.0.2.0:0","nonce":32}}]`
	present, err := appliedOSDBlocklist(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(present) != 2 || !present["127.0.0.1:0/979770770"] || !present["[::1]:0/0"] {
		t.Fatal("exact instance addresses were lost", present)
	}
	if present["192.0.2.0:0/32"] || present["127.0.0.1:0/979770771"] {
		t.Fatal("range or different nonce counted as exact fence")
	}
	if empty, err := appliedOSDBlocklist(`[] []`); err != nil || len(empty) != 0 {
		t.Fatal("empty native blocklists", empty, err)
	}
}

func TestAppliedOSDBlocklistFailsClosed(t *testing.T) {
	for _, output := range []string{
		``, `[]`, `[] [`, `null []`, `[] null`, `[] [] []`, `[] [] diagnostic`,
		`{"blocklist":[]} []`, `[{}] []`,
		`[{"entity_addr_t":{"addr":"127.0.0.1:0"}}] []`,
		`[{"entity_addr_t":{"addr":"127.0.0.1:0","nonce":null}}] []`,
		`[{"entity_addr_t":{"addr":"127.0.0.1:0","nonce":-1}}] []`,
	} {
		if _, err := appliedOSDBlocklist(output); err == nil {
			t.Fatalf("accepted invalid fence evidence: %s", output)
		}
	}
}

func TestCephFSFenceWaitsForAppliedInstance(t *testing.T) {
	dir := t.TempDir()
	// The monitor has committed the exact fence. The OSD initially reports
	// another nonce on the same endpoint, then applies the actual instance.
	script := `#!/bin/sh
case "$*" in
  *"osd dump"*) printf '%s' '{"blocklist":{"v1:127.0.0.1:0/99":"expiry"},"osds":[{"osd":0,"up":1}]}' ;;
  *"tell osd.0 dump_blocklist"*)
    if [ -e "$FENCE_FIXTURE_READY" ]; then
      printf '%s' '[{"entity_addr_t":{"addr":"127.0.0.1:0","nonce":99}}][]'
    else
      touch "$FENCE_FIXTURE_READY"
      printf '%s' '[{"entity_addr_t":{"addr":"127.0.0.1:0","nonce":98}}][]'
    fi ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "ceph"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	marker := filepath.Join(dir, "ready")
	t.Setenv("FENCE_FIXTURE_READY", marker)
	if err := NewManager(t.TempDir()).waitCephFSBlocklists(CephConfig{}, "v1:127.0.0.1:0/99"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("did not consult the OSD's applied map", err)
	}
}

func TestCephFSFenceRejectsMissingCommitAndDownOSD(t *testing.T) {
	for _, state := range []string{
		`{"osds":[{"osd":0,"up":1}]}`,
		`{"blocklist":null,"osds":[{"osd":0,"up":1}]}`,
		`{"blocklist":{},"osds":[{"osd":0,"up":1}]}`,
		`{"blocklist":{"127.0.0.1:0/99":"expiry"},"osds":[]}`,
		`{"blocklist":{"127.0.0.1:0/99":"expiry"},"osds":[{"osd":0,"up":0}]}`,
	} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "ceph"), []byte("#!/bin/sh\nprintf '%s' \"$FENCE_FIXTURE_MAP\"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			t.Setenv("FENCE_FIXTURE_MAP", state)
			if err := NewManager(t.TempDir()).waitCephFSBlocklists(CephConfig{}, "v1:127.0.0.1:0/99"); err == nil {
				t.Fatal("accepted unavailable committed fence evidence")
			}
		})
	}
}
