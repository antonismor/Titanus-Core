package localclient

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFollowerRealmReadUsesVerifiedLeaderAndDoesNotForwardWrites(t *testing.T) {
	dir := t.TempDir()
	writes := 0
	leader := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
			http.Error(w, "write denied", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/v1/realm/state":
			_, _ = w.Write([]byte("{\"name\":\"TITANUS-LAB\"}"))
		case "/v1/realm/fleets":
			_, _ = w.Write([]byte("[]"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer leader.Close()

	tlsCert := leader.TLS.Certificates[0]
	cert := pem.EncodeToMemory(&pem.Block{Type:"CERTIFICATE",Bytes:tlsCert.Certificate[0]})
	key, err := x509.MarshalPKCS8PrivateKey(tlsCert.PrivateKey)
	if err != nil { t.Fatal(err) }
	private := pem.EncodeToMemory(&pem.Block{Type:"PRIVATE KEY",Bytes:key})
	for name, content := range map[string][]byte{
		"ca.crt":cert, "node.crt":cert, "node.key":private,
	} {
		if err := os.WriteFile(filepath.Join(dir,name),content,0600); err != nil { t.Fatal(err) }
	}
	conf, err := json.Marshal(map[string]any{"peers":[]map[string]string{{"id":"ctl02","api":leader.URL}}})
	if err != nil { t.Fatal(err) }
	if err := os.WriteFile(filepath.Join(dir,"ha.json"),conf,0600); err != nil { t.Fatal(err) }
	for k,p := range map[string]string{
		"TITANUS_HA_CONFIG":"ha.json","TITANUS_CA":"ca.crt",
		"TITANUS_CERT":"node.crt","TITANUS_KEY":"node.key",
	} { t.Setenv(k,filepath.Join(dir,p)) }

	socket := filepath.Join(dir,"control.sock")
	listener, err := net.Listen("unix",socket)
	if err != nil { t.Fatal(err) }
	local := &http.Server{Handler:http.HandlerFunc(func(w http.ResponseWriter,r *http.Request) {
		if r.URL.Path == "/v1/realm/consensus" {
			_, _ = w.Write([]byte("{\"leader_api\":\""+leader.URL+"\"}"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("{\"error\":\"Realm quorum/leader unavailable\"}"))
	})}
	defer local.Close()
	go func(){ _ = local.Serve(listener) }()

	c := New(socket)
	state, err := c.RealmState()
	if err != nil || state.Name != "TITANUS-LAB" {
		t.Fatalf("follower Realm state: %q, %v",state.Name,err)
	}
	fleets, err := c.ListFleets()
	if err != nil || len(fleets)!=0 { t.Fatalf("follower fleets: %v %v",fleets,err) }
	_, err = c.Orchestration(http.MethodPost,"/v1/realm/tasks",map[string]any{})
	if err == nil || !strings.Contains(err.Error(),"quorum/leader") {
		t.Fatalf("write should fail closed: %v",err)
	}
	if writes!=0 { t.Fatalf("write unexpectedly forwarded: %d",writes) }
}
