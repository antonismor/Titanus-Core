package disk

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetachStaleRBDMappingAfterRebootFailsClosedOnAmbiguity(t *testing.T) {
	cases := []struct {
		name, output string
		ok, unmap    bool
	}{
		{"absent", "[]", true, false},
		{"current", `[{"pool":"titanus","name":"data","device":"/dev/rbd0"}]`, true, true},
		{"current-with-retained-fenced-mapping", `[{"pool":"titanus","name":"data","device":"/dev/rbd1"},{"pool":"titanus","name":"data","device":"/dev/rbd0"}]`, true, true},
		{"unknown", "null", false, false},
		{"malformed", "[] diagnostic", false, false},
		{"foreign-device", `[{"pool":"titanus","name":"other","device":"/dev/rbd0"}]`, false, false},
		{"different-device", `[{"pool":"titanus","name":"data","device":"/dev/rbd1"}]`, false, false},
		{"foreign-namespace", `[{"pool":"titanus","namespace":"other","name":"data","device":"/dev/rbd0"}]`, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "calls")
			script := `#!/bin/sh
case "$*" in
 *"device list --format json") printf '%s' "$RBD_MAPPING_FIXTURE" ;;
 *"unmap /dev/rbd0") printf '%s\n' unmap >> "$RBD_MAPPING_CALLS" ;;
 *) printf '%s\n' unexpected >> "$RBD_MAPPING_CALLS"; exit 2 ;;
esac
`
			if e := os.WriteFile(filepath.Join(dir, "rbd"), []byte(script), 0700); e != nil {
				t.Fatal(e)
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			t.Setenv("RBD_MAPPING_FIXTURE", tc.output)
			t.Setenv("RBD_MAPPING_CALLS", calls)
			m := NewManager(t.TempDir())
			if e := m.ConfigureCeph(CephConfig{}); e != nil {
				t.Fatal(e)
			}
			spec := Spec{Name: "data", Provider: ProviderCephRBD, Initialized: true, LayoutVersion: 1, ManagedID: "retained"}
			if e := os.MkdirAll(m.diskDir("data"), 0700); e != nil {
				t.Fatal(e)
			}
			if e := writeJSON(m.specPath("data"), spec, 0600); e != nil {
				t.Fatal(e)
			}
			if e := os.WriteFile(m.devicePath("data"), []byte("/dev/rbd0\n"), 0600); e != nil {
				t.Fatal(e)
			}
			w := Writer{Disk: "data", CatalogID: "retained", Address: "v1:192.0.2.1:0/99"}
			if e := writeJSON(m.writerPath("data"), w, 0600); e != nil {
				t.Fatal(e)
			}
			e := m.detach("data")
			if (e == nil) != tc.ok {
				t.Fatal("unexpected detach result", e)
			}
			_, exists := os.Stat(m.devicePath("data"))
			if tc.ok && !os.IsNotExist(exists) {
				t.Fatal("stale local marker retained")
			}
			if !tc.ok && exists != nil {
				t.Fatal("denied detach lost its evidence")
			}
			out, _ := os.ReadFile(calls)
			want := ""
			if tc.unmap {
				want = "unmap\n"
			}
			if string(out) != want {
				t.Fatal("unexpected native mutation", string(out))
			}
			got, e := m.Writer("data")
			if e != nil || got != w {
				t.Fatal("old writer evidence changed", got, e)
			}
			if strings.Contains(string(out), "map ") {
				t.Fatal("detach reacquired a native mapping")
			}
		})
	}
}
