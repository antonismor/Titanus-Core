//go:build linux

package route

import (
	"encoding/json"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestNativeGatewayVIP(t *testing.T) {
	if os.Getenv("TITANUS_GATEWAY_VIP_TEST") != "1" {
		t.Skip("requires explicit native network namespace fixture")
	}
	if os.Geteuid() != 0 {
		t.Fatal("native VIP fixture requires root")
	}
	if root := os.Getenv("TITANUS_VIP_CHILD_ROOT"); root != "" {
		node := os.Getenv("TITANUS_VIP_CHILD_NODE")
		mode := os.Getenv("TITANUS_VIP_CHILD_MODE")
		read := func() (realm.State, error) {
			var s realm.State
			data, err := os.ReadFile(filepath.Join(root, "state.json"))
			if err != nil {
				return s, err
			}
			err = json.Unmarshal(data, &s)
			return s, err
		}
		m := NewManagerWithStateProvider(read)
		if err := m.ConfigureVIP(filepath.Join(root, node), node, LinuxVIP{}); err != nil {
			t.Fatal(err)
		}
		defer m.Close()
		if err := m.Reconcile(); err != nil {
			t.Fatal(err)
		}
		state, err := read()
		if err != nil {
			t.Fatal(err)
		}
		g := state.Gateways["edge"]
		present, err := (LinuxVIP{}).Has(g)
		if err != nil {
			t.Fatal(err)
		}
		if mode == "retired" {
			if present {
				t.Fatal("stale snapshot revived retired VIP")
			}
			return
		}
		if !present {
			t.Fatal("kernel VIP absent")
		}
		if err = os.WriteFile(filepath.Join(root, node+"-ready"), []byte("ready"), 0600); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, err = os.Stat(filepath.Join(root, node+"-withdraw")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("native VIP command deadline")
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err = m.Withdraw(g); err != nil {
			t.Fatal(err)
		}
		if present, err = (LinuxVIP{}).Has(g); err != nil || present {
			t.Fatal("acknowledged retained kernel VIP", err)
		}
		return
	}
	root := t.TempDir()
	names := []string{fmt.Sprintf("titanus-vip-a-%d", os.Getpid()), fmt.Sprintf("titanus-vip-b-%d", os.Getpid())}
	command := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("isolated fixture ip: %v %s", err, out)
		}
	}
	for _, name := range names {
		command("netns", "add", name)
		t.Cleanup(func() { exec.Command("ip", "netns", "del", name).Run() })
		command("-n", name, "link", "set", "lo", "up")
	}
	store, err := realm.Open(filepath.Join(root, "realm"), "VIP-CI")
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []string{"a", "b"} {
		if err = store.UpsertNode(realm.Node{ID: node, State: realm.NodeReady, Capabilities: []model.Capability{model.CapabilityGateway}}); err != nil {
			t.Fatal(err)
		}
	}
	g := realm.Gateway{Name: "edge", VIP: "192.0.2.99/32", Device: "lo", Owner: "a", Standby: "b"}
	if err = store.PutGateway(g); err != nil {
		t.Fatal(err)
	}
	writeState := func() {
		t.Helper()
		if err := durable.WriteJSON(filepath.Join(root, "state.json"), store.Snapshot(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeState()
	start := func(index int, node, mode string) *exec.Cmd {
		t.Helper()
		cmd := exec.Command("ip", "netns", "exec", names[index], os.Args[0], "-test.run=^TestNativeGatewayVIP$", "-test.v")
		cmd.Env = append(os.Environ(), "TITANUS_VIP_CHILD_ROOT="+root, "TITANUS_VIP_CHILD_NODE="+node, "TITANUS_VIP_CHILD_MODE="+mode)
		output, err := os.OpenFile(filepath.Join(root, node+"-"+mode+".log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout = output
		cmd.Stderr = output
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		output.Close()
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		return cmd
	}
	ready := func(node string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(root, node+"-ready")); err == nil {
				return
			}
			if time.Now().After(deadline) {
				data, _ := os.ReadFile(filepath.Join(root, node+"-active.log"))
				t.Fatalf("VIP child readiness: %s", data)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	withdraw := func(node string, cmd *exec.Cmd) {
		t.Helper()
		os.WriteFile(filepath.Join(root, node+"-withdraw"), nil, 0600)
		if err := cmd.Wait(); err != nil {
			data, _ := os.ReadFile(filepath.Join(root, node+"-active.log"))
			t.Fatalf("VIP kernel withdrawal: %v %s", err, data)
		}
	}
	old := start(0, "a", "active")
	ready("a")
	intent, err := store.BeginGatewayTransfer("edge", "b")
	if err != nil {
		t.Fatal(err)
	}
	if store.Snapshot().Gateways["edge"].Owner != "a" {
		t.Fatal("intent transferred before fence")
	}
	withdraw("a", old)
	if err = store.CompleteGatewayTransfer(intent, false); err != nil {
		t.Fatal(err)
	}
	stale := start(0, "a", "retired")
	if err = stale.Wait(); err != nil {
		t.Fatal("retired epoch revived", err)
	}
	writeState()
	next := start(1, "b", "active")
	ready("b")
	withdraw("b", next)
	t.Log("TITANUS_NATIVE_GATEWAY_VIP_HANDOFF_OK")
}
