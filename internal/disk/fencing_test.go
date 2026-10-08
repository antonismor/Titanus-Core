package disk

import "testing"

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
