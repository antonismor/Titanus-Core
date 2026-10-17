package disk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCephFSFenceAndRefreshUseCLIInstanceAddress(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$*" in
 *" fsid") printf '%s' 'lab-fsid' ;;
 *"fs subvolume getpath"*) printf '%s' '/volumes/data/uuid' ;;
 *"fs get"*) printf '%s' '{"mdsmap":{"max_mds":1}}' ;;
 *"config get mds_session_blocklist_on_evict"*) printf '%s' '{"mds_session_blocklist_on_evict":"true"}' ;;
 *"config get mds_session_blocklist_on_timeout"*) printf '%s' '{"mds_session_blocklist_on_timeout":"true"}' ;;
 *"client ls"*) printf '%s' '[{"id":9,"inst":"client.9 v1:192.0.2.1:0/99","client_metadata":{"root":"/volumes/data/uuid"}}]' ;;
 *"osd blocklist add"*)
  while [ "$#" -gt 0 ] && [ "$1" != add ]; do shift; done
  shift
  # Reef CephEntityAddr accepts IP:port/nonce, not the MDS v1: prefix.
  [ "$1" = '192.0.2.1:0/99' ] || exit 22
  printf '%s\n' "$1" >> "$FENCE_CLI_CALLS" ;;
 *"osd dump"*) printf '%s' '{"blocklist":{"192.0.2.1:0/99":"expiry"},"osds":[{"osd":0,"up":1}]}' ;;
 *"tell osd.0 dump_blocklist"*) printf '%s' '[{"entity_addr_t":{"addr":"192.0.2.1:0","nonce":99}}][]' ;;
 *"client evict id=9"*) exit 0 ;;
 *) exit 2 ;;
esac
`
	if e := os.WriteFile(filepath.Join(dir, "ceph"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	calls := filepath.Join(dir, "calls")
	t.Setenv("FENCE_CLI_CALLS", calls)
	m := NewManager(t.TempDir())
	if e := m.ConfigureCeph(CephConfig{}); e != nil {
		t.Fatal(e)
	}
	c := Catalog{Spec: Spec{Name: "data", Provider: ProviderCephFS, SizeBytes: 1 << 20, Initialized: true, LayoutVersion: 1, RemotePath: "/volumes/data/uuid"}, Backend: Backend{FSID: "lab-fsid", Filesystem: "cephfs", Object: "/volumes/data/uuid"}, Fleet: "db", AutoFailover: true}
	w := Writer{Disk: "data", CatalogID: c.ID(), Address: "v1:192.0.2.1:0/99", Session: 9}
	gates := 0
	check := func() error { gates++; return nil }
	if e := m.Fence(c, w, check); e != nil {
		t.Fatal("initial native fence", e)
	}
	if e := m.RefreshFence(c, w, check); e != nil {
		t.Fatal("retained native fence", e)
	}
	out, e := os.ReadFile(calls)
	if e != nil {
		t.Fatal(e)
	}
	if string(out) != strings.Repeat("192.0.2.1:0/99\n", 3) {
		t.Fatal("initial fence, post-eviction retention and refresh did not retain exact nonce", string(out))
	}
	if w.Address != "v1:192.0.2.1:0/99" || gates < 5 {
		t.Fatal("stored identity or quorum gates changed")
	}
}
