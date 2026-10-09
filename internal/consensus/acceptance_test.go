package consensus

import (
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/antonismor/Titanus-Core/internal/version"
)

// A bounded real-TLS/durable-Raft workload. This shares a host kernel and is
// intentionally separate from independent-VM/power/storage acceptance.
func TestAcceptanceSustainedQuorum(t *testing.T) {
	if os.Getenv("TITANUS_ACCEPTANCE_TEST") != "1" {
		t.Skip("opt-in measured acceptance workload")
	}
	seconds, e := strconv.Atoi(os.Getenv("TITANUS_ACCEPTANCE_SECONDS"))
	if e != nil || seconds < 15 || seconds > 120 {
		t.Fatal("duration must be 15..120 seconds")
	}
	c := newCluster(t)
	current := c.leader(-1)
	f := testFleet("acceptance")
	f.MinimumAvailable = 0
	if e = c.stores[current].PutFleet(f); e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	latencies := []float64{}
	acknowledged := 0
	lastInstances := 1
	revision := c.nodes[current].Snapshot().Revision
	var electionSeconds float64
	isolated := -1
	for time.Since(start) < time.Duration(seconds)*time.Second {
		if isolated < 0 && time.Since(start) >= time.Duration(seconds/3)*time.Second {
			isolated = current
			fault := time.Now()
			if e = c.nodes[isolated].transport.Close(); e != nil {
				t.Fatal(e)
			}
			current = c.leader(isolated)
			electionSeconds = time.Since(fault).Seconds()
			before := c.nodes[isolated].Snapshot().Revision
			if _, e = c.stores[isolated].ScaleFleet(f.Name, 99); e == nil {
				t.Fatal("isolated minority acknowledged a write")
			}
			if c.nodes[isolated].Snapshot().Revision != before {
				t.Fatal("minority candidate escaped into committed state")
			}
		}
		lastInstances = 1 + acknowledged%3
		begin := time.Now()
		if _, e = c.stores[current].ScaleFleet(f.Name, lastInstances); e != nil {
			t.Fatal("quorum workload failed", e)
		}
		latencies = append(latencies, time.Since(begin).Seconds())
		acknowledged++
		next := c.nodes[current].Snapshot().Revision
		if next <= revision {
			t.Fatal("acknowledged revision did not advance")
		}
		revision = next
		time.Sleep(100 * time.Millisecond) // Deliberate finite rate: <=10 writes/s.
	}
	if acknowledged < 30 || isolated < 0 {
		t.Fatal("insufficient sustained/fault evidence")
	}
	if e = c.nodes[isolated].Close(); e != nil {
		t.Fatal(e)
	}
	c.nodes[isolated] = nil
	c.open(isolated)
	c.converge(revision)
	for i := range c.nodes {
		if e = c.nodes[i].Close(); e != nil {
			t.Fatal(e)
		}
		c.nodes[i] = nil
	}
	restart := time.Now()
	for i := range c.nodes {
		c.open(i)
	}
	current = c.leader(-1)
	c.converge(revision)
	for _, n := range c.nodes {
		state := n.Snapshot()
		if state.Revision != revision || state.Fleets[f.Name].Instances != lastInstances {
			t.Fatal("acknowledged workload integrity lost after restart")
		}
	}
	sort.Float64s(latencies)
	evidence, _ := json.Marshal(map[string]any{
		"test": "sustained_quorum", "architecture": version.Info().Arch,
		"revision": os.Getenv("TITANUS_ACCEPTANCE_REVISION"),
		"topology": "3 TLS voters / 1 shared kernel", "duration_seconds": time.Since(start).Seconds(),
		"writes": acknowledged, "max_write_rate": 10, "committed_revision": revision,
		"write_p50_seconds": latencies[len(latencies)/2], "write_p95_seconds": latencies[(len(latencies)-1)*95/100],
		"write_max_seconds": latencies[len(latencies)-1], "election_seconds": electionSeconds,
		"restart_seconds": time.Since(restart).Seconds(), "minority_write_refused": true,
		"all_voter_integrity_verified": true, "independent_host_acceptance": false,
	})
	t.Log("TITANUS_ACCEPTANCE_EVIDENCE " + string(evidence))
}
