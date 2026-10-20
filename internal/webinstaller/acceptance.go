package webinstaller

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/controlapi"
	"github.com/antonismor/Titanus-Core/internal/deploy"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/prerequisites"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/remote"
	"github.com/antonismor/Titanus-Core/internal/version"
)

type AcceptanceCheck struct {
	Node     string `json:"node"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Details  string `json:"details"`
	Attempts int    `json:"attempts"`
}

type AcceptanceReport struct {
	Format         string                    `json:"format"`
	ID             string                    `json:"id"`
	SessionID      string                    `json:"session_id"`
	Realm          string                    `json:"realm"`
	Revision       string                    `json:"revision"`
	ReleaseVersion string                    `json:"release_version"`
	Profile        model.InstallationProfile `json:"profile"`
	TotalNodes     int                       `json:"total_nodes"`
	Scope          string                    `json:"scope"`
	Status         string                    `json:"status"`
	ReadOnly       bool                      `json:"checks_read_only"`
	Checks         []AcceptanceCheck         `json:"checks"`
	StartedAt      time.Time                 `json:"started_at"`
	FinishedAt     time.Time                 `json:"finished_at,omitempty"`
}

type acceptanceSSH interface {
	Run(model.NodeSpec, string) (remote.Result, error)
}

// Only GET is available. Filter on the host before any response leaves it;
// Realm workloads, secret records, tokens and environment values are excluded.
const acceptanceAPIProbe = `import http.client,json,socket,ssl,sys
path=sys.argv[1]
if path=="/v1/compatibility":
    tls=ssl.create_default_context(cafile="/etc/titanus/pki/ca.crt")
    tls.load_verify_locations(cafile="/etc/titanus/pki/ca.crl")
    tls.verify_flags|=ssl.VERIFY_CRL_CHECK_LEAF
    tls.load_cert_chain("/etc/titanus/pki/node.crt","/etc/titanus/pki/node.key")
    conn=http.client.HTTPSConnection(sys.argv[2],int(sys.argv[3]),timeout=5,context=tls)
elif path in ("/v1/realm/state","/v1/realm/consensus"):
    conn=http.client.HTTPConnection("localhost",timeout=5)
    conn.sock=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM)
    conn.sock.settimeout(5)
    conn.sock.connect("/run/titanus/titanus.sock")
else:
    raise SystemExit("Unsupported acceptance endpoint")
conn.request("GET",path)
response=conn.getresponse()
if response.status!=200: raise SystemExit("Acceptance API HTTP "+str(response.status))
raw=response.read((4<<20)+1)
conn.close()
if len(raw)>4<<20: raise SystemExit("Acceptance response exceeds size limit")
p=json.loads(raw)
if path=="/v1/realm/state":
    keys=("id","address","capabilities","state","last_pulse","power_quarantined","storage_quarantined")
    p={"name":p.get("name"),"network":p.get("network"),"native_ha":p.get("native_ha",False),"nodes":{k:{f:v.get(f) for f in keys if f in v} for k,v in p.get("nodes",{}).items()}}
elif path=="/v1/realm/consensus":
    p={k:p.get(k,"") for k in ("id","realm","state","leader_api")}
else:
    p={k:p.get(k) for k in ("node_id","realm","schema","capabilities")}
print(json.dumps(p))
`

func acceptanceGet(ssh acceptanceSSH, node model.NodeSpec, port int, endpoint string, out any) error {
	return acceptanceGetFrom(ssh, node, node, port, endpoint, out)
}

func acceptanceGetFrom(ssh acceptanceSSH, source, node model.NodeSpec, port int, endpoint string, out any) error {
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	command := "python3 -c " + quote(acceptanceAPIProbe) + " " + quote(endpoint) + " " + quote(node.ManagementIP) + " " + strconv.Itoa(port)
	result, err := ssh.Run(source, prerequisites.SudoScript(command))
	if err != nil {
		return fmt.Errorf("verified read-only API probe failed on %s", node.Name)
	}
	if err := json.Unmarshal([]byte(result.Stdout), out); err != nil {
		return fmt.Errorf("invalid acceptance response from %s", node.Name)
	}
	return nil
}

func newAcceptanceReport(plan model.RealmPlan, session, scope string) AcceptanceReport {
	return AcceptanceReport{Format: "titanus-installation-report/v1", ID: newSessionID(), SessionID: session, Realm: plan.RealmName, Revision: plan.Installation.Revision, ReleaseVersion: plan.Installation.ReleaseVersion, Profile: plan.Installation.Profile, TotalNodes: len(plan.Nodes), Scope: scope, Status: "Running", ReadOnly: true, Checks: []AcceptanceCheck{}, StartedAt: time.Now().UTC()}
}

type acceptanceTask struct {
	node, name string
	run        func() (string, error)
}

// Read-only readiness acceptance, not a fault-injection or workload benchmark.
// Retry observations while services converge; never replay installation steps.
func runClusterAcceptance(ctx context.Context, plan model.RealmPlan, ssh acceptanceSSH, report AcceptanceReport, delay time.Duration, emit func(AcceptanceReport)) AcceptanceReport {
	if emit == nil {
		emit = func(AcceptanceReport) {}
	}
	var tasks []acceptanceTask
	for _, node := range plan.Nodes {
		tasks = append(tasks, acceptanceTask{node.Name, "Titanus services", func() (string, error) {
			result, err := ssh.Run(node, "systemctl --no-pager is-active titanusd.service titanus-agent.service")
			if err != nil || !slices.Equal(strings.Fields(result.Stdout), []string{"active", "active"}) {
				return "", fmt.Errorf("titanusd and titanus-agent must both be active")
			}
			return "titanusd and titanus-agent are active", nil
		}}, acceptanceTask{node.Name, "HTTPS identity, certificates and release", func() (string, error) {
			var info controlapi.CompatibilityInfo
			// Check reachability from a CONTROL host, rather than only loopback
			// health on each target. Its Realm certificate stays on that host.
			if err := acceptanceGetFrom(ssh, plan.ControlNodes()[0], node, plan.Installation.APIPort, "/v1/compatibility", &info); err != nil {
				return "", err
			}
			b := info.Capabilities.Build
			if info.NodeID != node.Name || info.Realm != plan.RealmName || b.Version != plan.Installation.ReleaseVersion || b.Revision != plan.Installation.Revision || b.StateProfile != version.StateProfile || !info.Capabilities.Supports(info.Schema) {
				return "", fmt.Errorf("node/Realm/release/protocol identity differs from the selected Plan")
			}
			return "Verified HTTPS/mTLS, node identity and exact selected release (" + b.Arch + ")", nil
		}})
	}
	for _, node := range plan.ControlNodes() {
		tasks = append(tasks, acceptanceTask{node.Name, "Realm membership, roles and Fabric", func() (string, error) {
			var state realm.State
			if err := acceptanceGet(ssh, node, plan.Installation.APIPort, "/v1/realm/state", &state); err != nil {
				return "", err
			}
			if err := validateAcceptanceState(state, plan, time.Now()); err != nil {
				return "", err
			}
			return fmt.Sprintf("%d/%d expected nodes READY with fresh pulses, matching roles and Fabric", len(state.Nodes), len(plan.Nodes)), nil
		}})
	}
	tasks = append(tasks, acceptanceTask{"Realm", "Controller quorum", func() (string, error) {
		controls := plan.ControlNodes()
		leaders := 0
		leader := ""
		origins := map[string]bool{}
		for _, n := range controls {
			origins["https://"+net.JoinHostPort(n.ManagementIP, strconv.Itoa(plan.Installation.APIPort))] = true
		}
		for _, n := range controls {
			var status map[string]string
			if err := acceptanceGet(ssh, n, plan.Installation.APIPort, "/v1/realm/consensus", &status); err != nil {
				return "", err
			}
			if len(controls) == 1 && status["state"] == "standalone" {
				return "Standalone CONTROL node responds", nil
			}
			if status["id"] != n.Name || status["realm"] != plan.RealmName || (status["state"] != "Leader" && status["state"] != "Follower") || !origins[status["leader_api"]] {
				return "", fmt.Errorf("invalid controller identity/quorum on %s", n.Name)
			}
			if leader != "" && leader != status["leader_api"] {
				return "", fmt.Errorf("controllers disagree on the leader")
			}
			leader = status["leader_api"]
			if status["state"] == "Leader" {
				leaders++
				if leader != "https://"+net.JoinHostPort(n.ManagementIP, strconv.Itoa(plan.Installation.APIPort)) {
					return "", fmt.Errorf("leader origin differs from its identity")
				}
			}
		}
		if leaders != 1 {
			return "", fmt.Errorf("expected one leader, observed %d", leaders)
		}
		return fmt.Sprintf("%d/%d CONTROL voters respond and agree on one leader", len(controls), len(controls)), nil
	}})
	if plan.Ceph.Enabled {
		node := plan.StorageNodes()[0]
		tasks = append(tasks, acceptanceTask{node.Name, "Ceph health and storage policy", func() (string, error) {
			command := "ceph status --format json"
			if plan.NativeHAStorage() {
				command = deploy.HACephProbeCommand()
			}
			result, err := ssh.Run(node, prerequisites.SudoScript(command))
			if err != nil {
				return "", fmt.Errorf("read-only Ceph probe failed")
			}
			if plan.NativeHAStorage() {
				if err := deploy.ValidateHACephReport([]byte(result.Stdout), plan); err != nil {
					return "", err
				}
			} else if err := validateCleanCeph([]byte(result.Stdout)); err != nil {
				return "", err
			}
			if plan.NativeHAStorage() {
				return "HEALTH_OK; PGs active+clean; Native HA monitor, manager, pool and host-domain policy verified", nil
			}
			return "HEALTH_OK; all PGs active+clean", nil
		}}, acceptanceTask{node.Name, "Ceph OSD inventory", func() (string, error) {
			result, err := ssh.Run(node, prerequisites.SudoScript("ceph osd stat --format json"))
			if err != nil {
				return "", fmt.Errorf("read-only OSD inventory unavailable")
			}
			var osds struct {
				Total int `json:"num_osds"`
				Up    int `json:"num_up_osds"`
				In    int `json:"num_in_osds"`
			}
			if json.Unmarshal([]byte(result.Stdout), &osds) != nil || osds.Total < 1 || osds.Total != osds.Up || osds.Total != osds.In {
				return "", fmt.Errorf("all Ceph OSDs must be up and in")
			}
			expected := 0
			for _, n := range plan.StorageNodes() {
				expected += len(n.CephDevices)
			}
			if plan.Ceph.Provision && osds.Total != expected {
				return "", fmt.Errorf("expected %d OSDs from selected devices, observed %d", expected, osds.Total)
			}
			return fmt.Sprintf("%d/%d OSDs up and in", osds.Up, osds.Total), nil
		}})
	}
	for _, task := range tasks {
		report.Checks = append(report.Checks, AcceptanceCheck{Node: task.node, Name: task.name, Status: "Pending"})
	}
	emit(report)
	for index, task := range tasks {
		check := &report.Checks[index]
		for attempt := 1; attempt <= 3; attempt++ {
			if ctx.Err() != nil {
				check.Status = "Failed"
				check.Details = "Acceptance deadline exceeded"
				break
			}
			check.Status = "Running"
			check.Attempts = attempt
			check.Details = fmt.Sprintf("Read-only probe %d/3", attempt)
			emit(report)
			detail, err := task.run()
			if err == nil {
				check.Status = "Passed"
				check.Details = detail
				break
			}
			check.Details = err.Error()
			check.Status = "Failed"
			if attempt < 3 {
				check.Status = "Running"
				emit(report)
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
				case <-timer.C:
				}
			}
		}
		emit(report)
	}
	report.Status = "Success"
	for _, check := range report.Checks {
		if check.Status != "Passed" {
			report.Status = "Failed"
		}
	}
	report.FinishedAt = time.Now().UTC()
	emit(report)
	return report
}

func validateAcceptanceState(state realm.State, plan model.RealmPlan, now time.Time) error {
	if state.Name != plan.RealmName || len(state.Nodes) != len(plan.Nodes) || state.Network.FabricCIDR != plan.FabricCIDR || state.Network.ServiceCIDR != plan.ServiceCIDR || state.Network.NodePrefix != plan.Installation.NodePrefix || state.Network.VXLANID != plan.Installation.VXLANID || state.NativeHA != (plan.Installation.Profile == model.ProfileNativeHA) {
		return fmt.Errorf("Realm identity, topology or Fabric differs from the Plan")
	}
	for _, expected := range plan.Nodes {
		n, ok := state.Nodes[expected.Name]
		wantCaps := append([]model.Capability(nil), expected.Capabilities...)
		gotCaps := append([]model.Capability(nil), n.Capabilities...)
		slices.Sort(wantCaps)
		slices.Sort(gotCaps)
		if !ok || n.ID != expected.Name || n.Address != net.JoinHostPort(expected.ManagementIP, strconv.Itoa(plan.Installation.APIPort)) || n.State != realm.NodeReady || n.PowerQuarantined || n.StorageQuarantined || now.Sub(n.LastPulse) > 60*time.Second || n.LastPulse.After(now.Add(10*time.Second)) || !slices.Equal(wantCaps, gotCaps) {
			return fmt.Errorf("node %s is missing, stale, quarantined or differs from the planned identity/roles", expected.Name)
		}
	}
	return nil
}

func validateCleanCeph(data []byte) error {
	var status struct {
		Health struct {
			Status string `json:"status"`
		} `json:"health"`
		PG struct {
			Total  int `json:"num_pgs"`
			States []struct {
				Name  string `json:"state_name"`
				Count int    `json:"count"`
			} `json:"pgs_by_state"`
		} `json:"pgmap"`
	}
	if json.Unmarshal(data, &status) != nil || status.Health.Status != "HEALTH_OK" || status.PG.Total < 1 {
		return fmt.Errorf("Ceph must be HEALTH_OK with nonempty PG evidence")
	}
	clean := 0
	for _, s := range status.PG.States {
		if s.Name != "active+clean" {
			return fmt.Errorf("Ceph PGs are not all active+clean")
		}
		clean += s.Count
	}
	if clean != status.PG.Total {
		return fmt.Errorf("incomplete clean-PG evidence")
	}
	return nil
}

func (s *Server) runAcceptance(plan model.RealmPlan, ssh acceptanceSSH) {
	report := newAcceptanceReport(plan, s.sessionID(), "cluster")
	if !plan.Installation.Start {
		report.Scope = "files-only"
		report.Status = "Success"
		report.FinishedAt = time.Now().UTC()
		report.Checks = append(report.Checks, AcceptanceCheck{Node: "Realm", Name: "Cluster acceptance", Status: "Skipped", Details: "Services were not requested; live cluster readiness has not been verified."})
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		last := map[int]string{}
		report = runClusterAcceptance(ctx, plan, ssh, report, 2*time.Second, func(update AcceptanceReport) {
			// Publish a terminal result only after the final report is durable.
			if update.Status != "Running" {
				return
			}
			copy := update
			copy.Checks = append([]AcceptanceCheck(nil), update.Checks...)
			s.mu.Lock()
			s.state.Report = &copy
			s.state.Status = "acceptance"
			s.state.Phase = "Complete"
			s.state.Progress = 96
			s.state.Message = "Running read-only cluster acceptance checks."
			s.state.UpdatedAt = time.Now().UTC()
			err := s.persistStateLocked()
			s.mu.Unlock()
			if err != nil {
				cancel()
			}
			for index, check := range update.Checks {
				if check.Status == "Pending" {
					continue
				}
				event := fmt.Sprintf("[%s] %s: %s (attempt %d) %s", check.Node, check.Name, check.Status, check.Attempts, check.Details)
				if event != last[index] {
					s.logf("ACCEPTANCE", "%s", event)
					last[index] = event
				}
			}
		})
	}
	s.completeAcceptance(report)
}

func (s *Server) completeAcceptance(report AcceptanceReport) {
	path := filepath.Join(s.cfg.StateDir, "reports")
	err := os.MkdirAll(path, 0700)
	if err == nil {
		data, _ := json.MarshalIndent(report, "", "  ")
		err = atomicWrite(filepath.Join(path, report.ID+".json"), append(data, '\n'), 0600)
	}
	if err != nil {
		report.Status = "Failed"
		report.Checks = append(report.Checks, AcceptanceCheck{Node: "Installer", Name: "Status Report persistence", Status: "Failed", Details: "Could not save the final Status Report."})
	}
	s.mu.Lock()
	s.state.Report = &report
	s.state.Status = "completed"
	s.state.Message = "Installation and cluster acceptance completed successfully."
	if report.Scope == "files-only" {
		s.state.Message = "Release installed successfully; services were not requested and cluster acceptance was not run."
	}
	if report.Status != "Success" {
		s.state.Status = "failed"
		s.state.Error = "Cluster acceptance failed. Review the Status Report and verbose output."
		s.state.Message = "Installation finished with failed acceptance checks."
	} else {
		s.state.Error = ""
	}
	s.state.Phase = "Complete"
	s.state.Progress = 100
	s.state.Completed = report.Status == "Success"
	s.state.PreparedOnly = false
	s.state.Running = s.running
	s.state.FinishedAt = time.Now().UTC()
	s.state.UpdatedAt = s.state.FinishedAt
	if s.persistStateLocked() != nil {
		s.state.Status = "failed"
		s.state.Completed = false
		s.state.Report.Status = "Failed"
		s.state.Error = "Cannot persist final installation status."
		report.Checks = append(report.Checks, AcceptanceCheck{Node: "Installer", Name: "Final status persistence", Status: "Failed", Details: s.state.Error})
		data, _ := json.MarshalIndent(report, "", "  ")
		_ = atomicWrite(filepath.Join(path, report.ID+".json"), append(data, '\n'), 0600)
	}
	s.mu.Unlock()
	s.logf("ACCEPTANCE", "Status Report: %s", report.Status)
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	s.mu.RLock()
	report := s.state.Report
	s.mu.RUnlock()
	if report == nil {
		writeError(w, http.StatusNotFound, "no Status Report is available")
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="titanus-status-report.json"`)
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleAcceptance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	s.operationMu.Lock()
	defer s.operationMu.Unlock()
	if s.quiescing {
		writeError(w, http.StatusConflict, "installer is switching to terminal-only mode")
		return
	}
	plan, ok := s.currentPlan()
	s.mu.Lock()
	if s.running || !ok || s.state.PreparedOnly || s.state.DeploymentPath == "" || (s.state.Status != "completed" && (s.state.Report == nil || s.state.Report.Scope != "cluster")) {
		s.mu.Unlock()
		writeError(w, http.StatusConflict, "a completed live deployment is required for acceptance; installation is never replayed")
		return
	}
	executor := remote.NewSSHExecutor()
	executor.CommandTimeout = 45 * time.Second
	dir := s.bootstrapFor(plan).Directory
	executor.IdentityFile = filepath.Join(dir, "id_ed25519")
	executor.KnownHostsFile = filepath.Join(dir, "known_hosts")
	for _, path := range []string{executor.IdentityFile, executor.KnownHostsFile} {
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			s.mu.Unlock()
			writeError(w, http.StatusConflict, "saved pinned SSH trust is unavailable; acceptance will not fall back to other keys")
			return
		}
	}
	previous := s.state
	s.running = true
	s.state.Running = true
	s.state.Completed = false
	s.state.Status = "acceptance"
	s.state.Phase = "Complete"
	s.state.Error = ""
	s.state.FinishedAt = time.Time{}
	s.state.Report = nil
	s.state.UpdatedAt = time.Now().UTC()
	if err := s.persistStateLocked(); err != nil {
		s.running = false
		s.state = previous
		s.mu.Unlock()
		writeError(w, 500, "cannot persist acceptance launch")
		return
	}
	state := s.state
	s.mu.Unlock()
	go func() { defer s.finishRun(true); s.runAcceptance(plan, executor) }()
	writeJSON(w, http.StatusAccepted, state)
}
