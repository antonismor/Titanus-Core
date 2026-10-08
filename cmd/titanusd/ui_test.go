package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/secrets"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeCommandCenter(t *testing.T) {
	if os.Getenv("TITANUS_COMMAND_CENTER_TEST") != "1" {
		t.Skip("requires built daemon, browser and native Unix socket")
	}
	binary := os.Getenv("TITANUS_DAEMON_BINARY")
	if binary == "" {
		t.Fatal("daemon binary required")
	}
	root := t.TempDir()
	a, e := identity.InitAuthority(filepath.Join(root, "pki"), "LAB")
	if e != nil {
		t.Fatal(e)
	}
	cert, key, e := a.Issue("server", []string{"127.0.0.1"}, identity.RoleController, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	adminCert, adminKey, e := a.Issue("operator", nil, identity.RoleAdmin, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	stateRoot := filepath.Join(root, "state")
	s, e := realm.Open(stateRoot, "LAB")
	if e != nil {
		t.Fatal(e)
	}
	s.UpsertNode(realm.Node{ID: "execution-01", State: realm.NodeReady, Capabilities: []model.Capability{model.CapabilityExecution}, Resources: realm.Resources{MemoryBytes: 16 << 30, MemoryUsedBytes: 2 << 30, CPUMilliCapacity: 4000}})
	s.PutFleet(realm.Fleet{Name: "api", Instances: 2, MinimumAvailable: 1, Template: realm.UnitTemplate{Source: "app", Command: []string{"app"}, CPUPercent: 50}})
	keys := secrets.Keyring{Active: "ui-key", Keys: map[string][]byte{"ui-key": bytes.Repeat([]byte{3}, 32)}}
	raw, _ := json.Marshal(keys)
	keyPath := filepath.Join(root, "secret-keys.json")
	os.WriteFile(keyPath, raw, 0600)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	address := listener.Addr().String()
	listener.Close()
	origin := "https://" + address
	logPath := filepath.Join(root, "daemon.log")
	logFile, e := os.Create(logPath)
	if e != nil {
		t.Fatal(e)
	}
	defer logFile.Close()
	cmd := exec.Command(binary)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TITANUS_STATE_ROOT=" + stateRoot, "TITANUS_REALM_NAME=LAB", "TITANUS_SOCKET=" + filepath.Join(root, "titanus.sock"), "TITANUS_CLUSTER_LISTEN=" + address, "TITANUS_CA=" + a.CertPath, "TITANUS_CERT=" + cert, "TITANUS_KEY=" + key, "TITANUS_SECRET_KEYRING=" + keyPath}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Log(string(data))
		}
	}()
	cfg, e := identity.TLSConfig(a.CertPath, adminCert, adminKey, false)
	if e != nil {
		t.Fatal(e)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: time.Second}
	ready := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		r, e := client.Get(origin + "/v1/realm/state")
		if e == nil {
			r.Body.Close()
			if r.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatal("daemon did not start")
	}
	script := exec.Command("node", os.Getenv("TITANUS_UI_SCRIPT"))
	script.Env = append(os.Environ(), "TITANUS_UI_ORIGIN="+origin, "TITANUS_UI_CERT="+adminCert, "TITANUS_UI_KEY="+adminKey)
	output, e := script.CombinedOutput()
	if e != nil {
		t.Fatalf("browser UI/API check: %v\n%s", e, output)
	}
	t.Log(string(output))
	r, e := client.Get(origin + "/v1/realm/tasks")
	if e != nil {
		t.Fatal(e)
	}
	var tasks []realm.Task
	e = json.NewDecoder(r.Body).Decode(&tasks)
	r.Body.Close()
	if e != nil || len(tasks) != 1 || !tasks[0].CancelRequested {
		t.Fatal("UI did not update real Task state")
	}
	raw, e = os.ReadFile(filepath.Join(stateRoot, "realm", "state.json"))
	if e != nil || bytes.Contains(raw, []byte("BROWSER_SECRET_CANARY")) {
		t.Fatal("browser submitted secret persisted as plaintext")
	}
	anonymous := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: time.Second}
	r, e = anonymous.Get(origin + "/command-center")
	if e == nil {
		defer r.Body.Close()
		io.Copy(io.Discard, r.Body)
		if r.StatusCode == 200 {
			t.Fatal("anonymous browser gained access")
		}
	}
}
