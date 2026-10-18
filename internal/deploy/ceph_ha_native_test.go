package deploy

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/model"
	"os"
	"os/exec"
	"testing"
)

func TestNativeThreeMonitorHAPolicy(t *testing.T) {
	if os.Getenv("TITANUS_CEPH_HA_TEST") != "1" {
		t.Skip("isolated native three-monitor storage fixture")
	}
	p := model.RealmPlan{Installation: &model.Installation{Profile: model.ProfileNativeHA}, Ceph: model.CephSpec{Enabled: true, Replication: 3, EnableRBD: true}}
	p.Ceph.EnableCephFS = os.Getenv("TITANUS_CEPH_HA_CEPHFS") == "1"
	for i, id := range []string{"a", "b", "c"} {
		p.Nodes = append(p.Nodes, model.NodeSpec{Name: id, ManagementIP: fmt.Sprintf("127.0.0.%d", 51+i), Capabilities: []model.Capability{model.CapabilityStorage}})
	}
	data, e := exec.Command("bash", "-c", HACephProbeCommand()).Output()
	if e != nil {
		t.Fatal(e)
	}
	e = ValidateHACephReport(data, p)
	if os.Getenv("TITANUS_CEPH_HA_DEGRADED") == "1" {
		if e == nil {
			t.Fatal("degraded storage authorized HA activation")
		}
		return
	}
	if e != nil {
		t.Fatal(e, string(data))
	}
}
