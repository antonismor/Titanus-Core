package observe

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBoundedJournalAndConcurrentRotation(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if e := Append(dir, "log", []byte(strings.Repeat("x", 60)+"\n"), 256, false); e != nil {
					t.Error(e)
				}
			}
		}()
	}
	wg.Wait()
	for _, name := range []string{"log", "log.1"} {
		info, e := os.Stat(filepath.Join(dir, name))
		if e != nil || info.Size() > 256 {
			t.Fatal("unbounded log", name, e)
		}
	}
	if e := Append(dir, "log", make([]byte, 257), 256, false); e == nil {
		t.Fatal("oversized record accepted")
	}
}
func TestJournalFailsClosedOnSymlinkAndNonRegular(t *testing.T) {
	for _, name := range []string{"log", "log.lock", "log.1"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			outside := filepath.Join(t.TempDir(), "outside")
			os.WriteFile(outside, []byte("unchanged"), 0600)
			os.Symlink(outside, filepath.Join(dir, name))
			if name == "log.1" {
				os.WriteFile(filepath.Join(dir, "log"), []byte("full"), 0600)
			}
			if e := Append(dir, "log", []byte("new"), 4, true); e == nil {
				t.Fatal("symlink log accepted")
			}
			data, _ := os.ReadFile(outside)
			if string(data) != "unchanged" {
				t.Fatal("followed host symlink")
			}
		})
	}
	dir := t.TempDir()
	os.Mkdir(filepath.Join(dir, "log"), 0700)
	if e := Append(dir, "log", nil, 1024, false); e == nil {
		t.Fatal("directory log accepted")
	}
}
func TestEventsRoundTripAndFiniteMetricLabels(t *testing.T) {
	r, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 300; i++ {
		if e = r.Record(Event{Kind: "api.result", TargetHash: TargetHash("/v1/node/leases/object"), Status: 403}); e != nil {
			t.Fatal(e)
		}
	}
	events, e := r.Tail()
	if e != nil || len(events) != 256 {
		t.Fatal("bounded event tail", len(events), e)
	}
	for i := 0; i < 100; i++ {
		op := Operation("/v1/node/units/attacker-" + strings.Repeat("secret", i))
		r.Request("SECRET_METHOD", op, 200, time.Millisecond)
	}
	metrics := r.Metrics()
	if strings.Contains(metrics, "secret") || strings.Contains(metrics, "SECRET_METHOD") || strings.Count(metrics, "titanus_http_requests_total{") != 1 {
		t.Fatal("unbounded/raw metric labels", metrics)
	}
	data, _ := json.Marshal(events)
	if bytes.Contains(data, []byte("/v1/node/leases")) {
		t.Fatal("raw target leaked")
	}
}

func TestLegacyOversizeLogIsBoundedAtRotation(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "unit.log"), []byte("old-history-most-recent-tail"), 0600)
	if err := Append(dir, "unit.log", []byte("new"), 4, false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "unit.log.1"))
	if err != nil || string(data) != "tail" {
		t.Fatal("legacy tail lost or unbounded", string(data), err)
	}
}

func TestOrchestrationOperationsHaveFiniteSecretFreeLabels(t *testing.T) {
	for path, want := range map[string]string{"/v1/realm/tasks/private-name/cancel": "realm/tasks/cancel", "/v1/realm/autoscalers/private-name": "realm/autoscalers", "/v1/realm/secrets/private-name": "realm/secrets", "/v1/node/units/private-name/usage": "node/units/usage"} {
		if got := Operation(path); got != want {
			t.Fatalf("%s => %s", path, got)
		}
	}
}
