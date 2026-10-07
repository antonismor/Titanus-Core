package deploy

import (
	"strings"
	"testing"

	"github.com/antonismor/Titanus-Core/internal/model"
)

func TestDaemonEnvironmentPublishesControllerEndpoint(t *testing.T) {
	plan := model.RealmPlan{RealmName: "LAB", ControlVIP: "10.0.0.100"}
	node := model.NodeSpec{Name: "gateway-1", ManagementIP: "10.0.0.20"}
	primary := model.NodeSpec{Name: "control-1", ManagementIP: "10.0.0.10"}

	env := daemonEnvironment(plan, node, primary, RealmDeployOptions{ClusterPort: 9443}, false, true)
	for _, wanted := range []string{
		"TITANUS_NODE_ID=gateway-1",
		"TITANUS_GATEWAY_MODE=true",
		"TITANUS_CONTROLLER_ENDPOINT=https://10.0.0.100:9443",
	} {
		if !strings.Contains(env, wanted) {
			t.Fatalf("daemon environment missing %q:\n%s", wanted, env)
		}
	}
}

func TestAgentEnvironmentUsesDedicatedFabricAddress(t *testing.T) {
	plan := model.RealmPlan{RealmName: "LAB", ControlVIP: "10.0.0.100"}
	node := model.NodeSpec{
		Name: "worker-1", ManagementIP: "10.0.0.21", FabricIP: "192.0.2.21",
		Capabilities: []model.Capability{model.CapabilityExecution},
	}
	primary := model.NodeSpec{Name: "control-1", ManagementIP: "10.0.0.10"}

	env := agentEnvironment(plan, node, primary, RealmDeployOptions{ClusterPort: 9443})
	for _, wanted := range []string{
		"TITANUS_NODE_FABRIC_ADDRESS=192.0.2.21",
		"TITANUS_CONTROLLER=https://10.0.0.100:9443",
	} {
		if !strings.Contains(env, wanted) {
			t.Fatalf("agent environment missing %q:\n%s", wanted, env)
		}
	}
	if !strings.Contains(realmAgentService, "--fabric-address=${TITANUS_NODE_FABRIC_ADDRESS}") {
		t.Fatalf("embedded agent service does not pass dedicated Fabric address")
	}
}
