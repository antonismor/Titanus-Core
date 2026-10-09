package observe

import (
	"encoding/json"
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/durable"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type NodeObservation struct {
	NodeID        string    `json:"node_id"`
	Realm         string    `json:"realm"`
	Time          time.Time `json:"time"`
	Events        []Event   `json:"events"`
	Incomplete    bool      `json:"incomplete"`
	LogFailures   int       `json:"log_failures"`
	AuditFailures uint64    `json:"audit_failures"`
}
type CentralEvent struct {
	NodeHash    string    `json:"node_hash"`
	Time        time.Time `json:"time"`
	Kind        string    `json:"kind"`
	ActorHash   string    `json:"actor_hash,omitempty"`
	ObjectHash  string    `json:"object_hash,omitempty"`
	RequestHash string    `json:"request_hash,omitempty"`
	Status      int       `json:"status,omitempty"`
	Ready       bool      `json:"ready"`
	Live        bool      `json:"live"`
	Restarts    int       `json:"restarts"`
}
type Alert struct {
	NodeHash string `json:"node_hash,omitempty"`
	Code     string `json:"code"`
}
type CollectionStatus struct {
	Time      time.Time         `json:"time"`
	Expected  int               `json:"expected"`
	Collected int               `json:"collected"`
	Alerts    []Alert           `json:"alerts"`
	Cursors   map[string]string `json:"cursors"`
}
type Central struct {
	Dir string
	mu  sync.Mutex
}

func NewCentral(root string) (*Central, error) {
	dir := filepath.Join(root, "observability")
	st, e := os.Lstat(dir)
	if e != nil || !st.IsDir() || st.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("private existing observation directory required")
	}
	return &Central{Dir: dir}, nil
}
func (c *Central) Status() (CollectionStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}
func (c *Central) statusLocked() (CollectionStatus, error) {
	s := CollectionStatus{Alerts: []Alert{}, Cursors: map[string]string{}}
	f, e := openRegular(filepath.Join(c.Dir, "collection.json"), 0, 0)
	if os.IsNotExist(e) {
		return s, nil
	}
	if e != nil {
		return s, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || st.Size() > 32768 {
		return s, fmt.Errorf("invalid collection status")
	}
	if e = json.NewDecoder(f).Decode(&s); e != nil {
		return s, e
	}
	if len(s.Cursors) > 64 || len(s.Alerts) > 256 {
		return s, fmt.Errorf("collection bounds exceeded")
	}
	return s, nil
}
func centralize(node string, e Event) CentralEvent {
	kind := "other"
	switch e.Kind {
	case "daemon.open", "api.intent", "api.result", "api.aborted", "unit.state":
		kind = e.Kind
	}
	status := e.Status
	if status < 100 || status > 599 {
		status = 0
	}
	r := e.Restarts
	if r < 0 || r > 1000000 {
		r = 0
	}
	out := CentralEvent{NodeHash: TargetHash(node), Time: e.Time, Kind: kind, Status: status, Ready: e.Ready, Live: e.Live, Restarts: r}
	if e.Actor != "" {
		out.ActorHash = TargetHash(e.Actor)
	}
	if e.Object != "" {
		out.ObjectHash = TargetHash(e.Object)
	}
	if e.Request != "" {
		out.RequestHash = TargetHash(e.Request)
	}
	return out
}

// Save collects only fixed structured fields. Commands, paths, environments,
// arbitrary states/messages and raw workload/daemon output are never accepted.
func (c *Central) Save(expected []string, observations map[string]NodeObservation, now time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	old, e := c.statusLocked()
	if e != nil {
		return e
	}
	s := CollectionStatus{Time: now, Expected: len(expected), Alerts: []Alert{}, Cursors: map[string]string{}}
	sort.Strings(expected)
	if len(expected) > 64 {
		s.Alerts = append(s.Alerts, Alert{Code: "collection.capacity"})
		expected = expected[:64]
	}
	for _, id := range expected {
		hash := TargetHash(id)
		s.Cursors[hash] = old.Cursors[hash]
		o, ok := observations[id]
		if !ok || o.NodeID != id || o.Time.After(now.Add(time.Second)) || now.Sub(o.Time) > 20*time.Second || len(o.Events) > 256 {
			s.Alerts = append(s.Alerts, Alert{hash, "node.unreachable_or_stale"})
			continue
		}
		s.Collected++
		if o.Incomplete {
			s.Alerts = append(s.Alerts, Alert{hash, "diagnostics.incomplete"})
		}
		if o.LogFailures > 0 {
			s.Alerts = append(s.Alerts, Alert{hash, "log.degraded"})
		}
		if o.AuditFailures > 0 {
			s.Alerts = append(s.Alerts, Alert{hash, "audit.degraded"})
		}
		cursor := old.Cursors[hash]
		start := 0
		found := cursor == ""
		encoded := [][]byte{}
		digests := []string{}
		for i, event := range o.Events {
			raw, e := json.Marshal(centralize(id, event))
			if e != nil {
				return e
			}
			encoded = append(encoded, raw)
			digest := TargetHash(string(raw))
			digests = append(digests, digest)
			if digest == cursor {
				start = i + 1
				found = true
			}
		}
		if !found {
			s.Alerts = append(s.Alerts, Alert{hash, "events.retention_gap"})
		}
		for _, raw := range encoded[start:] {
			if e := Append(c.Dir, "cluster.ndjson", append(raw, '\n'), JournalLimit, true); e != nil {
				return e
			}
		}
		if len(digests) > 0 {
			s.Cursors[hash] = digests[len(digests)-1]
		} else {
			s.Cursors[hash] = cursor
		}
	}
	return durable.WriteJSON(filepath.Join(c.Dir, "collection.json"), s, 0600)
}
func (c *Central) Tail() ([]CentralEvent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	items := []CentralEvent{}
	for _, name := range []string{"cluster.ndjson.1", "cluster.ndjson"} {
		f, e := openRegular(filepath.Join(c.Dir, name), 0, 0)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return nil, e
		}
		st, e := f.Stat()
		if e != nil || st.Size() > JournalLimit {
			f.Close()
			return nil, fmt.Errorf("unbounded central journal")
		}
		d := json.NewDecoder(f)
		for {
			var event CentralEvent
			e = d.Decode(&event)
			if e == io.EOF {
				break
			}
			if e != nil {
				f.Close()
				return nil, e
			}
			items = append(items, event)
			if len(items) > 256 {
				items = items[1:]
			}
		}
		f.Close()
	}
	return items, nil
}
