package fencing

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProxmoxSetupVerifiesTrustIdentityAndPowerPrivilegesWithoutPowerCommands(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("private credential files require root")
	}
	for _, scenario := range []string{"ready", "uuid-mismatch", "missing-power", "untrusted-ca"} {
		t.Run(scenario, func(t *testing.T) {
			uuid := "01234567-89ab-cdef-0123-456789abcdef"
			calls := 0
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet {
					t.Error("setup sent a power/mutating request", r.Method, r.URL)
					w.WriteHeader(405)
					return
				}
				if r.Header.Get("Authorization") != "PVEAPIToken=installer@pve!worker=fake-test-secret" {
					t.Error("incorrect token identity")
				}
				data := any(nil)
				switch r.URL.Path {
				case "/api2/json/nodes/pve/qemu/4003/config":
					identity := uuid
					if scenario == "uuid-mismatch" {
						identity = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
					}
					data = map[string]string{"smbios1": "uuid=" + identity}
				case "/api2/json/nodes/pve/qemu/4003/status/current":
					data = map[string]string{"status": "running"}
				case "/api2/json/access/permissions":
					if r.URL.Query().Get("path") != "/vms/4003" {
						t.Error("wrong permission scope")
					}
					privs := map[string]int{"VM.Audit": 0}
					if scenario != "missing-power" {
						privs["VM.PowerMgmt"] = 0
					}
					data = map[string]any{"/vms/4003": privs}
				default:
					t.Error("unexpected API path", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"data": data})
			}))
			defer server.Close()
			ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
			if scenario == "untrusted-ca" {
				ca = []byte("invalid CA")
			}
			input := ProxmoxSetup{Address: server.URL, Hypervisor: "pve", User: "installer@pve!worker", Secret: "fake-test-secret", CAPEM: string(ca), VMIDs: map[string]int{"worker": 4003}}
			root := t.TempDir()
			os.Chmod(root, 0700)
			targets, err := ConfigureProxmox(root, input, map[string]string{"worker": uuid})
			if scenario == "ready" {
				if err != nil {
					t.Fatal(err)
				}
				if calls != 3 {
					t.Fatal("expected three read-only checks", calls)
				}
				for _, p := range []string{targets["worker"].CredentialFile, targets["worker"].CAFile} {
					st, e := os.Stat(p)
					if e != nil || st.Mode().Perm() != 0600 {
						t.Fatal("credentials not private", e)
					}
				}
				serialized, _ := json.Marshal(targets)
				if strings.Contains(string(serialized), input.Secret) {
					t.Fatal("token serialized into Plan")
				}
			} else {
				if err == nil {
					t.Fatal("failed readiness accepted")
				}
				if strings.Contains(fmt.Sprint(err), input.Secret) {
					t.Fatal("error leaked credential")
				}
				files, _ := filepath.Glob(filepath.Join(root, "*"))
				if len(files) != 0 {
					t.Fatal("failed setup retained credentials", files)
				}
			}
		})
	}
}
