package deploy

import (
	"strings"
	"testing"
)

// A missing /run/titanus-offline.lock must not prevent systemd from starting
// titanus-agent after a clean boot. Keep the writable sandbox restricted.
func TestRealmAgentServiceOfflineLockBootstrap(t *testing.T) {
	const pre = "ExecStartPre=+/bin/sh -c 'umask 077; : >>/run/titanus-offline.lock'\n"
	const guarded = "ReadWritePaths=/var/lib/titanus /var/log/titanus /etc/titanus/pki -/run/titanus-offline.lock\n"
	if !strings.Contains(realmAgentService, pre) {
		t.Fatal("agent unit must create the 0600 offline lock before namespace setup")
	}
	if !strings.Contains(realmAgentService, guarded) {
		t.Fatal("missing offline lock path must be optional at ExecStartPre")
	}
	if strings.Contains(realmAgentService, " /run\n") {
		t.Fatal("do not widen systemd write sandbox to all of /run")
	}
}
