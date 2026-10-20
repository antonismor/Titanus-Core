package prerequisites

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/fencing"
	"github.com/antonismor/Titanus-Core/internal/model"
	"github.com/antonismor/Titanus-Core/internal/remote"
)

func TestPackagesFollowStorageAndFencingPlan(t *testing.T) {
	plan := model.RealmPlan{Ceph: model.CephSpec{Enabled: true, Provision: true, EnableCephFS: true}, Installation: &model.Installation{Profile: model.ProfileNativeHA, PowerFencing: map[string]fencing.Target{"wrk": {Provider: "ipmi"}}}}
	storage := strings.Join(Packages(plan, model.NodeSpec{Capabilities: []model.Capability{model.CapabilityStorage}}), " ")
	for _, p := range []string{"ceph-common", "ceph-mon", "ceph-mgr", "ceph-osd", "ceph-mds", "lvm2", "nftables"} {
		if !strings.Contains(storage, p) {
			t.Fatal(storage)
		}
	}
	if strings.Contains(storage, "radosgw") || strings.Contains(storage, "ipmitool") {
		t.Fatal(storage)
	}
	control := strings.Join(Packages(plan, model.NodeSpec{Capabilities: []model.Capability{model.CapabilityControl}}), " ")
	if !strings.Contains(control, "ipmitool") || strings.Contains(control, "ceph-osd") {
		t.Fatal(control)
	}
	plan.Ceph.Provision = false
	if strings.Contains(strings.Join(Packages(plan, model.NodeSpec{Capabilities: []model.Capability{model.CapabilityStorage}}), " "), "ceph-osd") {
		t.Fatal("existing Ceph should not install new daemons")
	}
}

func TestDependencyScriptInstallsOnlyMissingAndPersistsModulesIdempotently(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0700)
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("id", "echo 0\n")
	write("dpkg-query", `test -f "$FIXTURE/installed" || exit 1; printf 'install ok installed'`)
	write("apt-get", `echo "$*" >> "$FIXTURE/apt.log"; case "$*" in *install*) touch "$FIXTURE/installed";; esac`)
	write("modprobe", `echo "$*" >> "$FIXTURE/modules.log"`)
	moduleFile := filepath.Join(dir, "modules.conf")
	for i := 0; i < 2; i++ {
		cmd := exec.Command("sh", "-c", script([]string{"nftables", "kmod"}, []string{"overlay", "vxlan"}, moduleFile))
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FIXTURE="+dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	apt, _ := os.ReadFile(filepath.Join(dir, "apt.log"))
	modules, _ := os.ReadFile(moduleFile)
	if strings.Count(string(apt), "install -y") != 1 || !strings.Contains(string(apt), "--no-upgrade nftables kmod") {
		t.Fatalf("unexpected package operations %s", apt)
	}
	if string(modules) != "overlay\nvxlan\n" {
		t.Fatalf("duplicate module config %q", modules)
	}
}

type recordingRunner struct {
	calls       []string
	failGuard   bool
	failInstall bool
}

func (r *recordingRunner) Run(n model.NodeSpec, c string) (remote.Result, error) {
	r.calls = append(r.calls, n.Name+":"+c)
	if r.failGuard && strings.Contains(c, "for path in") || r.failInstall && strings.Contains(c, "TITANUS_DEPENDENCIES_OK") {
		return remote.Result{}, fmt.Errorf("injected failure")
	}
	return remote.Result{}, nil
}

func TestAutomaticDependencyRepairIsBoundedAndRejectsUnsafeFailures(t *testing.T) {
	for _, scenario := range []struct {
		name, failure               string
		enabled, persistent, module bool
		wantOK                      bool
		wantAttempts                int
	}{
		{"network-recovers", "Temporary failure resolving 'deb.example'", true, false, false, true, 3},
		{"lock-recovers", "Could not get lock /var/lib/dpkg/lock-frontend", true, false, false, true, 3},
		{"network-exhausted", "Unable to connect to deb.example", true, true, false, false, 2},
		{"repair-disabled", "Temporary failure resolving 'deb.example'", false, false, false, false, 1},
		{"signature-failure", "NO_PUBKEY; Temporary failure resolving 'deb.example'", true, false, false, false, 1},
		{"checksum-failure", "Hash Sum mismatch", true, false, false, false, 1},
		{"package-script-failure", "dpkg: error processing; Unable to connect", true, false, false, false, 1},
		{"unknown-failure", "Unrecognized failure", true, false, false, false, 1},
		{"module-index-recovers", "", true, false, true, true, 2},
		{"module-exhausted", "", true, true, true, false, 2},
		{"module-repair-disabled", "", false, false, true, false, 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write("id", "echo 0\n")
			write("sleep", "exit 0\n")
			write("dpkg-query", "exit 1\n")
			write("apt-get", `echo "$*" >> "$FIXTURE/apt.log"
if [ -n "$FAILURE" ] && { [ "$PERSISTENT" = true ] || [ ! -f "$FIXTURE/failed" ]; }; then
 touch "$FIXTURE/failed"; echo "$FAILURE" >&2; exit 100
fi
`)
			write("modprobe", `echo "$*" >> "$FIXTURE/module.log"; test "$PERSISTENT" != true && test -f "$FIXTURE/index-fixed"`)
			write("depmod", `touch "$FIXTURE/index-fixed"`)
			packages := []string{"nftables"}
			var modules []string
			if scenario.module {
				packages = nil
				modules = []string{"overlay"}
			}
			command := dependencyScript(packages, modules, filepath.Join(dir, "modules.conf"), scenario.enabled, false)
			cmd := exec.Command("sh", "-c", command)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "FIXTURE="+dir, "FAILURE="+scenario.failure, fmt.Sprintf("PERSISTENT=%t", scenario.persistent))
			out, err := cmd.CombinedOutput()
			if (err == nil) != scenario.wantOK {
				t.Fatalf("success=%v wanted=%v: %s", err == nil, scenario.wantOK, out)
			}
			log := "apt.log"
			if scenario.module {
				log = "module.log"
			}
			calls, _ := os.ReadFile(filepath.Join(dir, log))
			if strings.Count(string(calls), "\n") != scenario.wantAttempts {
				t.Fatalf("unexpected attempts %s; output=%s", calls, out)
			}
			if !scenario.enabled && strings.Contains(string(out), "TITANUS_REPAIR") {
				t.Fatal("repair ran while disabled", string(out))
			}
			if !scenario.wantOK && strings.Contains(string(out), "TITANUS_DEPENDENCIES_OK") {
				t.Fatal("failure reported success", string(out))
			}
		})
	}
}

func TestAllFreshGuardsPrecedeAnyDependencyMutation(t *testing.T) {
	plan := model.RealmPlan{Installation: &model.Installation{Operation: "install"}, Nodes: []model.NodeSpec{{Name: "first"}, {Name: "second"}}}
	r := &recordingRunner{failGuard: true}
	if err := EnsureNodes(r, plan); err == nil || len(r.calls) != 1 || strings.Contains(r.calls[0], "apt-get") {
		t.Fatal("guard failure changed nodes", r.calls)
	}
	r = &recordingRunner{}
	if err := EnsureNodes(r, plan); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 4 || !strings.Contains(r.calls[1], "for path in") || !strings.Contains(r.calls[2], "apt-get") {
		t.Fatal(r.calls)
	}
	r = &recordingRunner{failInstall: true}
	if err := EnsureNodes(r, plan); err == nil || !strings.Contains(err.Error(), "first") {
		t.Fatal("node missing from dependency failure", err)
	}
}
