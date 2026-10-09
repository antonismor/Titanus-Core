package observe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCentralRedactionRestartDedupeAndMissingCoverage(t *testing.T) {
	root := t.TempDir()
	r, e := New(root)
	if e != nil {
		t.Fatal(e)
	}
	c, e := NewCentral(root)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	o := NodeObservation{NodeID: "n1", Realm: "LAB", Time: now, Events: []Event{{Time: now, Kind: "SECRET_KIND", Actor: "SECRET_ACTOR", Object: "SECRET_OBJECT", State: "SECRET_STATE", Operation: "SECRET_OPERATION", Request: "SECRET_REQUEST", Status: 200}}}
	if e = c.Save([]string{"n1", "missing"}, map[string]NodeObservation{"n1": o}, now); e != nil {
		t.Fatal(e)
	}
	events, e := c.Tail()
	if e != nil || len(events) != 1 {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(events)
	if strings.Contains(string(raw), "SECRET_") {
		t.Fatal("central event leaked untrusted field", string(raw))
	}
	status, e := c.Status()
	if e != nil || status.Collected != 1 || status.Expected != 2 || len(status.Alerts) != 1 {
		t.Fatal("missing host coverage hidden", e)
	}
	reopened, e := NewCentral(root)
	if e != nil {
		t.Fatal(e)
	}
	if e = reopened.Save([]string{"n1"}, map[string]NodeObservation{"n1": o}, now); e != nil {
		t.Fatal(e)
	}
	events, e = reopened.Tail()
	if e != nil || len(events) != 1 {
		t.Fatal("restart forgot persisted collection cursor", e)
	}
	// Bound both physical journal segments under sustained collection.
	for i := 0; i < 4000; i++ {
		o.Events[0].Time = now.Add(time.Duration(i) * time.Nanosecond)
		if e = c.Save([]string{"n1"}, map[string]NodeObservation{"n1": o}, now); e != nil {
			t.Fatal(e)
		}
	}
	for _, name := range []string{"cluster.ndjson", "cluster.ndjson.1"} {
		st, e := os.Stat(filepath.Join(r.Dir, name))
		if e == nil && st.Size() > JournalLimit {
			t.Fatal("unbounded central segment")
		}
	}
}
