package reconcile

import (
	"testing"

	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/security"
)

func TestFleetSecurityReachesUnit(t *testing.T) {
	fleet := realm.Fleet{Template: realm.UnitTemplate{Security: security.Policy{Capabilities: []string{"NET_BIND_SERVICE"}}}}
	spec := unitSpec(fleet, realm.Assignment{ID: "web-1"})
	spec.Normalize()
	if spec.Security.Seccomp != security.DefaultProfile || !*spec.Security.NoNewPrivileges || len(spec.Security.Capabilities) != 1 || spec.Security.Capabilities[0] != "CAP_NET_BIND_SERVICE" {
		t.Fatalf("lost Fleet security: %+v", spec.Security)
	}
}
