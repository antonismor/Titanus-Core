package unitruntime

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeValidationAndThresholds(t *testing.T) {
	for _, url := range []string{"http://127.0.0.1:8080/ready", "tcp://127.0.0.1:8080"} {
		if _, err := ParseProbe(url); err != nil {
			t.Fatal(err)
		}
	}
	for _, url := range []string{"http://localhost:8080/", "http://10.0.0.1:80/", "file:///tmp/probe", "http://127.0.0.1/", "http://user@127.0.0.1:80/"} {
		if _, err := ParseProbe(url); err == nil {
			t.Fatalf("unsafe probe URL accepted: %s", url)
		}
	}
	result := advanceProbe(ProbeResult{}, fmt.Errorf("not ready"), time.Now())
	if result.Failures != 1 || result.Successes != 0 {
		t.Fatal(result)
	}
	result = advanceProbe(result, nil, time.Now())
	if result.Successes != 1 || result.Failures != 0 || result.LastError != "" {
		t.Fatal("success did not reset failure streak")
	}
}

func TestRestartBackoffLimitAndOperatorStop(t *testing.T) {
	code := 7
	now := time.Now()
	spec := Spec{Health: Health{Restart: "on-failure", InitialBackoffSeconds: 2, MaxBackoffSeconds: 5, MaxRestarts: 3}}
	state := State{Status: StatusFailed, DesiredRunning: true, ExitCode: &code}
	if restartDue(spec, &state, now) || !state.NextStartAt.Equal(now.Add(2*time.Second)) {
		t.Fatal("missing initial delay")
	}
	if restartDue(spec, &state, now.Add(time.Second)) || !restartDue(spec, &state, now.Add(2*time.Second)) {
		t.Fatal("backoff not enforced")
	}
	state.RestartCount = 2
	state.NextStartAt = time.Time{}
	if restartDue(spec, &state, now) || !state.NextStartAt.Equal(now.Add(5*time.Second)) {
		t.Fatal("exponential cap incorrect")
	}
	state.DesiredRunning = false
	if restartDue(spec, &state, now.Add(time.Minute)) {
		t.Fatal("operator stop was restarted")
	}
	state.DesiredRunning = true
	state.RestartCount = 3
	if restartDue(spec, &state, now.Add(time.Minute)) || state.LastError != "restart limit reached" {
		t.Fatal("restart limit not enforced")
	}
	zero := 0
	state = State{Status: StatusStopped, DesiredRunning: true, ExitCode: &zero}
	if restartDue(spec, &state, now) || !state.NextStartAt.IsZero() {
		t.Fatal("on-failure restarted clean exit")
	}
	state.HealthFailed = true
	if restartDue(spec, &state, now) || state.NextStartAt.IsZero() {
		t.Fatal("liveness failure lost after clean termination")
	}
}

func TestProbeWorkerHTTPAndTCP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(503)
		} else {
			w.WriteHeader(200)
		}
	}))
	defer server.Close()
	address := server.Listener.Addr().(*net.TCPAddr)
	for _, tc := range []struct {
		protocol, path string
		pass           bool
	}{{"http", "/ready", true}, {"http", "/bad", false}, {"tcp", "", true}} {
		encoded, _ := json.Marshal(probeRequest{Probe: Probe{Protocol: tc.protocol, Path: tc.path, Port: address.Port, TimeoutSeconds: 1}})
		if err := ProbeWorker(string(encoded)); (err == nil) != tc.pass {
			t.Fatalf("%s %s: %v", tc.protocol, tc.path, err)
		}
	}

}

func TestHealthNormalizationDoesNotMutateCaller(t *testing.T) {
	probe := &Probe{Protocol: "http", Port: 80}
	health := Health{Readiness: probe}
	health.Normalize("never")
	if probe.TimeoutSeconds != 0 || probe.Path != "" {
		t.Fatal("caller probe mutated")
	}
}
