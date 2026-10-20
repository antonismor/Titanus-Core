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
	// Package/module names are fixed internal constants, never operator input.
	s := `set -eu
test "$(id -u)" = 0 || { echo 'Dependency installation requires root or passwordless sudo' >&2; exit 1; }
test "$(uname -s)" = Linux && test -r /etc/debian_version || { echo 'Automatic dependencies require a Debian-family Linux host' >&2; exit 1; }
command -v apt-get >/dev/null && command -v dpkg-query >/dev/null
set --
for package in ` + strings.Join(packages, " ") + `; do
    if [ "$(dpkg-query -W -f='${Status}' "$package" 2>/dev/null || true)" != 'install ok installed' ]; then
        set -- "$@" "$package"
    fi
done
if [ "$#" -gt 0 ]; then
    printf 'Installing required packages:'; printf ' %s' "$@"; printf '\n'
    apt-get update -qq -o APT::Update::Error-Mode=any
    DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=120 install -y --no-install-recommends --no-upgrade "$@"
else
    printf 'All required packages are already installed.\n'
fi
`
	if len(modules) > 0 {
		s += `test ! -L '` + moduleFile + `' || { echo 'Refusing symlinked Titanus module configuration' >&2; exit 1; }
install -d -m 0755 "$(dirname '` + moduleFile + `')"
for module in ` + strings.Join(modules, " ") + `; do
    modprobe "$module"
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
	return script(Packages(plan, node), []string{"overlay", "bridge", "veth", "vxlan"}, "/etc/modules-load.d/titanus-runtime.conf")
}

func EnsureInstaller(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", InstallerScript()).CombinedOutput()
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
		command := `set -eu; if [ "$(id -u)" = 0 ]; then SUDO=''; else SUDO='sudo -n'; fi; $SUDO sh -c '` + strings.ReplaceAll(TargetScript(plan, node), "'", "'\"'\"'") + `'`
		if _, err := runner.Run(node, command); err != nil {
			return fmt.Errorf("dependency installation on %s: %w", node.Name, err)
		}
	}
	return nil
}
