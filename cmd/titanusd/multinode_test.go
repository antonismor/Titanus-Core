package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/consensus"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Five isolated network endpoints and independent node state roots use the real
// daemon, agents, mTLS transport, Source transfer and kernel runtime. They share
// a CI kernel; this is not represented as physical-host/reboot acceptance.
func TestNativeMultiNodeFailure(t *testing.T) {
	if os.Getenv("TITANUS_MULTINODE_TEST") != "1" {
		t.Skip("requires privileged network namespaces and native binaries")
	}
	if os.Geteuid() != 0 {
		t.Fatal("root required")
	}
	root := t.TempDir()
	bin := os.Getenv("TITANUS_BIN_DIR")
	if !filepath.IsAbs(bin) {
		t.Fatal("absolute native binary directory required")
	}
	authority, e := identity.InitAuthority(filepath.Join(root, "authority"), "MULTI-CI")
	if e != nil {
		t.Fatal(e)
	}
	run := func(args ...string) {
		t.Helper()
		out, e := exec.Command(args[0], args[1:]...).CombinedOutput()
		if e != nil {
			t.Fatalf("%v: %s %v", args, out, e)
		}
	}
	bridge := "tn-ci-br"
	run("ip", "link", "add", bridge, "type", "bridge")
	run("ip", "addr", "add", "10.248.0.1/24", "dev", bridge)
	run("ip", "link", "set", bridge, "up")
	names, ips, roots, certs, keys, pkis, logs := [5]string{}, [5]string{}, [5]string{}, [5]string{}, [5]string{}, [5]string{}, [5]string{}
	daemons := [5]*exec.Cmd{}
	agents := [2]*exec.Cmd{}
	stop := func(cmd **exec.Cmd) {
		if *cmd != nil {
			(*cmd).Process.Kill()
			(*cmd).Wait()
			*cmd = nil
		}
	}
	t.Cleanup(func() {
		if t.Failed() {
			for i := 3; i < 5; i++ {
				for _, args := range [][]string{{"nft", "list", "table", "ip", "titanus_nat"}, {"ip", "route"}} {
					out, err := exec.Command("nsenter", append([]string{"--net=" + filepath.Join("/run/netns", names[i]), "--"}, args...)...).CombinedOutput()
					t.Logf("node %d %v: %s (%v)", i, args, out, err)
				}
				for _, name := range []string{"agent.log", "fabric/services.json", "fabric/allocations.json"} {
					b, _ := os.ReadFile(filepath.Join(roots[i], name))
					t.Logf("node %d %s: %s", i, name, b)
				}
			}
		}
		for i := 3; i < 5; i++ {
			m := unitruntime.NewManager(unitruntime.Config{StateRoot: roots[i], CgroupRoot: fmt.Sprintf("/sys/fs/cgroup/titanus-multi-%d", i), InitBinary: filepath.Join(bin, "titanus-init")})
			items, _ := m.List()
			for _, u := range items {
				m.Stop(u.ID, time.Second)
			}
		}
		for i := range agents {
			stop(&agents[i])
		}
		for i := range daemons {
			stop(&daemons[i])
		}
		for _, ns := range names {
			if ns != "" {
				exec.Command("ip", "netns", "delete", ns).Run()
			}
		}
		exec.Command("ip", "link", "delete", bridge).Run()
		if t.Failed() {
			for _, p := range logs {
				b, _ := os.ReadFile(p)
				t.Logf("%s:\n%s", p, b)
			}
		}
	})
	peers := []consensus.Peer{}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("node-%d", i)
		names[i] = "tn-ci-" + id
		ips[i] = fmt.Sprintf("10.248.0.%d", 10+i)
		roots[i] = filepath.Join(root, id)
		os.MkdirAll(roots[i], 0700)
		pkis[i] = filepath.Join(roots[i], "pki")
		os.MkdirAll(pkis[i], 0700)
		logs[i] = filepath.Join(roots[i], "daemon.log")
		run("ip", "netns", "add", names[i])
		host, guest := fmt.Sprintf("tnh%d", i), fmt.Sprintf("tng%d", i)
		run("ip", "link", "add", host, "type", "veth", "peer", "name", guest)
		run("ip", "link", "set", guest, "netns", names[i])
		run("ip", "link", "set", host, "master", bridge)
		run("ip", "link", "set", host, "up")
		run("ip", "-n", names[i], "link", "set", guest, "name", "eth0")
		run("ip", "-n", names[i], "addr", "add", ips[i]+"/24", "dev", "eth0")
		run("ip", "-n", names[i], "link", "set", "eth0", "up")
		run("ip", "-n", names[i], "link", "set", "lo", "up")
		run("ip", "-n", names[i], "route", "add", "default", "via", "10.248.0.1")
		// ip netns exec also creates a mount namespace and remounts sysfs,
		// hiding the native cgroup mount. Enter only the network namespace;
		// every fixture command must retain the host cgroups v2 hierarchy.
		run("nsenter", "--net="+filepath.Join("/run/netns", names[i]), "--", "test", "-r", "/sys/fs/cgroup/cgroup.controllers")
		role := identity.RoleNode
		if i < 3 {
			role = identity.RoleController
			peers = append(peers, consensus.Peer{ID: id, Address: ips[i] + ":9444", API: "https://" + ips[i] + ":9443"})
		}
		certs[i], keys[i], e = authority.Issue(id, []string{ips[i]}, role, time.Hour)
		if e != nil {
			t.Fatal(e)
		}
		for _, name := range []string{"ca.crt", "ca.crl"} {
			b, e := os.ReadFile(filepath.Join(authority.Dir, name))
			if e != nil {
				t.Fatal(e)
			}
			os.WriteFile(filepath.Join(pkis[i], name), b, 0600)
		}
		if i < 3 {
			s, e := realm.Open(roots[i], "MULTI-CI")
			if e != nil {
				t.Fatal(e)
			}
			if e = s.ConfigureNetwork(realm.RealmNetwork{FabricCIDR: "10.246.0.0/16", ServiceCIDR: "10.247.0.0/16", NodePrefix: 24, VXLANID: 4243}); e != nil {
				t.Fatal(e)
			}
			if e = source.NewManager(roots[i]).ImportDirectory("app", "/tmp/titanus-rootfs"); e != nil {
				t.Fatal(e)
			}
		}
	}
	for i := 0; i < 3; i++ {
		b, _ := json.Marshal(consensus.Config{ID: peers[i].ID, Realm: "MULTI-CI", Peers: peers, Bootstrap: i == 0})
		os.WriteFile(filepath.Join(roots[i], "ha.json"), b, 0600)
	}
	start := func(i int) {
		t.Helper()
		file, e := os.OpenFile(logs[i], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		cmd := exec.Command("nsenter", "--net="+filepath.Join("/run/netns", names[i]), "--", filepath.Join(bin, "titanusd"))
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TITANUS_STATE_ROOT=" + roots[i], "TITANUS_REALM_NAME=MULTI-CI", "TITANUS_NODE_ID=" + fmt.Sprintf("node-%d", i), "TITANUS_SOCKET=" + filepath.Join(roots[i], "daemon.sock"), "TITANUS_CLUSTER_LISTEN=" + ips[i] + ":9443", "TITANUS_CA=" + filepath.Join(pkis[i], "ca.crt"), "TITANUS_CERT=" + certs[i], "TITANUS_KEY=" + keys[i], "TITANUS_INIT_BINARY=" + filepath.Join(bin, "titanus-init"), "TITANUS_CGROUP_ROOT=" + fmt.Sprintf("/sys/fs/cgroup/titanus-multi-%d", i)}
		if i < 3 {
			cmd.Env = append(cmd.Env, "TITANUS_CONTROLLER_MODE=true", "TITANUS_HA_CONFIG="+filepath.Join(roots[i], "ha.json"))
		}
		cmd.Stdout = file
		cmd.Stderr = file
		e = cmd.Start()
		file.Close()
		if e != nil {
			t.Fatal(e)
		}
		daemons[i] = cmd
		mountNS, e := os.Readlink(fmt.Sprintf("/proc/%d/ns/mnt", cmd.Process.Pid))
		hostMountNS, hostErr := os.Readlink("/proc/self/ns/mnt")
		if e != nil || hostErr != nil || mountNS != hostMountNS {
			t.Fatalf("node %d did not preserve the native mount namespace: %q != %q (%v, %v)", i, mountNS, hostMountNS, e, hostErr)
		}
	}
	for i := 0; i < 5; i++ {
		start(i)
	}
	adminCert, adminKey, e := authority.Issue("operator", nil, identity.RoleAdmin, time.Hour)
	if e != nil {
		t.Fatal(e)
	}
	cfg, e := identity.TLSConfig(authority.CertPath, adminCert, adminKey, false)
	if e != nil {
		t.Fatal(e)
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg, DialContext: (&net.Dialer{Timeout: time.Second}).DialContext}, Timeout: 5 * time.Second}
	request := func(i int, method, path string, body any, out any) error {
		var reader io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			reader = bytes.NewReader(b)
		}
		r, e := http.NewRequest(method, "https://"+ips[i]+":9443"+path, reader)
		if e != nil {
			return e
		}
		r.Header.Set("Content-Type", "application/json")
		resp, e := client.Do(r)
		if e != nil {
			return e
		}
		defer resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			return fmt.Errorf("HTTP %d %s", resp.StatusCode, b)
		}
		if out != nil {
			return json.NewDecoder(resp.Body).Decode(out)
		}
		return nil
	}
	wait := func(duration time.Duration, condition func() bool) {
		t.Helper()
		for deadline := time.Now().Add(duration); time.Now().Before(deadline); {
			if condition() {
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
		t.Fatal("multi-node condition timed out")
	}
	leader := -1
	findLeader := func() bool {
		for i := 0; i < 3; i++ {
			if daemons[i] == nil {
				continue
			}
			var status map[string]string
			if request(i, "GET", "/v1/realm/consensus", nil, &status) == nil && status["state"] == "Leader" {
				var state realm.State
				if request(i, "GET", "/v1/realm/state", nil, &state) == nil {
					leader = i
					return true
				}
			}
		}
		return false
	}
	wait(20*time.Second, findLeader)
	origins := []string{}
	for _, p := range peers {
		origins = append(origins, p.API)
	}
	for i := 3; i < 5; i++ {
		file, e := os.Create(filepath.Join(roots[i], "agent.log"))
		if e != nil {
			t.Fatal(e)
		}
		cmd := exec.Command("nsenter", "--net="+filepath.Join("/run/netns", names[i]), "--", filepath.Join(bin, "titanus-agent"), "--node", fmt.Sprintf("node-%d", i), "--address", ips[i]+":9443", "--fabric-address", ips[i], "--controller", strings.Join(origins, ","), "--ca", filepath.Join(pkis[i], "ca.crt"), "--cert", certs[i], "--key", keys[i], "--state-root", roots[i], "--interval", "2s", "--capabilities", "EXECUTION", "rack="+fmt.Sprint(i))
		cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
		cmd.Stdout = file
		cmd.Stderr = file
		e = cmd.Start()
		file.Close()
		if e != nil {
			t.Fatal(e)
		}
		agents[i-3] = cmd
	}
	snapshot := func() realm.State {
		var s realm.State
		if e := request(leader, "GET", "/v1/realm/state", nil, &s); e != nil {
			findLeader()
		}
		return s
	}
	wait(20*time.Second, func() bool { return len(snapshot().Nodes) == 2 })
	fleet := realm.Fleet{Name: "web", Instances: 2, MinimumAvailable: 1, MaxSurge: 1, SpreadLabel: "rack", Template: realm.UnitTemplate{Source: "app", Command: []string{"/bin/rollout-app", "v1"}, MemoryBytes: 64 << 20, CPUPercent: 50, PidsMax: 128, Fabric: true, Health: unitruntime.Health{Readiness: &unitruntime.Probe{Protocol: "http", Port: 8080, Path: "/ready", IntervalSeconds: 1, FailureThreshold: 1}}}}
	if e = request(leader, "POST", "/v1/realm/fleets", fleet, nil); e != nil {
		t.Fatal(e)
	}
	complete := func(g uint64) bool {
		s := snapshot()
		f, ok := s.Fleets["web"]
		return ok && f.Generation == g && realm.NewPlacementEngine().Rolling(s, f, time.Now()).Complete
	}
	wait(45*time.Second, func() bool { return complete(1) })
	s := snapshot()
	used := map[string]bool{}
	for _, a := range s.Assignments {
		used[a.NodeID] = true
	}
	if len(used) != 2 {
		t.Fatal("workload did not cross two execution nodes")
	}
	// Pin the destination to the OTHER worker. Service load balancing alone
	// could select a local backend and would not prove VXLAN transport.
	remoteBackend := func(version string) bool {
		for _, a := range snapshot().Assignments {
			if a.NodeID == "node-3" && a.State == realm.AssignmentActive && a.NetworkAddress != "" {
				out, e := exec.Command("nsenter", "--net="+filepath.Join("/run/netns", names[4]), "--", "curl", "-f", "-s", "--max-time", "2", "http://"+a.NetworkAddress+":8080/ready").CombinedOutput()
				if e == nil && string(out) == version {
					return true
				}
			}
		}
		return false
	}
	wait(20*time.Second, func() bool { return remoteBackend("v1") })
	route := realm.Route{Name: "web", Fleet: "web", ListenPort: 18080, TargetPort: 8080}
	var stored realm.Route
	if e = request(leader, "POST", "/v1/realm/routes", route, &stored); e != nil {
		t.Fatal(e)
	}
	var serviceFailure string
	service := func(version string) bool {
		out, e := exec.Command("nsenter", "--net="+filepath.Join("/run/netns", names[4]), "--", "curl", "-f", "-s", "--max-time", "2", "http://"+stored.ServiceIP+":18080/ready").CombinedOutput()
		serviceFailure = fmt.Sprintf("Service %s expected %s: %q (%v)", stored.ServiceIP, version, out, e)
		return e == nil && string(out) == version
	}
	t.Cleanup(func() { if t.Failed() { t.Log(serviceFailure) } })
	wait(20*time.Second, func() bool { return service("v1") })
	fleet.Template.Command = []string{"/bin/rollout-app", "v2"}
	if e = request(leader, "POST", "/v1/realm/fleets", fleet, nil); e != nil {
		t.Fatal(e)
	}
	wait(50*time.Second, func() bool { return complete(2) })
	wait(20*time.Second, func() bool { return remoteBackend("v2") })
	wait(20*time.Second, func() bool { return service("v2") })
	fleet.Template.Command = []string{"/bin/rollout-app", "broken"}
	if e = request(leader, "POST", "/v1/realm/fleets", fleet, nil); e != nil {
		t.Fatal(e)
	}
	time.Sleep(8 * time.Second)
	s = snapshot()
	available := 0
	for _, a := range s.Assignments {
		if a.Generation == 2 && a.State == realm.AssignmentActive {
			available++
		}
	}
	if available < 1 {
		t.Fatal("real failed rollout violated minimum availability")
	}
	if e = request(leader, "POST", "/v1/realm/fleets/web/rollback", map[string]uint64{"generation": 2}, nil); e != nil {
		t.Fatal(e)
	}
	wait(50*time.Second, func() bool { return complete(4) })
	oldLeader := leader
	stop(&daemons[oldLeader])
	wait(20*time.Second, findLeader)
	if leader == oldLeader {
		t.Fatal("dead controller retained authority")
	}
	wait(20*time.Second, func() bool { return service("v2") })
	start(oldLeader)
	wait(20*time.Second, func() bool {
		var status map[string]string
		return request(oldLeader, "GET", "/v1/realm/consensus", nil, &status) == nil
	})
	// Cut the execution node's actual interface while its daemon/monitor still
	// run. Real Pulse expiry and kernel lease fencing must govern replacement.
	run("ip", "-n", names[3], "link", "set", "eth0", "down")
	m := unitruntime.NewManager(unitruntime.Config{StateRoot: roots[3], CgroupRoot: "/sys/fs/cgroup/titanus-multi-3", InitBinary: filepath.Join(bin, "titanus-init")})
	wait(40*time.Second, func() bool {
		items, e := m.List()
		if e != nil {
			return false
		}
		for _, u := range items {
			if u.Status == unitruntime.StatusActive {
				return false
			}
		}
		return len(items) > 0
	})
	wait(120*time.Second, func() bool { s := snapshot(); return s.Nodes["node-3"].State == realm.NodeUnreachable && complete(4) })
	if !service("v2") {
		t.Fatal("survivor Service Fabric lost availability")
	}
	s = snapshot()
	for _, a := range s.Assignments {
		if a.NodeID != "node-4" {
			t.Fatal("unreachable node retained stateless assignment")
		}
	}
	run("ip", "-n", names[3], "link", "set", "eth0", "up")
	wait(30*time.Second, func() bool { return snapshot().Nodes["node-3"].State == realm.NodeReady })
	t.Log("TITANUS_NATIVE_MULTINODE_FAILURE_ROLLBACK_VXLAN_OK")
}
