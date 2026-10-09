package localclient

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestM6OrchestrationReachesTransport(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"received":true}`))
	}))
	defer server.Close()
	c := &Client{http: server.Client(), base: server.URL}
	for _, path := range []string{"/v1/realm/task-schedules", "/v1/realm/task-schedules/example/pause", "/v1/realm/secret-rotation", "/v1/realm/observations", "/v1/realm/alerts"} {
		if _, e := c.Orchestration(http.MethodGet, path, nil); e != nil {
			t.Fatal(path, e)
		}
	}
	before := requests
	for _, path := range []string{"/v1/realm/task-schedules-other", "/v1/realm/secret-rotation/extra", "/v1/node/secret-keyring", "/v1/realm/state"} {
		if _, e := c.Orchestration(http.MethodGet, path, nil); e == nil {
			t.Fatal("unexpected endpoint admitted", path)
		}
	}
	if requests != before || before != 5 {
		t.Fatal("endpoint admission/transport mismatch")
	}
}
