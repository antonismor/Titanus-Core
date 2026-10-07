package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/preflight"
	"github.com/antonismor/Titanus-Core/internal/remote"
)

type RealmDeployOptions struct {
	BinDir         string
	PKIDir         string
	NodePrefix     int
	VXLANID        int
	ClusterPort    int
	StartServices  bool
}

type RealmNodeResult struct {
	Node    string
	Address string
	Role    string
	Message string
}

type RealmDeployer struct {
	SSH *remote.SSHExecutor
}

func NewRealmDeployer() *RealmDeployer {
	executor := remote.NewSSHExecutor()
	executor.CommandTimeout = 3 * time.Minute
	return &RealmDeployer{SSH: executor}
}

func (d *RealmDeployer) Deploy(plan model.RealmPlan, opts RealmDeployOptions) ([]RealmNodeResult, error) {
	plan.Normalize()
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if opts.BinDir == "" {
		opts.BinDir = "./bin"
	}
	if opts.NodePrefix == 0 {
		opts.NodePrefix = 24
	}
	if opts.VXLANID == 0 {
		opts.VXLANID = 4242
	}
	if opts.ClusterPort == 0 {
		opts.ClusterPort = 9443
	}
	if opts.PKIDir == "" {
		opts.PKIDir = filepath.Join(".", ".titanus-pki", plan.RealmName)
	}
	if err := validateEnvValue(plan.RealmName); err != nil {
		return nil, err
	}

	binaries := map[string]string{
		"titanus":       filepath.Join(opts.BinDir, "titanus"),
		"titanusd":      filepath.Join(opts.BinDir, "titanusd"),
		"titanus-agent": filepath.Join(opts.BinDir, "titanus-agent"),
		"titanus-init":  filepath.Join(opts.BinDir, "titanus-init"),
	}
	for name, path := range binaries {
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("%s binary %s: %w", name, path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", path)
		}
	}

	reports := preflight.NewRunner().Run(plan)
	if err := preflight.Summary(reports); err != nil {
		return nil, fmt.Errorf("Realm deployment blocked: %w", err)
	}

	primary, err := primaryController(plan)
	if err != nil {
		return nil, err
	}
	gatewayExplicit := false
	for _, node := range plan.Nodes {
		if hasCapability(node, model.CapabilityGateway) {
			gatewayExplicit = true
			break
		}
	}

	auth, err := identity.InitAuthority(opts.PKIDir, plan.RealmName)
	if err != nil {
		return nil, fmt.Errorf("initialize Titanus Realm CA: %w", err)
	}

	tempDir, err := os.MkdirTemp("", "titanus-realm-deploy-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)

	daemonUnit := filepath.Join(tempDir, "titanusd.service")
	agentUnit := filepath.Join(tempDir, "titanus-agent.service")
	if err := os.WriteFile(daemonUnit, []byte(realmDaemonService), 0644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(agentUnit, []byte(realmAgentService), 0644); err != nil {
		return nil, err
	}

	nodes := append([]model.NodeSpec(nil), plan.Nodes...)
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].Name == primary.Name {
			return true
		}
		if nodes[j].Name == primary.Name {
			return false
		}
		return nodes[i].Name < nodes[j].Name
	})

	type nodeFiles struct {
		node       model.NodeSpec
		cert       string
		key        string
		daemonEnv  string
		agentEnv   string
		controller bool
		gateway    bool
	}
	prepared := make([]nodeFiles, 0, len(nodes))

	for _, node := range nodes {
		if err := validateEnvValue(node.Name); err != nil {
			return nil, err
		}
		cert, key, err := auth.IssueNode(node.Name, []string{node.ManagementIP})
		if err != nil {
			return nil, fmt.Errorf("issue certificate for %s: %w", node.Name, err)
		}
		controller := node.Name == primary.Name
		gateway := hasCapability(node, model.CapabilityGateway) || (!gatewayExplicit && controller)

		daemonEnvPath := filepath.Join(tempDir, node.Name+"-daemon.env")
		agentEnvPath := filepath.Join(tempDir, node.Name+"-agent.env")
		if err := os.WriteFile(daemonEnvPath, []byte(daemonEnvironment(plan, node, primary, opts, controller, gateway)), 0600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(agentEnvPath, []byte(agentEnvironment(plan, node, primary, opts)), 0600); err != nil {
			return nil, err
		}
		prepared = append(prepared, nodeFiles{
			node: node, cert: cert, key: key,
			daemonEnv: daemonEnvPath, agentEnv: agentEnvPath,
			controller: controller, gateway: gateway,
		})
	}

	results := make([]RealmNodeResult, 0, len(prepared))
	for _, item := range prepared {
		if err := d.stageNode(item.node, binaries, auth.CertPath, item.cert, item.key, daemonUnit, agentUnit, item.daemonEnv, item.agentEnv); err != nil {
			return results, err
		}
		if item.controller {
			if err := d.SSH.CopyFile(item.node, auth.KeyPath, "/tmp/titanus-ca.key"); err != nil {
				return results, fmt.Errorf("copy Realm CA key to primary %s: %w", item.node.Name, err)
			}
		}
		if err := d.installNode(item.node, item.controller); err != nil {
			return results, err
		}
		role := "EXECUTION"
		if item.controller {
			role = "PRIMARY-CONTROL"
		} else if hasCapability(item.node, model.CapabilityControl) {
			role = "CONTROL-STANDBY"
		}
		if item.gateway {
			role += "+GATEWAY"
		}
		results = append(results, RealmNodeResult{
			Node: item.node.Name, Address: item.node.ManagementIP,
			Role: role, Message: "installed",
		})
	}

	if err := d.seedRealm(primary, plan, opts); err != nil {
		return results, err
	}
	if !opts.StartServices {
		return results, nil
	}
	if err := d.startPrimary(primary); err != nil {
		return results, err
	}
	for _, item := range prepared {
		if item.node.Name == primary.Name {
			continue
		}
		if err := d.startWorker(item.node); err != nil {
			return results, err
		}
	}
	if plan.Ceph.Enabled && plan.Ceph.Provision {
		cephResult, err := NewCephDeployer().Deploy(plan)
		if err != nil {
			return results, fmt.Errorf("Titanus Realm is running but Ceph provisioning failed: %w", err)
		}
		results = append(results, RealmNodeResult{
			Node: "ceph", Address: cephResult.FSID, Role: "DISTRIBUTED-STORAGE",
			Message: fmt.Sprintf("installed:%d-osd:%s-usable", cephResult.OSDs, formatBytes(cephResult.UsableBytes)),
		})
	}
	return results, nil
}

func (d *RealmDeployer) stageNode(node model.NodeSpec, binaries map[string]string, ca, cert, key, daemonUnit, agentUnit, daemonEnv, agentEnv string) error {
	files := []struct {
		local  string
		remote string
	}{
		{binaries["titanus"], "/tmp/titanus"},
		{binaries["titanusd"], "/tmp/titanusd"},
		{binaries["titanus-agent"], "/tmp/titanus-agent"},
		{binaries["titanus-init"], "/tmp/titanus-init"},
		{ca, "/tmp/titanus-ca.crt"},
		{cert, "/tmp/titanus-node.crt"},
		{key, "/tmp/titanus-node.key"},
		{daemonUnit, "/tmp/titanusd.service"},
		{agentUnit, "/tmp/titanus-agent.service"},
		{daemonEnv, "/tmp/titanus-daemon.env"},
		{agentEnv, "/tmp/titanus-agent.env"},
	}
	for _, file := range files {
		if err := d.SSH.CopyFile(node, file.local, file.remote); err != nil {
			return fmt.Errorf("stage %s on %s: %w", filepath.Base(file.local), node.Name, err)
		}
	}
	return nil
}

func (d *RealmDeployer) installNode(node model.NodeSpec, installCAKey bool) error {
	commands := []string{
		remoteSudoPrefix,
		"$SUDO install -d -m 0755 /etc/titanus /etc/titanus/pki /var/lib/titanus /var/lib/titanus/realm /var/lib/titanus/units /var/lib/titanus/sources /var/lib/titanus/disks /var/lib/titanus/fabric /var/log/titanus /usr/local/libexec",
		"$SUDO install -m 0755 /tmp/titanus /usr/local/bin/titanus",
		"$SUDO install -m 0755 /tmp/titanusd /usr/local/sbin/titanusd",
		"$SUDO install -m 0755 /tmp/titanus-agent /usr/local/sbin/titanus-agent",
		"$SUDO install -m 0755 /tmp/titanus-init /usr/local/libexec/titanus-init",
		"$SUDO install -m 0644 /tmp/titanus-ca.crt /etc/titanus/pki/ca.crt",
		"$SUDO install -m 0644 /tmp/titanus-node.crt /etc/titanus/pki/node.crt",
		"$SUDO install -m 0600 /tmp/titanus-node.key /etc/titanus/pki/node.key",
		"$SUDO install -m 0600 /tmp/titanus-daemon.env /etc/titanus/daemon.env",
		"$SUDO install -m 0600 /tmp/titanus-agent.env /etc/titanus/agent.env",
		"$SUDO install -m 0644 /tmp/titanusd.service /etc/systemd/system/titanusd.service",
		"$SUDO install -m 0644 /tmp/titanus-agent.service /etc/systemd/system/titanus-agent.service",
	}
	if installCAKey {
		commands = append(commands, "$SUDO install -m 0600 /tmp/titanus-ca.key /etc/titanus/pki/ca.key")
	}
	commands = append(commands,
		"$SUDO systemctl daemon-reload",
		"$SUDO systemctl enable titanusd.service titanus-agent.service",
		"$SUDO rm -f /tmp/titanus /tmp/titanusd /tmp/titanus-agent /tmp/titanus-init /tmp/titanus-ca.crt /tmp/titanus-node.crt /tmp/titanus-node.key /tmp/titanus-daemon.env /tmp/titanus-agent.env /tmp/titanusd.service /tmp/titanus-agent.service /tmp/titanus-ca.key",
	)
	if _, err := d.SSH.Run(node, strings.Join(commands, "; ")); err != nil {
		return fmt.Errorf("install Titanus on %s: %w", node.Name, err)
	}
	return nil
}

func (d *RealmDeployer) seedRealm(primary model.NodeSpec, plan model.RealmPlan, opts RealmDeployOptions) error {
	cmd := fmt.Sprintf(
		"%s; $SUDO env TITANUS_STATE_ROOT=/var/lib/titanus /usr/local/bin/titanus realm seed --name %s --fabric-cidr %s --service-cidr %s --node-prefix %d --vxlan-id %d",
		remoteSudoPrefix,
		shellQuote(plan.RealmName), shellQuote(plan.FabricCIDR), shellQuote(plan.ServiceCIDR),
		opts.NodePrefix, opts.VXLANID,
	)
	if _, err := d.SSH.Run(primary, cmd); err != nil {
		return fmt.Errorf("seed Realm on %s: %w", primary.Name, err)
	}
	return nil
}

func (d *RealmDeployer) startPrimary(primary model.NodeSpec) error {
	cmd := remoteSudoPrefix + "; $SUDO systemctl restart titanusd.service; " +
		"$SUDO systemctl restart titanus-agent.service; " +
		"$SUDO systemctl --no-pager --full is-active titanusd.service titanus-agent.service"
	if _, err := d.SSH.Run(primary, cmd); err != nil {
		return fmt.Errorf("start primary controller %s: %w", primary.Name, err)
	}
	return nil
}

func (d *RealmDeployer) startWorker(node model.NodeSpec) error {
	cmd := remoteSudoPrefix + "; $SUDO systemctl restart titanusd.service; " +
		"$SUDO systemctl restart titanus-agent.service; " +
		"$SUDO systemctl --no-pager --full is-active titanusd.service titanus-agent.service"
	if _, err := d.SSH.Run(node, cmd); err != nil {
		return fmt.Errorf("start Titanus Node %s: %w", node.Name, err)
	}
	return nil
}

func primaryController(plan model.RealmPlan) (model.NodeSpec, error) {
	for _, node := range plan.Nodes {
		if hasCapability(node, model.CapabilityControl) {
			return node, nil
		}
	}
	return model.NodeSpec{}, fmt.Errorf("Realm Plan contains no CONTROL node")
}

func hasCapability(node model.NodeSpec, wanted model.Capability) bool {
	for _, capability := range node.Capabilities {
		if capability == wanted {
			return true
		}
	}
	return false
}

func daemonEnvironment(plan model.RealmPlan, node, primary model.NodeSpec, opts RealmDeployOptions, controller, gateway bool) string {
	return fmt.Sprintf(
		"TITANUS_STATE_ROOT=/var/lib/titanus\nTITANUS_REALM_NAME=%s\nTITANUS_NODE_ID=%s\nTITANUS_CONTROLLER_MODE=%t\nTITANUS_GATEWAY_MODE=%t\nTITANUS_CONTROLLER_ENDPOINT=https://%s:%d\nTITANUS_CLUSTER_LISTEN=0.0.0.0:%d\nTITANUS_CA=/etc/titanus/pki/ca.crt\nTITANUS_CERT=/etc/titanus/pki/node.crt\nTITANUS_KEY=/etc/titanus/pki/node.key\n",
		plan.RealmName, node.Name, controller, gateway, controllerAddress(plan, primary), opts.ClusterPort, opts.ClusterPort,
	)
}

func agentEnvironment(plan model.RealmPlan, node, primary model.NodeSpec, opts RealmDeployOptions) string {
	caps := make([]string, 0, len(node.Capabilities))
	for _, capability := range node.Capabilities {
		caps = append(caps, string(capability))
	}
	sort.Strings(caps)
	fabricAddress := node.FabricIP
	if fabricAddress == "" {
		fabricAddress = node.ManagementIP
	}
	return fmt.Sprintf(
		"TITANUS_NODE_ID=%s\nTITANUS_NODE_ADDRESS=%s\nTITANUS_NODE_FABRIC_ADDRESS=%s\nTITANUS_CONTROLLER=https://%s:%d\nTITANUS_CA=/etc/titanus/pki/ca.crt\nTITANUS_CERT=/etc/titanus/pki/node.crt\nTITANUS_KEY=/etc/titanus/pki/node.key\nTITANUS_CAPABILITIES=%s\n",
		node.Name, node.ManagementIP, fabricAddress, controllerAddress(plan, primary), opts.ClusterPort, strings.Join(caps, ","),
	)
}

func controllerAddress(plan model.RealmPlan, primary model.NodeSpec) string {
	if strings.TrimSpace(plan.ControlVIP) != "" {
		return strings.TrimSpace(plan.ControlVIP)
	}
	return primary.ManagementIP
}

func validateEnvValue(value string) error {
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("invalid newline/NUL in environment value")
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

const remoteSudoPrefix = "if [ \"$(id -u)\" = 0 ]; then SUDO=''; else SUDO='sudo -n'; fi"

const realmDaemonService = "[Unit]\n" +
	"Description=Titanus Realm Daemon\n" +
	"After=network-online.target\n" +
	"Wants=network-online.target\n\n" +
	"[Service]\n" +
	"Type=simple\n" +
	"EnvironmentFile=/etc/titanus/daemon.env\n" +
	"ExecStart=/usr/local/sbin/titanusd\n" +
	"Restart=always\n" +
	"RestartSec=3\n" +
	"RuntimeDirectory=titanus\n" +
	"RuntimeDirectoryMode=0755\n" +
	"LimitNOFILE=1048576\n" +
	"NoNewPrivileges=no\n\n" +
	"[Install]\n" +
	"WantedBy=multi-user.target\n"

const realmAgentService = "[Unit]\n" +
	"Description=Titanus Realm Node Agent\n" +
	"After=network-online.target titanusd.service\n" +
	"Wants=network-online.target\n\n" +
	"[Service]\n" +
	"Type=simple\n" +
	"EnvironmentFile=/etc/titanus/agent.env\n" +
	"ExecStart=/usr/local/sbin/titanus-agent --node=$" + "{TITANUS_NODE_ID} --address=$" + "{TITANUS_NODE_ADDRESS} --fabric-address=$" + "{TITANUS_NODE_FABRIC_ADDRESS} --controller=$" + "{TITANUS_CONTROLLER} --ca=$" + "{TITANUS_CA} --cert=$" + "{TITANUS_CERT} --key=$" + "{TITANUS_KEY} --capabilities=$" + "{TITANUS_CAPABILITIES}\n" +
	"Restart=always\n" +
	"RestartSec=5\n" +
	"NoNewPrivileges=true\n" +
	"ProtectSystem=strict\n" +
	"ProtectHome=true\n" +
	"ReadWritePaths=/var/lib/titanus /var/log/titanus\n" +
	"PrivateTmp=true\n\n" +
	"[Install]\n" +
	"WantedBy=multi-user.target\n"
