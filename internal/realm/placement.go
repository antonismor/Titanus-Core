package realm

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/antonismor/Titanus-Core/internal/model"
)

type NodeScore struct {
	NodeID string  `json:"node_id"`
	Score  float64 `json:"score"`
	Reason string  `json:"reason"`
}

type PlacementEngine struct{}

func NewPlacementEngine() *PlacementEngine { return &PlacementEngine{} }

func (p *PlacementEngine) Rank(state State, fleet Fleet, existing []Assignment) []NodeScore {
	countsByNode := map[string]int{}
	spreadCounts := map[string]int{}
	for _, assignment := range existing {
		if assignment.Fleet != fleet.Name {
			continue
		}
		countsByNode[assignment.NodeID]++
		if fleet.SpreadLabel != "" {
			if node, ok := state.Nodes[assignment.NodeID]; ok {
				spreadCounts[node.Labels[fleet.SpreadLabel]]++
			}
		}
	}

	scores := make([]NodeScore, 0)
	for _, node := range state.Nodes {
		if !eligible(node, fleet) || !catalogPlacement(state, node, fleet) {
			continue
		}
		if len(fleet.Template.Ports) > 0 && countsByNode[node.ID] > 0 {
			continue
		}
		freeMemory := node.Resources.MemoryBytes - node.Resources.MemoryUsedBytes
		if fleet.Template.MemoryBytes > 0 && freeMemory < fleet.Template.MemoryBytes {
			continue
		}

		cpuFreeRatio := ratio(
			node.Resources.CPUMilliCapacity-node.Resources.CPUMilliUsed,
			node.Resources.CPUMilliCapacity,
		)
		memFreeRatio := ratio(freeMemory, node.Resources.MemoryBytes)
		reliability := float64(node.Successes+1) / float64(node.Successes+node.Failures+1)

		score := cpuFreeRatio*35 + memFreeRatio*35 + reliability*15
		score -= float64(countsByNode[node.ID]) * 20

		spread := ""
		if fleet.SpreadLabel != "" {
			spread = node.Labels[fleet.SpreadLabel]
			score -= float64(spreadCounts[spread]) * 25
		}
		if node.Resources.GPUCount > 0 {
			score += 2
		}

		reason := fmt.Sprintf("cpu=%.2f mem=%.2f reliability=%.2f units=%d spread=%s",
			cpuFreeRatio, memFreeRatio, reliability, countsByNode[node.ID], spread)
		scores = append(scores, NodeScore{NodeID: node.ID, Score: score, Reason: reason})
	}
	sort.Slice(scores, func(i, j int) bool {
		if math.Abs(scores[i].Score-scores[j].Score) < 0.00001 {
			return scores[i].NodeID < scores[j].NodeID
		}
		return scores[i].Score > scores[j].Score
	})
	return scores
}

func (p *PlacementEngine) Plan(state State, fleet Fleet) ([]Assignment, error) {
	now := time.Now().UTC()
	assignments := make([]Assignment, 0, fleet.Instances)
	working := cloneState(state)

	for i := 0; i < fleet.Instances; i++ {
		ranked := p.Rank(working, fleet, assignments)
		if len(ranked) == 0 {
			return nil, fmt.Errorf("no eligible Node remains for Fleet %s instance %d", fleet.Name, i+1)
		}
		nodeID := ranked[0].NodeID
		id := fmt.Sprintf("%s-%03d-g%d", fleet.Name, i+1, fleet.Generation)
		assignment := Assignment{
			ID: id, Fleet: fleet.Name, NodeID: nodeID,
			State: AssignmentPlanned, Generation: fleet.Generation,
			CreatedAt: now, UpdatedAt: now,
		}
		assignments = append(assignments, assignment)

		node := working.Nodes[nodeID]
		node.Resources.MemoryUsedBytes += fleet.Template.MemoryBytes
		node.Resources.CPUMilliUsed += int64(fleet.Template.CPUPercent * 10)
		node.Resources.UnitCount++
		working.Nodes[nodeID] = node
	}
	return assignments, nil
}

func (p *PlacementEngine) Reconcile(state State, fleet Fleet) ([]Assignment, error) {
	now := time.Now().UTC()
	existing := map[string]Assignment{}
	for _, assignment := range state.Assignments {
		if assignment.Fleet == fleet.Name && assignment.Generation == fleet.Generation {
			existing[assignment.ID] = assignment
		}
	}

	result := make([]Assignment, 0, fleet.Instances)
	working := cloneState(state)
	for slot := 1; slot <= fleet.Instances; slot++ {
		id := fmt.Sprintf("%s-%03d-g%d", fleet.Name, slot, fleet.Generation)
		if current, ok := existing[id]; ok {
			if node, exists := working.Nodes[current.NodeID]; exists && eligible(node, fleet) && catalogPlacement(state, node, fleet) {
				result = append(result, current)
				continue
			}
		}

		ranked := p.Rank(working, fleet, result)
		if len(ranked) == 0 {
			return nil, fmt.Errorf("no eligible Node remains for Fleet %s slot %d", fleet.Name, slot)
		}
		nodeID := ranked[0].NodeID
		startAfter := time.Time{}
		if previous, ok := existing[id]; ok && !previous.LeaseExpiresAt.IsZero() {
			startAfter = previous.LeaseExpiresAt.Add(2 * time.Second)
		}
		assignment := Assignment{
			ID: id, Fleet: fleet.Name, NodeID: nodeID,
			State: AssignmentPlanned, Generation: fleet.Generation,
			CreatedAt: now, UpdatedAt: now, StartAfter: startAfter,
		}
		result = append(result, assignment)
		node := working.Nodes[nodeID]
		node.Resources.MemoryUsedBytes += fleet.Template.MemoryBytes
		node.Resources.CPUMilliUsed += int64(fleet.Template.CPUPercent * 10)
		node.Resources.UnitCount++
		working.Nodes[nodeID] = node
	}
	return result, nil
}

func eligible(node Node, fleet Fleet) bool {
	if node.State != NodeReady || node.StorageQuarantined {
		return false
	}
	if !hasCapability(node.Capabilities, model.CapabilityExecution) {
		return false
	}
	for key, value := range fleet.RequiredLabels {
		if node.Labels[key] != value {
			return false
		}
	}
	return true
}

func hasCapability(caps []model.Capability, wanted model.Capability) bool {
	for _, cap := range caps {
		if cap == wanted {
			return true
		}
	}
	return false
}

func ratio(free, total int64) float64 {
	if total <= 0 {
		return 0
	}
	value := float64(free) / float64(total)
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func catalogPlacement(state State, node Node, fleet Fleet) bool {
	for _, mount := range fleet.Template.Mounts {
		if c, ok := state.Disks[mount.Disk]; ok && c.LocalNode != "" && c.LocalNode != node.ID {
			return false
		}
	}
	return true
}
