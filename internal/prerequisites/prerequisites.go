// Package prerequisites installs the OS tools required by an explicitly started
// installation. Offline validation, preparation and discovery never call it.
package prerequisites

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/remote"
)

var basePackages = []string{"ca-certificates", "coreutils", "gzip", "iproute2", "kmod", "nftables", "openssh-client", "python3", "systemd", "tar", "util-linux"}

func Packages(plan model.RealmPlan, node model.NodeSpec) []string {
	packages := append([]string{}, basePackages...)
	storage, control := false, false
	for _, c := range node.Capabilities {
		storage = storage || c == model.CapabilityStorage
		control = control || c == model.CapabilityControl
	}
	if plan.Ceph.Enabled {
		packages = append(packages, "ceph-common")
		if storage && plan.Ceph.Provision {
			packages = append(packages, "ceph-mon", "ceph-mgr", "ceph-osd", "lvm2")
			if plan.Ceph.EnableCephFS {
				packages = append(packages, "ceph-mds")
			}
			if plan.Ceph.EnableRGW {
				packages = append(packages, "radosgw")
			}
		}
	}
	if control && plan.Installation != nil && plan.Installation.Profile == model.ProfileNativeHA {
		for _, target := range plan.Installation.PowerFencing {
			switch target.Provider {
			case "ipmi":
				packages = append(packages, "ipmitool")
			case "libvirt":
				packages = append(packages, "libvirt-clients")
			}
		}
	}
	sort.Strings(packages)
	out := packages[:0]
	for _, p := range packages {
		if len(out) == 0 || out[len(out)-1] != p {
			out = append(out, p)
		}
	}
	return out
}

func script(packages, modules []string, moduleFile string) string {
	return dependencyScript(packages, modules, moduleFile, false, false)
}

func dependencyScript(packages, modules []string, moduleFile string, autoRepair, reinstall bool) string {
	// Package/module names are fixed internal constants, never operator input.
	repair := "0"
	if autoRepair {
		repair = "1"
	}
	reinstallFlag := ""
	if reinstall {
		reinstallFlag = "--reinstall "
	}
	s := `set -eu
export LC_ALL=C
test "$(id -u)" = 0 || { echo 'Dependency installation requires root or passwordless sudo' >&2; exit 1; }
test "$(uname -s)" = Linux && test -r /etc/debian_version || { echo 'Automatic dependencies require a Debian-family Linux host' >&2; exit 1; }
command -v apt-get >/dev/null && command -v dpkg-query >/dev/null
auto_repair=` + repair + `
# Only APT transport/lock failures are retried, once. Never remove locks, alter
# repositories, disable verification or replay failing package maintainer scripts.
titanus_apt() {
    apt_log=$(mktemp)
    apt_attempt=0
    while :; do
        if DEBIAN_FRONTEND=noninteractive apt-get "$@" -o DPkg::Lock::Timeout=120 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 >"$apt_log" 2>&1; then
            cat "$apt_log"
            rm -f "$apt_log"
            if [ "$apt_attempt" = 1 ]; then echo 'TITANUS_REPAIR APT retry succeeded'; fi
            return 0
        else
            apt_status=$?
        fi
        cat "$apt_log" >&2
        if [ "$auto_repair" = 1 ] && [ "$apt_attempt" = 0 ] &&
           grep -Eiq 'Temporary failure resolving|Could not resolve|Could not connect|Unable to connect|Connection (failed|timed out|refused)|Could not get lock|Unable to acquire.*lock|is another process using it' "$apt_log" &&
           ! grep -Eiq 'NO_PUBKEY|not signed|signature|Hash Sum mismatch|Certificate verification failed|Authentication failed|dpkg:|error processing|Sub-process' "$apt_log"; then
            echo 'TITANUS_REPAIR APT transient transport/lock failure; waiting 2s, retry 1/1'
            apt_attempt=1
            sleep 2
        else
            if [ "$apt_attempt" = 1 ]; then echo 'TITANUS_REPAIR APT retry exhausted; installation blocked'; fi
            rm -f "$apt_log"
            return "$apt_status"
        fi
    done
}
set --
for package in ` + strings.Join(packages, " ") + `; do
    if [ '` + fmt.Sprint(reinstall) + `' = true ] || [ "$(dpkg-query -W -f='${Status}' "$package" 2>/dev/null || true)" != 'install ok installed' ]; then
        set -- "$@" "$package"
    fi
done
if [ "$#" -gt 0 ]; then
    printf 'Installing required packages:'; printf ' %s' "$@"; printf '\n'
    titanus_apt update -qq -o APT::Update::Error-Mode=any
    titanus_apt install -y --no-install-recommends --no-upgrade ` + reinstallFlag + `"$@"
else
    printf 'All required packages are already installed.\n'
fi
`
	if len(modules) > 0 {
		s += `test ! -L '` + moduleFile + `' || { echo 'Refusing symlinked Titanus module configuration' >&2; exit 1; }
install -d -m 0755 "$(dirname '` + moduleFile + `')"
for module in ` + strings.Join(modules, " ") + `; do
    if ! modprobe "$module"; then
        if [ "$auto_repair" != 1 ]; then exit 1; fi
        echo "TITANUS_REPAIR Module $module failed; rebuild module indexes and retry 1/1"
        depmod -a
        if ! modprobe "$module"; then
            echo "TITANUS_REPAIR Module $module retry exhausted; installation blocked"
            exit 1
        fi
        echo "TITANUS_REPAIR Module $module loaded after index repair"
    fi
    if ! test -f '` + moduleFile + `' || ! grep -qxF "$module" '` + moduleFile + `'; then
        printf '%s\n' "$module" >> '` + moduleFile + `'
    fi
done
chmod 0644 '` + moduleFile + `'
`
	}
	return s + "echo TITANUS_DEPENDENCIES_OK\n"
}

func InstallerScript() string {
	return script([]string{"ca-certificates", "openssh-client", "python3", "sshpass", "util-linux"}, nil, "")
}

func TargetScript(plan model.RealmPlan, node model.NodeSpec) string {
	return dependencyScript(Packages(plan, node), []string{"overlay", "bridge", "veth", "vxlan"}, "/etc/modules-load.d/titanus-runtime.conf", RepairEnabled(plan), false)
}

func EnsureInstaller(ctx context.Context) (string, error) {
	return EnsureInstallerWithRepair(ctx, false)
}

func EnsureInstallerWithRepair(ctx context.Context, enabled bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", dependencyScript([]string{"ca-certificates", "openssh-client", "python3", "sshpass", "util-linux"}, nil, "", enabled, false)).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("installer-host dependency installation failed: %w", err)
	}
	return string(out), nil
}

type Runner interface {
	Run(model.NodeSpec, string) (remote.Result, error)
}

// EnsureNodes runs only during explicit apply. Guard all selected nodes against
// overwriting an existing Realm before any package or kernel-module changes.
func EnsureNodes(runner Runner, plan model.RealmPlan) error {
	return EnsureNodesWithLog(runner, plan, nil)
}

func EnsureNodesWithLog(runner Runner, plan model.RealmPlan, logf func(string, ...any)) error {
	if plan.Installation != nil && plan.Installation.Operation == "rollback" {
		return nil
	}
	if plan.Installation != nil && plan.Installation.Operation == "install" {
		for _, node := range plan.Nodes {
			guard := `set -eu; if [ "$(id -u)" = 0 ]; then SUDO=''; else SUDO='sudo -n'; fi; $SUDO true; for path in /etc/titanus/daemon.env /etc/titanus/agent.env /etc/titanus/ha.json /etc/titanus/power-fencing.json /etc/titanus/pki /var/lib/titanus/realm; do if $SUDO test -e "$path" || $SUDO test -L "$path"; then echo 'Initial installation refuses existing Realm state/configuration' >&2; exit 1; fi; done`
			if _, err := runner.Run(node, guard); err != nil {
				return fmt.Errorf("fresh-install dependency guard on %s: %w", node.Name, err)
			}
		}
	}
	for _, node := range plan.Nodes {
		result, err := runner.Run(node, SudoScript(TargetScript(plan, node)))
		LogRepairs(node.Name, result.Stdout, logf)
		if err != nil {
			return fmt.Errorf("dependency installation on %s: %w", node.Name, err)
		}
	}
	return nil
}

func RepairEnabled(plan model.RealmPlan) bool {
	return plan.Installation != nil && plan.Installation.AutoRepair && plan.Installation.Operation != "rollback"
}

// RepairScript accepts only named checks with a fixed, idempotent remedy. The
// caller must re-run the complete pre-flight after repair, before any payload.
func RepairScript(check string) (string, bool) {
	packages := map[string]string{"iproute2": "iproute2", "nftables": "nftables", "nsenter": "util-linux"}
	if pkg, ok := packages[check]; ok {
		return SudoScript(dependencyScript([]string{pkg}, nil, "", false, true)), true
	}
	if check == "OverlayFS" {
		return SudoScript(dependencyScript(nil, []string{"overlay"}, "/etc/modules-load.d/titanus-runtime.conf", false, false)), true
	}
	return "", false
}

func SudoScript(script string) string {
	return `set -eu; if [ "$(id -u)" = 0 ]; then SUDO=''; else SUDO='sudo -n'; fi; $SUDO sh -c '` + strings.ReplaceAll(script, "'", "'\"'\"'") + `'`
}

// Preserve concise repair events even when verbose package output is truncated.
func LogRepairs(node, output string, logf func(string, ...any)) {
	if logf == nil {
		return
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "TITANUS_REPAIR ") {
			logf("[%s] %s", node, line)
		}
	}
}
