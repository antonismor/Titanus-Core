package deploy

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/remote"
)

// Each provisioning command must stop on the first failed bootstrap step.
const cephSudoPrefix = "set -e; " + remoteSudoPrefix

type CephDeployResult struct {
	FSID        string
	Monitors    int
	OSDs        int
	RawBytes    uint64
	UsableBytes uint64
	RBD         bool
	CephFS      bool
	RGW         bool
}

type CephDeployer struct {
	SSH *remote.SSHExecutor
}

func NewCephDeployer() *CephDeployer {
	executor := remote.NewSSHExecutor()
	executor.CommandTimeout = 20 * time.Minute
	return &CephDeployer{SSH: executor}
}

func (d *CephDeployer) Deploy(plan model.RealmPlan) (CephDeployResult, error) {
	plan.Normalize()
	if err := plan.Validate(); err != nil {
		return CephDeployResult{}, err
	}
	if !plan.Ceph.Enabled || !plan.Ceph.Provision {
		return CephDeployResult{}, fmt.Errorf("Ceph automatic provisioning is not enabled in this Plan")
	}
	storage := plan.StorageNodes()
	if len(storage) == 0 {
		return CephDeployResult{}, fmt.Errorf("Ceph requires at least one STORAGE node")
	}

	rawBytes, osdCount, err := d.validateDevices(storage)
	if err != nil {
		return CephDeployResult{}, err
	}
	usable := rawBytes / uint64(plan.Ceph.Replication)
	requested := uint64(plan.Ceph.RequestedTB) * 1000 * 1000 * 1000 * 1000
	if requested > 0 && usable < requested {
		return CephDeployResult{}, fmt.Errorf(
			"selected Ceph devices provide approximately %s usable with replication %d, below requested %d TB",
			formatBytes(usable), plan.Ceph.Replication, plan.Ceph.RequestedTB,
		)
	}

	if err := d.installPackages(plan); err != nil {
		return CephDeployResult{}, err
	}
	if err := d.prepareManagerCompatibility(storage); err != nil {
		return CephDeployResult{}, err
	}
	primary := storage[0]

	fsid, err := d.ensureFSID(primary)
	if err != nil {
		return CephDeployResult{}, err
	}

	tempDir, err := os.MkdirTemp("", "titanus-ceph-*")
	if err != nil {
		return CephDeployResult{}, err
	}
	defer os.RemoveAll(tempDir)

	confPath := filepath.Join(tempDir, "ceph.conf")
	if err := os.WriteFile(confPath, []byte(buildCephConfig(plan, fsid)), 0644); err != nil {
		return CephDeployResult{}, err
	}
	if err := d.distributeConfig(plan.Nodes, confPath); err != nil {
		return CephDeployResult{}, err
	}

	if err := d.ensureBootstrapArtifacts(primary, storage, fsid); err != nil {
		return CephDeployResult{}, err
	}

	artifacts := map[string]string{
		"mon":       filepath.Join(tempDir, "ceph.mon.keyring"),
		"admin":     filepath.Join(tempDir, "ceph.client.admin.keyring"),
		"bootstrap": filepath.Join(tempDir, "ceph.bootstrap-osd.keyring"),
		"monmap":    filepath.Join(tempDir, "monmap"),
	}
	remoteArtifacts := map[string]string{
		"mon":       "/var/lib/titanus/ceph/bootstrap/ceph.mon.keyring",
		"admin":     "/var/lib/titanus/ceph/bootstrap/ceph.client.admin.keyring",
		"bootstrap": "/var/lib/titanus/ceph/bootstrap/ceph.bootstrap-osd.keyring",
		"monmap":    "/var/lib/titanus/ceph/bootstrap/monmap",
	}
	for key, remotePath := range remoteArtifacts {
		if err := d.SSH.FetchFile(primary, remotePath, artifacts[key]); err != nil {
			return CephDeployResult{}, fmt.Errorf("fetch Ceph bootstrap artifact %s: %w", key, err)
		}
	}
	if err := d.distributeBootstrap(storage, artifacts); err != nil {
		return CephDeployResult{}, err
	}
	if err := d.startMonitors(storage); err != nil {
		return CephDeployResult{}, err
	}
	if err := d.waitForQuorum(primary); err != nil {
		return CephDeployResult{}, err
	}
	if err := d.hardenMonitors(primary); err != nil {
		return CephDeployResult{}, err
	}
	// Ceph Squid 19.2.6+ introduces aes256k for CephX. Prefer it
	// before any new OSD, MGR, MDS or client credentials are created.
	if _, err := d.SSH.Run(primary, cephSudoPrefix+"; if $SUDO ceph config help auth_preferred_cipher >/dev/null 2>&1; then $SUDO ceph config set mon auth_preferred_cipher aes256k; fi"); err != nil {
		return CephDeployResult{}, fmt.Errorf("set secure CephX cipher before provisioning: %w", err)
	}
	if err := d.startManagers(storage); err != nil {
		return CephDeployResult{}, err
	}
	if err := d.provisionOSDs(storage); err != nil {
		return CephDeployResult{}, err
	}
	if err := d.configurePools(primary, plan); err != nil {
		return CephDeployResult{}, err
	}
	if plan.Ceph.EnableCephFS {
		if err := d.startMDS(storage); err != nil {
			return CephDeployResult{}, err
		}
		if err := d.waitForCephFSVolumeAPI(primary); err != nil {
			return CephDeployResult{}, err
		}
	}
	if plan.Ceph.EnableRGW {
		if err := d.startRGW(primary); err != nil {
			return CephDeployResult{}, err
		}
	}
	if err := d.configureTitanusClient(primary, plan.Nodes, tempDir); err != nil {
		return CephDeployResult{}, err
	}
	if _, err := d.SSH.Run(primary, cephSudoPrefix+"; $SUDO install -d -m 0755 /var/lib/titanus/ceph; echo "+shellQuote(fsid)+" | $SUDO tee /var/lib/titanus/ceph/provisioned >/dev/null"); err != nil {
		return CephDeployResult{}, err
	}

	return CephDeployResult{
		FSID: fsid, Monitors: len(storage), OSDs: osdCount,
		RawBytes: rawBytes, UsableBytes: usable,
		RBD: plan.Ceph.EnableRBD, CephFS: plan.Ceph.EnableCephFS, RGW: plan.Ceph.EnableRGW,
	}, nil
}

func (d *CephDeployer) validateDevices(nodes []model.NodeSpec) (uint64, int, error) {
	var raw uint64
	count := 0
	for _, node := range nodes {
		if len(node.CephDevices) == 0 {
			return 0, 0, fmt.Errorf("storage node %s has no selected Ceph devices", node.Name)
		}
		for _, device := range node.CephDevices {
			cmd := cephSudoPrefix + "; " +
				"test -b " + shellQuote(device) + "; " +
				"test -z \"$(lsblk -n -o MOUNTPOINT " + shellQuote(device) + " | tr -d '[:space:]')\"; " +
				"blockdev --getsize64 " + shellQuote(device)
			result, err := d.SSH.Run(node, cmd)
			if err != nil {
				return 0, 0, fmt.Errorf("validate Ceph device %s on %s: %w", device, node.Name, err)
			}
			lines := strings.Fields(result.Stdout)
			if len(lines) == 0 {
				return 0, 0, fmt.Errorf("no size returned for %s on %s", device, node.Name)
			}
			size, err := strconv.ParseUint(lines[len(lines)-1], 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("parse size of %s on %s: %w", device, node.Name, err)
			}
			raw += size
			count++
		}
	}
	return raw, count, nil
}

func (d *CephDeployer) installPackages(plan model.RealmPlan) error {
	storageNames := map[string]bool{}
	for _, node := range plan.StorageNodes() {
		storageNames[node.Name] = true
	}
	for _, node := range plan.Nodes {
		packages := []string{"ceph-common"}
		if storageNames[node.Name] {
			packages = append(packages, "ceph-mon", "ceph-mgr", "ceph-osd", "lvm2")
			if plan.Ceph.EnableCephFS {
				packages = append(packages, "ceph-mds")
			}
			if plan.Ceph.EnableRGW {
				packages = append(packages, "radosgw")
			}
		}
		cmd := cephSudoPrefix + "; " +
			"$SUDO apt-get update -qq; " +
			"$SUDO env DEBIAN_FRONTEND=noninteractive apt-get install -y " + strings.Join(packages, " ")
		if _, err := d.SSH.Run(node, cmd); err != nil {
			return fmt.Errorf("install Ceph packages on %s: %w", node.Name, err)
		}
	}
	return nil
}

func cephReefManagerCompatibilityCommand() string {
	return cephSudoPrefix + `; mgr=/usr/share/ceph/mgr/mgr_util.py; marker='# Titanus Debian 13 Reef PyO3 compatibility'; changed=0; if { grep -Eq '^VERSION_ID="?13"?$' /etc/os-release && ceph --version | grep -q '^ceph version 18\.2\.7 '; }; then py=$(python3 -c 'import sys; print("%d.%d" % (sys.version_info.major, sys.version_info.minor))'); if [ "$py" != 3.13 ]; then echo 'unexpected Python for guarded Debian 13 Ceph Reef compatibility' >&2; exit 1; fi; if $SUDO grep -qx "$marker" "$mgr"; then :; elif [ "$($SUDO grep -cx '^import bcrypt$' "$mgr")" = 1 ] && [ "$($SUDO grep -c '^def password_hash(' "$mgr")" = 1 ]; then if ! $SUDO test -e "$mgr.titanus-original"; then $SUDO cp -a "$mgr" "$mgr.titanus-original"; fi; $SUDO sed -i 's/^import bcrypt$/# Titanus Debian 13 Reef PyO3 compatibility/' "$mgr"; $SUDO sed -i '/^def password_hash(/a\    import bcrypt' "$mgr"; changed=1; else echo 'refusing unknown Debian 13 Ceph Reef mgr_util.py layout' >&2; exit 1; fi; $SUDO grep -qx "$marker" "$mgr"; $SUDO sed -n '/^def password_hash(/,+2p' "$mgr" | grep -q '^    import bcrypt$'; if [ "$changed" = 1 ]; then for unit in $($SUDO systemctl list-units --type=service --state=running --no-legend 'ceph-mgr@*.service' | awk '{print $1}'); do $SUDO systemctl restart "$unit"; done; fi; fi`
}

// Debian 13 ships Ceph Reef 18.2.7 with a Python 3.13 bcrypt/PyO3
// incompatibility: mgr_util imports bcrypt in every ceph-mgr subinterpreter,
// which prevents required modules such as volumes and rbd_support from loading.
// Upstream Ceph moved crypto work out of subinterpreters in newer releases.
// This narrow, idempotent shim keeps bcrypt lazy on the affected package only;
// unknown layouts are refused rather than modified.
func (d *CephDeployer) prepareManagerCompatibility(nodes []model.NodeSpec) error {
	cmd := cephReefManagerCompatibilityCommand()
	for _, node := range nodes {
		if _, err := d.SSH.Run(node, cmd); err != nil {
			return fmt.Errorf("prepare Ceph manager compatibility on %s: %w", node.Name, err)
		}
	}
	return nil
}

func (d *CephDeployer) ensureFSID(primary model.NodeSpec) (string, error) {
	read, err := d.SSH.Run(primary, cephSudoPrefix+"; $SUDO test -r /var/lib/titanus/ceph/fsid && $SUDO cat /var/lib/titanus/ceph/fsid || true")
	if err == nil {
		if value := strings.TrimSpace(read.Stdout); value != "" {
			return value, nil
		}
	}
	fsid, err := randomUUID()
	if err != nil {
		return "", err
	}
	cmd := cephSudoPrefix + "; $SUDO install -d -m 0755 /var/lib/titanus/ceph; echo " +
		shellQuote(fsid) + " | $SUDO tee /var/lib/titanus/ceph/fsid >/dev/null"
	if _, err := d.SSH.Run(primary, cmd); err != nil {
		return "", err
	}
	return fsid, nil
}

func (d *CephDeployer) distributeConfig(nodes []model.NodeSpec, local string) error {
	for _, node := range nodes {
		if err := d.SSH.CopyFile(node, local, "/tmp/titanus-ceph.conf"); err != nil {
			return err
		}
		cmd := cephSudoPrefix + "; $SUDO install -d -m 0755 /etc/ceph; " +
			"$SUDO install -m 0644 /tmp/titanus-ceph.conf /etc/ceph/ceph.conf; rm -f /tmp/titanus-ceph.conf"
		if _, err := d.SSH.Run(node, cmd); err != nil {
			return fmt.Errorf("install ceph.conf on %s: %w", node.Name, err)
		}
	}
	return nil
}

func (d *CephDeployer) ensureBootstrapArtifacts(primary model.NodeSpec, monitors []model.NodeSpec, fsid string) error {
	var monmapArgs strings.Builder
	for _, node := range monitors {
		fmt.Fprintf(&monmapArgs, " --add %s %s", shellQuote(node.Name), shellQuote(cephPublicIP(node)))
	}
	cmd := cephSudoPrefix + "; " +
		"$SUDO install -d -m 0700 /var/lib/titanus/ceph/bootstrap /var/lib/ceph/bootstrap-osd; " +
		"keyType=''; if $SUDO ceph-authtool --help 2>&1 | grep -q -- '--key-type'; then keyType='--key-type aes256k'; fi; " +
		"if [ ! -s /var/lib/titanus/ceph/bootstrap/ceph.mon.keyring ]; then " +
		"$SUDO ceph-authtool --create-keyring /var/lib/titanus/ceph/bootstrap/ceph.mon.keyring $keyType --gen-key -n mon.; fi; " +
		"if [ ! -s /var/lib/titanus/ceph/bootstrap/ceph.client.admin.keyring ]; then " +
		"$SUDO ceph-authtool --create-keyring /var/lib/titanus/ceph/bootstrap/ceph.client.admin.keyring $keyType --gen-key -n client.admin --cap mon 'allow *' --cap osd 'allow *' --cap mds 'allow *' --cap mgr 'allow *'; fi; " +
		"if [ ! -s /var/lib/titanus/ceph/bootstrap/ceph.bootstrap-osd.keyring ]; then " +
		"$SUDO ceph-authtool --create-keyring /var/lib/titanus/ceph/bootstrap/ceph.bootstrap-osd.keyring $keyType --gen-key -n client.bootstrap-osd --cap mon 'profile bootstrap-osd' --cap mgr 'allow r'; fi; " +
		"$SUDO ceph-authtool /var/lib/titanus/ceph/bootstrap/ceph.mon.keyring --import-keyring /var/lib/titanus/ceph/bootstrap/ceph.client.admin.keyring; " +
		"$SUDO ceph-authtool /var/lib/titanus/ceph/bootstrap/ceph.mon.keyring --import-keyring /var/lib/titanus/ceph/bootstrap/ceph.bootstrap-osd.keyring; " +
		cephMonmapCreateCommand(monmapArgs.String(), fsid)
	if _, err := d.SSH.Run(primary, cmd); err != nil {
		return fmt.Errorf("create Ceph bootstrap artifacts on %s: %w", primary.Name, err)
	}
	return nil
}

func cephMonmapCreateCommand(monmapArgs, fsid string) string {
	return "$SUDO monmaptool --create --clobber" + monmapArgs + " --fsid " + shellQuote(fsid) + " /var/lib/titanus/ceph/bootstrap/monmap"
}

func (d *CephDeployer) distributeBootstrap(nodes []model.NodeSpec, artifacts map[string]string) error {
	for _, node := range nodes {
		for key, local := range artifacts {
			if err := d.SSH.CopyFile(node, local, "/tmp/titanus-ceph-"+key); err != nil {
				return err
			}
		}
		cmd := cephSudoPrefix + "; " +
			"$SUDO install -d -m 0755 /var/lib/ceph/bootstrap-osd /etc/ceph; " +
			"$SUDO install -m 0600 /tmp/titanus-ceph-admin /etc/ceph/ceph.client.admin.keyring; " +
			"$SUDO install -m 0600 /tmp/titanus-ceph-bootstrap /var/lib/ceph/bootstrap-osd/ceph.keyring; " +
			"$SUDO install -m 0600 /tmp/titanus-ceph-mon /var/lib/titanus-ceph-mon.keyring; " +
			"$SUDO install -m 0644 /tmp/titanus-ceph-monmap /var/lib/titanus-ceph-monmap; " +
			"$SUDO chown ceph:ceph /var/lib/titanus-ceph-mon.keyring /var/lib/titanus-ceph-monmap; " +
			"rm -f /tmp/titanus-ceph-admin /tmp/titanus-ceph-bootstrap /tmp/titanus-ceph-mon /tmp/titanus-ceph-monmap"
		if _, err := d.SSH.Run(node, cmd); err != nil {
			return fmt.Errorf("install Ceph bootstrap artifacts on %s: %w", node.Name, err)
		}
	}
	return nil
}

func (d *CephDeployer) startMonitors(nodes []model.NodeSpec) error {
	for _, node := range nodes {
		cmd := cephMonitorCommand(node)
		if _, err := d.SSH.Run(node, cmd); err != nil {
			return fmt.Errorf("start Ceph monitor %s: %w", node.Name, err)
		}
	}
	return nil
}

func cephMonitorCommand(node model.NodeSpec) string {
	return cephSudoPrefix + "; " +
		"$SUDO install -d -o ceph -g ceph -m 0750 /var/lib/ceph/mon/ceph-" + shellQuote(node.Name) + "; " +
		"if [ ! -s /var/lib/ceph/mon/ceph-" + shellQuote(node.Name) + "/keyring ]; then " +
		"$SUDO runuser -u ceph -- ceph-mon --mkfs -i " + shellQuote(node.Name) +
		" --monmap /var/lib/titanus-ceph-monmap --keyring /var/lib/titanus-ceph-mon.keyring; fi; " +
		"$SUDO systemctl enable --now ceph-mon@" + shellQuote(node.Name)
}

func (d *CephDeployer) waitForQuorum(primary model.NodeSpec) error {
	cmd := cephSudoPrefix + "; " +
		"for i in $(seq 1 60); do if $SUDO timeout 10s ceph -s >/dev/null 2>&1; then exit 0; fi; sleep 2; done; " +
		"$SUDO timeout 10s ceph -s; exit 1"
	if _, err := d.SSH.Run(primary, cmd); err != nil {
		return fmt.Errorf("Ceph monitor quorum did not become healthy: %w", err)
	}
	return nil
}

func (d *CephDeployer) hardenMonitors(primary model.NodeSpec) error {
	cmd := cephSudoPrefix + "; " +
		"$SUDO ceph mon enable-msgr2; " +
		"$SUDO ceph config set mon auth_allow_insecure_global_id_reclaim false"
	if _, err := d.SSH.Run(primary, cmd); err != nil {
		return fmt.Errorf("harden Ceph monitor transport/authentication: %w", err)
	}
	return nil
}

func (d *CephDeployer) startManagers(nodes []model.NodeSpec) error {
	limit := len(nodes)
	if limit > 2 {
		limit = 2
	}
	for _, node := range nodes[:limit] {
		cmd := cephSudoPrefix + "; " +
			"$SUDO install -d -o ceph -g ceph -m 0750 /var/lib/ceph/mgr/ceph-" + shellQuote(node.Name) + "; " +
			"$SUDO ceph auth get-or-create mgr." + shellQuote(node.Name) +
			" mon 'allow profile mgr' osd 'allow *' mds 'allow *' -o /var/lib/ceph/mgr/ceph-" + shellQuote(node.Name) + "/keyring; " +
			"$SUDO chown -R ceph:ceph /var/lib/ceph/mgr/ceph-" + shellQuote(node.Name) + "; " +
			"$SUDO systemctl enable --now ceph-mgr@" + shellQuote(node.Name)
		if _, err := d.SSH.Run(node, cmd); err != nil {
			return fmt.Errorf("start Ceph manager %s: %w", node.Name, err)
		}
	}
	return nil
}

func (d *CephDeployer) provisionOSDs(nodes []model.NodeSpec) error {
	for _, node := range nodes {
		for _, device := range node.CephDevices {
			marker := "/var/lib/titanus/ceph/osd-" + sanitizeDevice(device)
			cmd := cephSudoPrefix + "; $SUDO install -d -m 0755 /var/lib/titanus/ceph; " +
				"if [ -f " + shellQuote(marker) + " ]; then exit 0; fi; " +
				"if $SUDO pvs --noheadings -o vg_name " + shellQuote(device) + " 2>/dev/null | grep -q 'ceph-'; then " +
				"echo existing > /tmp/titanus-osd-state; " +
				"else " +
				"$SUDO ceph-volume lvm zap --destroy " + shellQuote(device) + "; " +
				"$SUDO ceph-volume lvm create --data " + shellQuote(device) + "; " +
				"fi; " +
				"echo " + shellQuote(device) + " | $SUDO tee " + shellQuote(marker) + " >/dev/null"
			if _, err := d.SSH.Run(node, cmd); err != nil {
				return fmt.Errorf("provision Ceph OSD %s on %s: %w", device, node.Name, err)
			}
		}
	}
	return nil
}

func (d *CephDeployer) configurePools(primary model.NodeSpec, plan model.RealmPlan) error {
	commands := []string{cephSudoPrefix}
	rep := strconv.Itoa(plan.Ceph.Replication)
	if plan.Ceph.EnableRBD {
		commands = append(commands,
			"$SUDO ceph osd pool create titanus || true",
			"$SUDO ceph osd pool set titanus size "+rep,
			"$SUDO rbd pool init titanus",
		)
	}
	if plan.Ceph.EnableCephFS {
		commands = append(commands,
			"$SUDO ceph osd pool create titanusfs_meta || true",
			"$SUDO ceph osd pool create titanusfs_data || true",
			"$SUDO ceph osd pool set titanusfs_meta size "+rep,
			"$SUDO ceph osd pool set titanusfs_data size "+rep,
			"$SUDO ceph fs get titanusfs >/dev/null 2>&1 || $SUDO ceph fs new titanusfs titanusfs_meta titanusfs_data",
		)
	}
	if _, err := d.SSH.Run(primary, strings.Join(commands, "; ")); err != nil {
		return fmt.Errorf("configure Titanus Ceph pools: %w", err)
	}
	return nil
}

func (d *CephDeployer) startMDS(nodes []model.NodeSpec) error {
	limit := len(nodes)
	if limit > 2 {
		limit = 2
	}
	for _, node := range nodes[:limit] {
		cmd := cephSudoPrefix + "; " +
			"$SUDO install -d -o ceph -g ceph -m 0750 /var/lib/ceph/mds/ceph-" + shellQuote(node.Name) + "; " +
			"$SUDO ceph auth get-or-create mds." + shellQuote(node.Name) +
			" mon 'profile mds' mgr 'profile mds' mds 'allow *' osd 'allow rw tag cephfs *=*' -o /var/lib/ceph/mds/ceph-" + shellQuote(node.Name) + "/keyring; " +
			"$SUDO chown -R ceph:ceph /var/lib/ceph/mds/ceph-" + shellQuote(node.Name) + "; " +
			"$SUDO systemctl enable --now ceph-mds@" + shellQuote(node.Name)
		if _, err := d.SSH.Run(node, cmd); err != nil {
			return fmt.Errorf("start Ceph MDS %s: %w", node.Name, err)
		}
	}
	return nil
}

func (d *CephDeployer) waitForCephFSVolumeAPI(primary model.NodeSpec) error {
	cmd := cephSudoPrefix + "; " +
		"for i in $(seq 1 60); do " +
		"if $SUDO timeout 10s ceph fs subvolume ls titanusfs --format json >/dev/null 2>&1; then exit 0; fi; " +
		"sleep 2; done; " +
		"$SUDO timeout 10s ceph fs subvolume ls titanusfs --format json; exit 1"
	if _, err := d.SSH.Run(primary, cmd); err != nil {
		return fmt.Errorf("CephFS volumes manager API did not become available: %w", err)
	}
	return nil
}

func (d *CephDeployer) startRGW(node model.NodeSpec) error {
	id := "rgw." + node.Name
	cmd := cephSudoPrefix + "; " +
		"$SUDO install -d -o ceph -g ceph -m 0750 /var/lib/ceph/radosgw/ceph-" + shellQuote(id) + "; " +
		"$SUDO ceph auth get-or-create client." + shellQuote(id) +
		" mon 'allow rw' osd 'allow rwx' -o /var/lib/ceph/radosgw/ceph-" + shellQuote(id) + "/keyring; " +
		"$SUDO chown -R ceph:ceph /var/lib/ceph/radosgw/ceph-" + shellQuote(id) + "; " +
		"$SUDO ceph config set client." + shellQuote(id) + " rgw_frontends 'beast port=7480'; " +
		"$SUDO systemctl enable --now ceph-radosgw@" + shellQuote(id)
	if _, err := d.SSH.Run(node, cmd); err != nil {
		return fmt.Errorf("start Ceph RGW %s: %w", node.Name, err)
	}
	return nil
}

func (d *CephDeployer) configureTitanusClient(primary model.NodeSpec, allNodes []model.NodeSpec, tempDir string) error {
	remoteKeyring := "/var/lib/titanus/ceph/ceph.client.titanus.keyring"
	remoteFenceKeyring := "/var/lib/titanus/ceph/ceph.client.titanus-fence.keyring"
	cmd := cephSudoPrefix + "; $SUDO install -d -m 0755 /var/lib/titanus/ceph; " +
		"$SUDO ceph auth get-or-create client.titanus mon 'allow r' mgr 'allow rw' osd 'allow rwx' mds 'allow rw' -o " + remoteKeyring + "; " +
		"$SUDO ceph auth get-or-create client.titanus-fence mon 'allow r, allow command \"osd blocklist\" with blocklistop=add' osd 'allow *' mds 'allow *' -o " + remoteFenceKeyring
	if _, err := d.SSH.Run(primary, cmd); err != nil {
		return fmt.Errorf("create Titanus CephX identities: %w", err)
	}
	localKeyring := filepath.Join(tempDir, "ceph.client.titanus.keyring")
	localFenceKeyring := filepath.Join(tempDir, "ceph.client.titanus-fence.keyring")
	if err := d.SSH.FetchFile(primary, remoteKeyring, localKeyring); err != nil {
		return err
	}
	if err := d.SSH.FetchFile(primary, remoteFenceKeyring, localFenceKeyring); err != nil {
		return err
	}
	for _, node := range allNodes {
		if err := d.SSH.CopyFile(node, localKeyring, "/tmp/ceph.client.titanus.keyring"); err != nil {
			return err
		}
		if err := d.SSH.CopyFile(node, localFenceKeyring, "/tmp/ceph.client.titanus-fence.keyring"); err != nil {
			return err
		}
		cmd := cephSudoPrefix + "; " +
			"$SUDO install -m 0600 /tmp/ceph.client.titanus.keyring /etc/ceph/ceph.client.titanus.keyring; " +
			"$SUDO install -m 0600 /tmp/ceph.client.titanus-fence.keyring /etc/ceph/ceph.client.titanus-fence.keyring; " +
			"rm -f /tmp/ceph.client.titanus.keyring /tmp/ceph.client.titanus-fence.keyring; " +
			"$SUDO /usr/local/bin/titanus disk ceph-config --cluster ceph --pool titanus --fs titanusfs --client client.titanus --conf /etc/ceph/ceph.conf --keyring /etc/ceph/ceph.client.titanus.keyring --fence-client client.titanus-fence --fence-keyring /etc/ceph/ceph.client.titanus-fence.keyring"
		if _, err := d.SSH.Run(node, cmd); err != nil {
			return fmt.Errorf("configure Titanus Ceph adapter on %s: %w", node.Name, err)
		}
	}
	return nil
}

// When Ceph uses the management network, the optional dedicated Ceph IP
// may be omitted.  Monitor addresses must nevertheless be explicit.
func cephPublicIP(node model.NodeSpec) string {
	if node.CephPublicIP != "" {
		return node.CephPublicIP
	}
	return node.ManagementIP
}

func buildCephConfig(plan model.RealmPlan, fsid string) string {
	storage := plan.StorageNodes()
	names := make([]string, 0, len(storage))
	hosts := make([]string, 0, len(storage))
	for _, node := range storage {
		names = append(names, node.Name)
		hosts = append(hosts, cephPublicIP(node))
	}
	return fmt.Sprintf(
		"[global]\nfsid = %s\nmon_initial_members = %s\nmon_host = %s\npublic_network = %s\ncluster_network = %s\nauth_cluster_required = cephx\nauth_service_required = cephx\nauth_client_required = cephx\nosd_pool_default_size = %d\nmon_allow_pool_delete = false\n\n",
		fsid, strings.Join(names, ","), strings.Join(hosts, ","), plan.Ceph.PublicCIDR, plan.Ceph.ClusterCIDR, plan.Ceph.Replication,
	)
}

func randomUUID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(data[:])
	return hexValue[0:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" +
		hexValue[16:20] + "-" + hexValue[20:32], nil
}

func sanitizeDevice(device string) string {
	value := strings.TrimPrefix(device, "/dev/")
	value = strings.ReplaceAll(value, "/", "-")
	value = strings.ReplaceAll(value, " ", "_")
	return value
}

func formatBytes(value uint64) string {
	const tiB = uint64(1024 * 1024 * 1024 * 1024)
	const giB = uint64(1024 * 1024 * 1024)
	if value >= tiB {
		return fmt.Sprintf("%.2f TiB", float64(value)/float64(tiB))
	}
	return fmt.Sprintf("%.2f GiB", float64(value)/float64(giB))
}
