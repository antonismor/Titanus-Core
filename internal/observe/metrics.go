package observe

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type RequestKey struct {
	Method, Operation string
	Status            int
}
type RequestMetric struct {
	Count   uint64
	Seconds float64
}

// Operations come from registered API route categories; attacker-controlled
// names, query parameters and methods never become metric labels.
func Operation(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[0] != "v1" {
		return "unknown"
	}
	if len(parts) == 2 {
		switch parts[1] {
		case "health", "version", "metrics", "diagnostics", "events":
			return parts[1]
		}
	}
	if len(parts) < 3 {
		return "unknown"
	}
	allowed := map[string]bool{"realm/task-schedules": true, "realm/secret-rotation": true, "realm/observations": true, "realm/alerts": true, "node/observation": true, "node/secret-keyring": true, "realm/schema": true, "compatibility": true, "realm/tasks": true, "realm/autoscalers": true, "realm/secrets": true, "realm/state": true, "realm/nodes": true, "realm/pulse": true, "realm/fleets": true, "realm/routes": true, "realm/policies": true, "realm/consensus": true, "identity/crl": true, "identity/renew": true, "node/units": true, "node/disks": true, "node/sources": true, "node/leases": true}
	op := parts[1] + "/" + parts[2]
	if !allowed[op] {
		return "unknown"
	}
	if len(parts) == 5 {
		switch parts[4] {
		case "start", "stop", "scale", "rollback", "snapshot", "snapshots", "restore", "owner", "detach", "fence", "logs", "usage", "cancel":
			op += "/" + parts[4]
		}
	}
	return op
}

func (r *Recorder) Request(method, operation string, status int, duration time.Duration) {
	method = Method(method)
	if status < 100 || status > 599 {
		status = 500
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	k := RequestKey{method, operation, status}
	m := r.requests[k]
	m.Count++
	m.Seconds += duration.Seconds()
	r.requests[k] = m
}
func Method(method string) string {
	switch method {
	case "GET", "POST", "DELETE", "HEAD", "PUT", "PATCH", "OPTIONS":
	default:
		return "other"
	}
	return method
}

func (r *Recorder) Metrics() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	b.WriteString("# TYPE titanus_http_requests_total counter\n# TYPE titanus_http_request_duration_seconds_sum counter\n# TYPE titanus_observation_failures_total counter\n")
	fmt.Fprintf(&b, "titanus_observation_failures_total %d\n", r.failures)
	keys := []RequestKey{}
	for k := range r.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
	for _, k := range keys {
		m := r.requests[k]
		labels := fmt.Sprintf("method=%q,operation=%q,status=%q", k.Method, k.Operation, fmt.Sprint(k.Status))
		fmt.Fprintf(&b, "titanus_http_requests_total{%s} %d\ntitanus_http_request_duration_seconds_sum{%s} %.9f\n", labels, m.Count, labels, m.Seconds)
	}
	return b.String()
}
