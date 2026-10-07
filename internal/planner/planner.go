package planner

import (
	"fmt"
	"strings"

	"github.com/antonismor/Titanus-Core/internal/model"
)

type ActionType string

const (
	ActionValidate  ActionType = "VALIDATE"
	ActionBootstrap ActionType = "BOOTSTRAP"
	ActionFabric    ActionType = "FABRIC"
	ActionStorage   ActionType = "STORAGE"
	ActionControl   ActionType = "CONTROL"
	ActionVerify    ActionType = "VERIFY"
)

type Action struct {
	Order       int
	Type        ActionType
	Target      string
	Description string
	Destructive bool
}

func Build(plan model.RealmPlan) ([]Action, error) {
	if err := plan.Validate(); err != nil {
		return nil, err
	}

	actions := []Action{{
		Order: 1, Type: ActionValidate, Target: plan.RealmName,
		Description: "Validate Realm topology, addressing and capabilities",
	}}

	order := 2
	for _, node := range plan.Nodes {
		actions = append(actions, Action{
			Order: order, Type: ActionBootstrap, Target: node.Name,
			Description: fmt.Sprintf("Bootstrap Titanus directories and host prerequisites on %s (%s)", node.Name, node.ManagementIP),
		})
		order++
	}

	actions = append(actions, Action{
		Order: order, Type: ActionFabric, Target: plan.RealmName,
		Description: fmt.Sprintf("Prepare Titanus Fabric %s and service range %s", plan.FabricCIDR, plan.ServiceCIDR),
	})
	order++

	if plan.Ceph.Enabled {
		names := make([]string, 0)
		for _, n := range plan.StorageNodes() {
			names = append(names, n.Name)
		}
		actions = append(actions, Action{
			Order: order, Type: ActionStorage, Target: strings.Join(names, ","),
			Description: fmt.Sprintf(
				"Prepare Ceph integration: public=%s cluster=%s replication=%d RBD=%t CephFS=%t RGW=%t",
				plan.Ceph.PublicCIDR, plan.Ceph.ClusterCIDR, plan.Ceph.Replication,
				plan.Ceph.EnableRBD, plan.Ceph.EnableCephFS, plan.Ceph.EnableRGW,
			),
			Destructive: false,
		})
		order++
	}

	actions = append(actions, Action{
		Order: order, Type: ActionControl, Target: plan.RealmName,
		Description: fmt.Sprintf("Initialize Realm Core across %d CONTROL-capable node(s)", len(plan.ControlNodes())),
	})
	order++

	actions = append(actions, Action{
		Order: order, Type: ActionVerify, Target: plan.RealmName,
		Description: "Verify node Pulse, Realm state and storage/Fabric reachability",
	})
	return actions, nil
}
