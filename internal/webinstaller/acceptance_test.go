package webinstaller

import (
	"context"
	cryptotls "crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/controlapi"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/remote"
	"github.com/antonismor/Titanus-Core/internal/version"
)

type acceptanceFixture struct {
	plan    model.RealmPlan
	failure string
	calls   int
}

func acceptancePlan() model.RealmPlan {
	p := validPlan()
	p.Nodes = nil
	for i := 0; i < 4; i++ {
		cap := model.CapabilityControl
		if i == 3 {
			cap = model.CapabilityExecution
		}
		p.Nodes = append(p.Nodes, model.NodeSpec{Name: fmt.Sprintf("node%d", i), ManagementIP: fmt.Sprintf("192.0.2.%d", 10+i), SSHUser: "root", SSHPort: 22, Capabilities: []model.Capability{cap}})
	}
	return p
}

func (f *acceptanceFixture) Run(node model.NodeSpec, command string) (remote.Result, error) {
	f.calls++
	if f.failure == "transport" {
		return remote.Result{Stderr: "PRIVATE_SECRET_CANARY"}, fmt.Errorf("PRIVATE_SECRET_CANARY")
	}
	encode := func(value any) (remote.Result, error) {
		data, _ := json.Marshal(value)
		return remote.Result{Stdout: string(data)}, nil
	}
	if strings.HasPrefix(command, "systemctl") {
		if f.failure == "services" && node.Name == "node3" {
			return remote.Result{Stdout: "active\ninactive"}, fmt.Errorf("inactive")
		}
		return remote.Result{Stdout: "active\nactive"}, nil
	}
	if strings.Contains(command, "ceph status --format json") {
		health := "HEALTH_OK"
		if f.failure == "ceph-health" {
			health = "HEALTH_WARN"
		}
		return remote.Result{Stdout: fmt.Sprintf(`{"health":{"status":%q},"pgmap":{"num_pgs":8,"pgs_by_state":[{"state_name":"active+clean","count":8}]}}`, health)}, nil
	}
	if strings.Contains(command, "ceph osd stat --format json") {
		count := 2
		if f.failure == "ceph-osds" {
			count = 1
		}
		return remote.Result{Stdout: fmt.Sprintf(`{"num_osds":%d,"num_up_osds":%d,"num_in_osds":%d}`, count, count, count)}, nil
	}
	endpoint := ""
	last := -1
	for _, p := range []string{"/v1/compatibility", "/v1/realm/state", "/v1/realm/consensus"} {
		if index := strings.LastIndex(command, p); index > last {
			last = index
			endpoint = p
		}
	}
	switch endpoint {
	case "/v1/compatibility":
		for _, target := range f.plan.Nodes {
			if strings.Contains(command, target.ManagementIP) {
				node = target
				break
			}
		}
		caps := version.Compatible()
		caps.Build.Version = f.plan.Installation.ReleaseVersion
		caps.Build.Revision = f.plan.Installation.Revision
		caps.Build.Arch = "amd64"
		if f.failure == "revision" && node.Name == "node3" {
			caps.Build.Revision = strings.Repeat("f", 40)
		}
		return encode(controlapi.CompatibilityInfo{NodeID: node.Name, Realm: f.plan.RealmName, Schema: version.MaxSchema, Capabilities: caps})
	case "/v1/realm/state":
		state := realm.State{Name: f.plan.RealmName, Network: realm.RealmNetwork{FabricCIDR: f.plan.FabricCIDR, ServiceCIDR: f.plan.ServiceCIDR, NodePrefix: f.plan.Installation.NodePrefix, VXLANID: f.plan.Installation.VXLANID}, Nodes: map[string]realm.Node{}}
		for _, n := range f.plan.Nodes {
			state.Nodes[n.Name] = realm.Node{ID: n.Name, Address: net.JoinHostPort(n.ManagementIP, fmt.Sprint(f.plan.Installation.APIPort)), State: realm.NodeReady, Capabilities: n.Capabilities, LastPulse: time.Now().UTC()}
		}
		n := state.Nodes["node3"]
		switch f.failure {
		case "stale":
			n.LastPulse = time.Now().Add(-time.Hour)
		case "roles":
			n.Capabilities = []model.Capability{model.CapabilityStorage}
		case "quarantined":
			n.PowerQuarantined = true
		case "fabric":
			state.Network.ServiceCIDR = "10.240.0.0/16"
		}
		state.Nodes["node3"] = n
		return encode(state)
	case "/v1/realm/consensus":
		state := "Follower"
		if node.Name == "node0" {
			state = "Leader"
		}
		leader := "https://192.0.2.10:9443"
		if f.failure == "split-leader" && node.Name == "node1" {
			state = "Leader"
			leader = "https://192.0.2.11:9443"
		}
		return encode(map[string]string{"state": state, "id": node.Name, "realm": f.plan.RealmName, "leader_api": leader})
	}
	return remote.Result{}, fmt.Errorf("unexpected command: %s", command)
}

func TestAcceptanceRequiresEveryNodeAndFullVerifiedCluster(t *testing.T) {
	for _, failure := range []string{"", "services", "revision", "stale", "roles", "quarantined", "fabric", "split-leader", "transport"} {
		t.Run(failure, func(t *testing.T) {
			plan := acceptancePlan()
			ssh := &acceptanceFixture{plan: plan, failure: failure}
			report := runClusterAcceptance(context.Background(), plan, ssh, newAcceptanceReport(plan, "session", "cluster"), 0, nil)
			if (report.Status == "Success") != (failure == "") || len(report.Checks) != 12 {
				t.Fatalf("unexpected report: %+v", report)
			}
			for _, check := range report.Checks {
				if check.Status == "Running" || check.Status == "Pending" || check.Attempts > 3 {
					t.Fatalf("unbounded/incomplete check: %+v", check)
				}
			}
			data, _ := json.Marshal(report)
			if strings.Contains(string(data), "PRIVATE_SECRET_CANARY") {
				t.Fatal("raw probe output leaked")
			}
		})
	}
}

func TestAcceptanceDeadlineFailsWithoutContactingHosts(t *testing.T) {
	plan := acceptancePlan()
	ssh := &acceptanceFixture{plan: plan}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := runClusterAcceptance(ctx, plan, ssh, newAcceptanceReport(plan, "session", "cluster"), 0, nil)
	if report.Status != "Failed" || ssh.calls != 0 {
		t.Fatal("canceled acceptance ran probes", report, ssh.calls)
	}
}

func TestCephAcceptanceRejectsUnhealthyOrMissingSelectedOSDs(t *testing.T) {
	for _, failure := range []string{"", "ceph-health", "ceph-osds"} {
		t.Run(failure, func(t *testing.T) {
			plan := acceptancePlan()
			plan.Nodes[3].Capabilities = []model.Capability{model.CapabilityStorage}
			plan.Nodes[3].CephDevices = []string{"/dev/disk/by-id/one", "/dev/disk/by-id/two"}
			plan.Ceph = model.CephSpec{Enabled: true, Provision: true, Replication: 1}
			report := runClusterAcceptance(context.Background(), plan, &acceptanceFixture{plan: plan, failure: failure}, newAcceptanceReport(plan, "session", "cluster"), 0, nil)
			if (report.Status == "Success") != (failure == "") || len(report.Checks) != 14 {
				t.Fatalf("invalid Ceph result: %+v", report)
			}
		})
	}
}

func TestFileOnlyInstallDoesNotClaimLiveClusterChecks(t *testing.T) {
	s := testServer(t)
	plan := validPlan()
	plan.Installation.Start = false
	ssh := &acceptanceFixture{plan: plan}
	s.runAcceptance(plan, ssh)
	if ssh.calls != 0 || s.state.Report.Scope != "files-only" || s.state.Report.Checks[0].Status != "Skipped" {
		t.Fatal("unrequested live acceptance", s.state)
	}
}

func TestFinalPersistenceFailureCannotLeaveSuccessfulReport(t *testing.T) {
	s := testServer(t)
	plan := validPlan()
	report := newAcceptanceReport(plan, "session", "cluster")
	report.Status = "Success"
	report.FinishedAt = time.Now().UTC()
	if err := os.Mkdir(filepath.Join(s.cfg.StateDir, "state.json"), 0700); err != nil {
		t.Fatal(err)
	}
	s.completeAcceptance(report)
	if s.state.Completed || s.state.Report.Status != "Failed" {
		t.Fatal("accepted unpersisted success", s.state)
	}
	data, err := os.ReadFile(filepath.Join(s.cfg.StateDir, "reports", report.ID+".json"))
	if err != nil || !strings.Contains(string(data), `"status": "Failed"`) {
		t.Fatal("on-disk report disagrees with failed state", err, string(data))
	}
}

func TestAcceptanceLaunchFailureRestoresCompletedState(t *testing.T) {
	s := testServer(t)
	plan := validPlan()
	requestJSON(t, s, http.MethodPost, "/api/v1/validate", plan)
	s.state.Status = "completed"
	s.state.Completed = true
	s.state.DeploymentPath = "fixture"
	dir := s.bootstrapFor(plan).Directory
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"id_ed25519", "known_hosts"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("private fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(s.cfg.StateDir, "state.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	res := requestJSON(t, s, http.MethodPost, "/api/v1/acceptance", nil)
	if res.Code != 500 || s.running || s.state.Status != "completed" || !s.state.Completed {
		t.Fatal("failed launch left a phantom active acceptance", res.Code, s.state)
	}
}

func TestStatusReportPersistsDownloadsAndKeepsCleanupReserved(t *testing.T) {
	s := testServer(t)
	plan := acceptancePlan()
	if res := requestJSON(t, s, http.MethodPost, "/api/v1/validate", plan); res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	report := runClusterAcceptance(context.Background(), plan, &acceptanceFixture{plan: plan}, newAcceptanceReport(plan, s.sessionID(), "cluster"), 0, nil)
	s.running = true
	s.state.Running = true
	s.completeAcceptance(report)
	if res := requestJSON(t, s, http.MethodPost, "/api/v1/quiesce", nil); res.Code != 409 {
		t.Fatal("completion released cleanup reservation")
	}
	s.finishRun(true)
	info, err := os.Stat(filepath.Join(s.cfg.StateDir, "reports", report.ID+".json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private report not saved", err)
	}
	restored, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	res := requestJSON(t, restored, http.MethodGet, "/api/v1/report", nil)
	if res.Code != 200 || !strings.Contains(res.Body.String(), `"status":"Success"`) {
		t.Fatal("report was lost after restart", res.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/report", nil)
	response := httptest.NewRecorder()
	s.Handler().ServeHTTP(response, request)
	if response.Code != 401 {
		t.Fatal("report exposed without authentication")
	}
}

func TestInterruptedAcceptanceCannotBecomeSuccessAndPrepareCannotRunIt(t *testing.T) {
	s := testServer(t)
	plan := validPlan()
	requestJSON(t, s, http.MethodPost, "/api/v1/validate", plan)
	report := newAcceptanceReport(plan, s.sessionID(), "cluster")
	s.state.Report = &report
	s.state.Status = "acceptance"
	s.state.Phase = "Complete"
	s.state.Running = true
	s.persistStateLocked()
	restored, err := New(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if restored.state.Report.Status != "Failed" || restored.state.Running || restored.state.Status != "interrupted" {
		t.Fatal("interrupted report accepted", restored.state)
	}
	for _, prepared := range []bool{true, false} {
		s.state.PreparedOnly = prepared
		s.state.Status = "completed"
		s.state.DeploymentPath = "fixture"
		if res := requestJSON(t, s, http.MethodPost, "/api/v1/acceptance", nil); res.Code != 409 {
			t.Fatal("acceptance admitted without saved trust or with prepare-only")
		}
	}
}

func TestCleanCephRequiresCompleteEvidence(t *testing.T) {
	for _, sample := range []struct {
		data string
		ok   bool
	}{
		{`{"health":{"status":"HEALTH_OK"},"pgmap":{"num_pgs":8,"pgs_by_state":[{"state_name":"active+clean","count":8}]}}`, true},
		{`{"health":{"status":"HEALTH_WARN"},"pgmap":{"num_pgs":8,"pgs_by_state":[{"state_name":"active+clean","count":8}]}}`, false},
		{`{"health":{"status":"HEALTH_OK"},"pgmap":{"num_pgs":8,"pgs_by_state":[{"state_name":"active+clean","count":7}]}}`, false},
		{`{"health":{"status":"HEALTH_OK"},"pgmap":{"num_pgs":8,"pgs_by_state":[{"state_name":"active+degraded","count":8}]}}`, false},
	} {
		if (validateCleanCeph([]byte(sample.data)) == nil) != sample.ok {
			t.Fatal(sample)
		}
	}
}

func TestAcceptanceProbeFiltersRealmSecretsBeforeOutput(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	harness := `import http.client,socket,sys
class FakeSocket:
 def settimeout(self,t): pass
 def connect(self,p): assert p=="/run/titanus/titanus.sock"
socket.socket=lambda *a:FakeSocket()
class Response:
 status=200
 def read(self,n): return b'{"name":"LAB","network":{},"nodes":{},"secrets":{"hidden":"PRIVATE_SECRET_CANARY"},"tasks":{"one":{"lease_token":"PRIVATE_SECRET_CANARY"}}}'
class Connection:
 def __init__(self,*a,**kw): pass
 def request(self,method,path): assert method=="GET" and path=="/v1/realm/state"
 def getresponse(self): return Response()
 def close(self): pass
http.client.HTTPConnection=Connection
probe=sys.argv[1];sys.argv=["probe","/v1/realm/state"]
exec(probe)
`
	out, err := exec.Command("python3", "-c", harness, acceptanceAPIProbe).CombinedOutput()
	if err != nil || strings.Contains(string(out), "PRIVATE_SECRET_CANARY") || !strings.Contains(string(out), `"name": "LAB"`) {
		t.Fatalf("unsafe probe output: %v %s", err, out)
	}
}

func TestAcceptanceHTTPSProbeVerifiesCertificatesAndHostname(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	dir := t.TempDir()
	authority, err := identity.InitAuthority(dir, "LAB")
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := authority.Issue("node", []string{"127.0.0.1"}, identity.RoleNode, identity.DefaultLeafLifetime)
	if err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{"node.crt": cert, "node.key": key} {
		data, _ := os.ReadFile(src)
		os.WriteFile(filepath.Join(dir, name), data, 0600)
	}
	tls, err := identity.TLSConfig(authority.CertPath, cert, key, true)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := cryptotls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	tls.Certificates = []cryptotls.Certificate{pair}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/v1/compatibility" {
			t.Errorf("unexpected API operation: %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"node_id":"node","realm":"LAB","schema":4,"capabilities":{}}`)
	}))
	server.TLS = tls
	server.StartTLS()
	defer server.Close()
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	probe := strings.ReplaceAll(acceptanceAPIProbe, "/etc/titanus/pki", dir)
	out, err := exec.Command("python3", "-c", probe, "/v1/compatibility", "127.0.0.1", port).CombinedOutput()
	if err != nil || !strings.Contains(string(out), `"node_id": "node"`) {
		t.Fatalf("verified HTTPS failed: %v %s", err, out)
	}
	wrongCert, wrongKey, err := authority.Issue("wrong", []string{"192.0.2.99"}, identity.RoleNode, identity.DefaultLeafLifetime)
	if err != nil {
		t.Fatal(err)
	}
	wrongTLS, err := identity.TLSConfig(authority.CertPath, wrongCert, wrongKey, true)
	if err != nil {
		t.Fatal(err)
	}
	wrongPair, err := cryptotls.LoadX509KeyPair(wrongCert, wrongKey)
	if err != nil {
		t.Fatal(err)
	}
	wrongTLS.Certificates = []cryptotls.Certificate{wrongPair}
	bad := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unverified server received a request") }))
	bad.TLS = wrongTLS
	bad.StartTLS()
	defer bad.Close()
	_, port, _ = net.SplitHostPort(bad.Listener.Addr().String())
	if out, err := exec.Command("python3", "-c", probe, "/v1/compatibility", "127.0.0.1", port).CombinedOutput(); err == nil {
		t.Fatal("accepted wrong server identity", string(out))
	}
}
