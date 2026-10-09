package deploy

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/consensus"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/preflight"
	"github.com/antonismor/Titanus-Core/internal/release"
	"github.com/antonismor/Titanus-Core/internal/version"
)

type PreparedNode struct {
	Node     string            `json:"node"`
	Script   string            `json:"script"`
	Config   string            `json:"config,omitempty"`
	Archives map[string]string `json:"archives,omitempty"`
}
type PreparedInstallation struct {
	Version     string         `json:"version"`
	Revision    string         `json:"revision"`
	Operation   string         `json:"operation"`
	PlanSHA256  string         `json:"plan_sha256"`
	ResultsPath string         `json:"results_path"`
	Nodes       []PreparedNode `json:"nodes"`
}

// PrepareRelease performs no SSH, discovery, service changes or disk operations.
// It creates a private reviewable deployment directory using the same installer
// shipped in the verified native archives. Apply is a separate explicit action.
func PrepareRelease(plan model.RealmPlan, output string) (PreparedInstallation, error) {
	var result PreparedInstallation
	if err := plan.Validate(); err != nil {
		return result, err
	}
	if plan.Installation == nil {
		return result, fmt.Errorf("plan v2 installation required")
	}
	i := *plan.Installation
	if _, err := os.Lstat(output); !os.IsNotExist(err) {
		return result, fmt.Errorf("output must be a new private directory")
	}
	// Complete both archive checks before generating keys or publishing any plan.
	if i.Operation != "rollback" {
		for _, arch := range []string{"amd64", "arm64"} {
			a := i.Archives[arch]
			if _, err := release.Verify(a.Path, a.SHA256, i.ReleaseVersion, i.Revision, arch); err != nil {
				return result, fmt.Errorf("%s: %w", arch, err)
			}
		}
	}
	parent := filepath.Dir(output)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return result, err
	}
	stage, err := os.MkdirTemp(parent, ".titanus-prepare-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(stage)
	opts := RealmDeployOptions{ClusterPort: i.APIPort, RaftPort: i.RaftPort, NodePrefix: i.NodePrefix, VXLANID: i.VXLANID}
	primary, err := primaryController(plan)
	if err != nil {
		return result, err
	}
	var auth identity.Authority
	if i.Operation == "install" {
		pki := i.PKIDir
		if pki == "" {
			pki = filepath.Join(parent, ".titanus-pki", plan.RealmName)
		}
		auth, err = identity.InitAuthority(pki, plan.RealmName)
		if err != nil {
			return result, err
		}
		cert, key, e := auth.Issue("operator", nil, identity.RoleAdmin, 24*time.Hour)
		if e != nil {
			return result, e
		}
		operator := filepath.Join(stage, "operator")
		if e = os.Mkdir(operator, 0700); e != nil {
			return result, e
		}
		for name, src := range map[string]string{"ca.crt": auth.CertPath, "ca.crl": filepath.Join(filepath.Dir(auth.CertPath), "ca.crl"), "admin.crt": cert, "admin.key": key} {
			data, e := os.ReadFile(src)
			if e != nil {
				return result, e
			}
			if e = os.WriteFile(filepath.Join(operator, name), data, 0600); e != nil {
				return result, e
			}
		}
	}
	result = PreparedInstallation{Version: i.ReleaseVersion, Revision: i.Revision, Operation: i.Operation, PlanSHA256: planDigest(plan), ResultsPath: filepath.Join(output, "results.json")}
	for _, node := range installationOrder(plan) {
		if filepath.Base(node.Name) != node.Name || node.Name == "." || node.Name == ".." || strings.ContainsAny(node.Name, "\x00\r\n") {
			return result, fmt.Errorf("unsafe node name")
		}
		dir := filepath.Join(stage, node.Name)
		if err = os.Mkdir(dir, 0700); err != nil {
			return result, err
		}
		cfg := map[string][]byte{}
		controller := hasCapability(node, model.CapabilityControl)
		gateway := hasCapability(node, model.CapabilityGateway)
		if i.Operation == "install" {
			role := identity.RoleNode
			if controller {
				role = identity.RoleController
			}
			cert, key, e := auth.Issue(node.Name, []string{node.ManagementIP}, role, 24*time.Hour)
			if e != nil {
				return result, e
			}
			for name, src := range map[string]string{"pki/ca.crt": auth.CertPath, "pki/ca.crl": filepath.Join(filepath.Dir(auth.CertPath), "ca.crl"), "pki/node.crt": cert, "pki/node.key": key} {
				data, e := os.ReadFile(src)
				if e != nil {
					return result, e
				}
				cfg[name] = data
			}
			if controller {
				data, e := os.ReadFile(auth.KeyPath)
				if e != nil {
					return result, e
				}
				cfg["pki/ca.key"] = data
			}
			cfg["daemon.env"] = []byte(daemonEnvironment(plan, node, primary, opts, controller, gateway))
			cfg["agent.env"] = []byte(agentEnvironment(plan, node, primary, opts))
			if controller && len(plan.ControlNodes()) > 1 {
				c := consensus.Config{ID: node.Name, Realm: plan.RealmName, Peers: controllerPeers(plan, opts), Bootstrap: node.Name == primary.Name}
				if e := c.Validate(); e != nil {
					return result, e
				}
				cfg["ha.json"], err = json.Marshal(c)
				if err != nil {
					return result, err
				}
			}
		}
		item := PreparedNode{Node: node.Name, Script: filepath.Join(output, node.Name, "apply.sh"), Archives: map[string]string{}}
		if len(cfg) > 0 {
			item.Config = filepath.Join(output, node.Name, "config.tar")
			if err = writeConfigTar(filepath.Join(dir, "config.tar"), cfg); err != nil {
				return result, err
			}
		}
		if i.Operation != "rollback" {
			for arch, a := range i.Archives {
				abs, e := filepath.Abs(a.Path)
				if e != nil {
					return result, e
				}
				item.Archives[arch] = abs
			}
		}
		script := releaseApplyScript(plan, node, i, controller, cfg)
		if item.Config != "" {
			data, e := os.ReadFile(filepath.Join(dir, "config.tar"))
			if e != nil {
				return result, e
			}
			sum := sha256.Sum256(data)
			script = strings.Replace(script, "mkdir \"$stage/config\"", "printf '%s  config.tar\\n' "+shellQuote(hex.EncodeToString(sum[:]))+" | sha256sum -c -\nmkdir \"$stage/config\"", 1)
		}
		if err = os.WriteFile(filepath.Join(dir, "apply.sh"), []byte(script), 0700); err != nil {
			return result, err
		}
		result.Nodes = append(result.Nodes, item)
	}
	data, _ := json.MarshalIndent(result, "", "  ")
	if err = os.WriteFile(filepath.Join(stage, "deployment.json"), append(data, '\n'), 0600); err != nil {
		return result, err
	}
	if err = os.Rename(stage, output); err != nil {
		return result, err
	}
	return result, nil
}

func writeConfigTar(filename string, cfg map[string][]byte) error {
	hashes := map[string]string{}
	for name, data := range cfg {
		sum := sha256.Sum256(data)
		hashes[name] = hex.EncodeToString(sum[:])
	}
	manifest, _ := json.Marshal(hashes)
	cfg["config.json"] = manifest
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	names := []string{}
	for name := range cfg {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := cfg[name]
		if err = tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(data))}); err != nil {
			return err
		}
		if _, err = tw.Write(data); err != nil {
			return err
		}
	}
	if err = tw.Close(); err != nil {
		return err
	}
	return f.Sync()
}

func releaseApplyScript(plan model.RealmPlan, node model.NodeSpec, i model.Installation, controller bool, cfg map[string][]byte) string {
	script := "#!/usr/bin/env bash\nset -euo pipefail\numask 077\nif [[ $(id -u) != 0 ]]; then echo 'Run as root on the selected dedicated node' >&2; exit 1; fi\ncd \"$(dirname \"$0\")\"\n"
	if i.Operation == "rollback" {
		// Invoke the already verified installed installer, never an untrusted PATH helper.
		return script + "python3 /usr/local/lib/titanus/current/scripts/install-release.py --rollback --expect-version " + shellQuote(i.ReleaseVersion) + " --expect-revision " + shellQuote(i.Revision) + "\nsystemctl --no-pager is-active titanusd.service titanus-agent.service\n"
	}
	script += "case $(uname -m) in x86_64) arch=amd64;; aarch64) arch=arm64;; *) exit 1;; esac\n"
	for _, arch := range []string{"amd64", "arm64"} {
		script += "if [[ $arch == " + arch + " ]]; then archive=" + shellQuote("release-"+arch+".tar.gz") + "; expected=" + shellQuote(i.Archives[arch].SHA256) + "; fi\n"
	}
	script += "printf '%s  %s\\n' \"$expected\" \"$archive\" | sha256sum -c -\nstage=$(mktemp -d)\ntrap 'rm -rf -- \"$stage\"' EXIT\ntar -xzf \"$archive\" -C \"$stage\"\nbundle=\"$stage/titanus-" + i.ReleaseVersion + "-linux-$arch\"\n"
	if i.Operation == "install" {
		script += "if [[ -e /etc/titanus/daemon.env || -e /var/lib/titanus/realm/state.json || -d /var/lib/titanus/realm/consensus ]]; then echo 'Initial installation refuses existing Realm state/configuration' >&2; exit 1; fi\nmkdir \"$stage/config\"\ntar -xf config.tar -C \"$stage/config\"\n"
	}
	script += "python3 \"$bundle/scripts/install-release.py\" --bundle \"$bundle\" --expect-version " + shellQuote(i.ReleaseVersion) + " --expect-revision " + shellQuote(i.Revision)
	if i.Operation == "install" {
		script += " --config \"$stage/config\""
	}
	script += "\n"
	if i.Operation == "install" && controller {
		script += fmt.Sprintf("TITANUS_STATE_ROOT=/var/lib/titanus /usr/local/bin/titanus realm seed --name %s --fabric-cidr %s --service-cidr %s --node-prefix %d --vxlan-id %d\n", shellQuote(plan.RealmName), shellQuote(plan.FabricCIDR), shellQuote(plan.ServiceCIDR), i.NodePrefix, i.VXLANID)
	}
	if i.Operation == "install" && i.Start {
		script += "systemctl enable titanusd.service titanus-agent.service\nsystemctl start titanusd.service titanus-agent.service\nsystemctl --no-pager is-active titanusd.service titanus-agent.service\n"
	}
	if i.Operation != "install" {
		script += "systemctl --no-pager is-active titanusd.service titanus-agent.service\n"
	}
	return script
}

func planDigest(plan model.RealmPlan) string {
	data, _ := json.Marshal(plan)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Install voters before non-voters, keeping the designated bootstrap first.
// Upgrades remain serial; protocol/schema ordering is restricted until M5.
func installationOrder(plan model.RealmPlan) []model.NodeSpec {
	nodes := append([]model.NodeSpec(nil), plan.Nodes...)
	sort.SliceStable(nodes, func(i, j int) bool {
		return hasCapability(nodes[i], model.CapabilityControl) && !hasCapability(nodes[j], model.CapabilityControl)
	})
	return nodes
}

// ApplyPrepared is intentionally explicit. It performs read-only preflight on
// the whole plan before transferring any files, then installs one node at a time.
func (d *RealmDeployer) ApplyPrepared(plan model.RealmPlan, prepared PreparedInstallation) ([]RealmNodeResult, error) {
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if plan.Installation == nil || prepared.PlanSHA256 != planDigest(plan) || prepared.Version != plan.Installation.ReleaseVersion || prepared.Revision != plan.Installation.Revision || prepared.Operation != plan.Installation.Operation || len(prepared.Nodes) != len(plan.Nodes) {
		return nil, fmt.Errorf("prepared deployment does not match plan")
	}
	nodes := installationOrder(plan)
	for index, node := range nodes {
		if prepared.Nodes[index].Node != node.Name {
			return nil, fmt.Errorf("prepared node order mismatch")
		}
	}
	check := d.ReleasePreflight
	if check == nil {
		check = func(p model.RealmPlan) error { return preflight.Summary(preflight.NewRunner().Run(p)) }
	}
	if err := check(plan); err != nil {
		return nil, err
	}
	arches := map[string]string{}
	for _, node := range plan.Nodes {
		res, err := d.SSH.Run(node, "uname -m")
		if err != nil {
			return nil, err
		}
		switch res.Stdout {
		case "x86_64":
			arches[node.Name] = "amd64"
		case "aarch64":
			arches[node.Name] = "arm64"
		default:
			return nil, fmt.Errorf("unsupported native host architecture on %s", node.Name)
		}
		command := remoteSudoPrefix + "; $SUDO test -d /etc"
		if prepared.Operation == "install" {
			command += " && ! $SUDO test -e /etc/titanus/daemon.env && ! $SUDO test -e /usr/local/lib/titanus/current && ! $SUDO test -d /var/lib/titanus/realm"
		} else {
			command += " && $SUDO test -L /usr/local/lib/titanus/current"
		}
		if _, err = d.SSH.Run(node, command); err != nil {
			return nil, fmt.Errorf("installation preflight failed on %s: %w", node.Name, err)
		}
		if _, err = d.SSH.Run(node, "command -v python3 && command -v systemctl && command -v tar && command -v sha256sum"); err != nil {
			return nil, fmt.Errorf("required release tools missing on %s: %w", node.Name, err)
		}
		if prepared.Operation != "install" {
			// Read only the public build identity and configured Realm/node IDs.
			// Never print keys, certificates or arbitrary environment values.
			inspect := "import json,pathlib,subprocess; env=dict(line.split('=',1) for line in pathlib.Path('/etc/titanus/daemon.env').read_text().splitlines() if '=' in line); print(json.dumps({'build':json.loads(subprocess.check_output(['/usr/local/bin/titanus','version','--json'])),'realm':env.get('TITANUS_REALM_NAME'),'node':env.get('TITANUS_NODE_ID')}))"
			res, err := d.SSH.Run(node, remoteSudoPrefix+"; $SUDO python3 -c "+shellQuote(inspect))
			if err != nil {
				return nil, fmt.Errorf("cannot inspect installed identity on %s: %w", node.Name, err)
			}
			var installed struct {
				Build version.Build `json:"build"`
				Realm string        `json:"realm"`
				Node  string        `json:"node"`
			}
			if err = json.Unmarshal([]byte(res.Stdout), &installed); err != nil || installed.Realm != plan.RealmName || installed.Node != node.Name || installed.Build.StateProfile != version.StateProfile || installed.Build.Arch != arches[node.Name] || installed.Build.OS != "linux" {
				return nil, fmt.Errorf("installed Realm/node/architecture/state profile differs from plan on %s", node.Name)
			}
		}
	}
	results := []RealmNodeResult{}
	for index, node := range nodes {
		item := prepared.Nodes[index]
		if item.Node != node.Name {
			return results, fmt.Errorf("prepared node order mismatch")
		}
		remoteDir, err := d.SSH.Run(node, "umask 077; mktemp -d /tmp/titanus-release-XXXXXXXX")
		if err != nil {
			return results, err
		}
		dir := strings.TrimSpace(remoteDir.Stdout)
		if !strings.HasPrefix(dir, "/tmp/titanus-release-") || strings.ContainsAny(dir, "\n\r\x00 ") {
			return results, fmt.Errorf("invalid remote staging path")
		}
		files := map[string]string{"apply.sh": item.Script}
		if item.Config != "" {
			files["config.tar"] = item.Config
		}
		arch := arches[node.Name]
		if prepared.Operation != "rollback" {
			files["release-"+arch+".tar.gz"] = item.Archives[arch]
		}
		applyErr := func() error {
			for name, local := range files {
				if e := d.SSH.CopyFile(node, local, dir+"/"+name); e != nil {
					return e
				}
			}
			_, e := d.SSH.Run(node, remoteSudoPrefix+"; $SUDO bash "+shellQuote(dir+"/apply.sh"))
			return e
		}()
		_, cleanupErr := d.SSH.Run(node, remoteSudoPrefix+"; $SUDO rm -rf -- "+shellQuote(dir))
		if applyErr != nil {
			return results, fmt.Errorf("%s incomplete; earlier nodes retain their recorded results: %w", node.Name, applyErr)
		}
		if cleanupErr != nil {
			return results, cleanupErr
		}
		results = append(results, RealmNodeResult{Node: node.Name, Address: node.ManagementIP, Role: prepared.Operation, Message: "installed"})
		data, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return results, err
		}
		if err = durable.WriteFile(prepared.ResultsPath, append(data, '\n'), 0600); err != nil {
			return results, err
		}
	}
	if prepared.Operation == "install" && plan.Installation.Start && plan.Ceph.Enabled && plan.Ceph.Provision {
		if _, err := NewCephDeployer().Deploy(plan); err != nil {
			return results, err
		}
	}
	return results, nil
}
