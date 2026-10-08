package controllerclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(code int) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}
}
func TestFailoverDoesNotReplayUnknownWritesOrTrustRedirects(t *testing.T) {
	for _, tc := range []struct {
		name         string
		method, path string
		first        int
		attempts     int
	}{{"unknown fleet write", "POST", "/v1/realm/fleets", 0, 1}, {"idempotent pulse", "POST", "/v1/realm/pulse", 0, 2}, {"leader rejection", "POST", "/v1/realm/fleets", 503, 2}, {"application 503", "POST", "/v1/realm/fleets", 504, 1}, {"redirect", "GET", "/v1/realm/state", 307, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					if tc.first == 0 {
						return nil, errors.New("connection lost after write")
					}
					resp := response(tc.first)
					if tc.first == 503 {
						resp.Header.Set("X-Titanus-Rejected", "true")
						resp.Header.Set("X-Titanus-Leader", "https://untrusted.invalid")
					}
					if tc.first == 307 {
						resp.Header.Set("Location", "https://untrusted.invalid")
					}
					return resp, nil
				}
				if r.URL.Host != "two:9443" {
					t.Fatal("untrusted host selected")
				}
				return response(200), nil
			})
			tr, e := New(base, "https://one:9443,https://two:9443")
			if e != nil {
				t.Fatal(e)
			}
			req, e := http.NewRequest(tc.method, "https://one:9443"+tc.path, strings.NewReader("payload"))
			if e != nil {
				t.Fatal(e)
			}
			resp, _ := tr.RoundTrip(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if calls != tc.attempts {
				t.Fatalf("attempts=%d, want %d", calls, tc.attempts)
			}
		})
	}
}
func TestRejectUnconfiguredOriginsAndBadConfiguration(t *testing.T) {
	base := roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("should not send request"); return nil, nil })
	for _, value := range []string{"http://one:9443", "https://user:pass@one:9443", "https://one:9443/path", ""} {
		if _, e := New(base, value); e == nil {
			t.Fatal("bad endpoint accepted", value)
		}
	}
	tr, e := New(base, "https://one:9443")
	if e != nil {
		t.Fatal(e)
	}
	req, _ := http.NewRequest("GET", "https://other:9443/v1/realm/state", nil)
	if _, e := tr.RoundTrip(req); e == nil {
		t.Fatal("unconfigured origin accepted")
	}
}

func TestRenewalDiscoveryAndAmbiguousSigningAreSeparate(t *testing.T) {
	var posts int
	tr, err := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == "GET" {
			if r.URL.Host == "one:9443" {
				return nil, errors.New("dead controller")
			}
			return response(200), nil
		}
		posts++
		if r.URL.Host != "two:9443" {
			t.Fatal("did not use discovered live controller")
		}
		return response(503), nil // Unmarked application error must not replay.
	}), "https://one:9443,https://two:9443")
	if err != nil {
		t.Fatal(err)
	}
	if err = tr.Discover(context.Background(), "https://one:9443"); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", "https://one:9443/v1/identity/renew", strings.NewReader("CSR"))
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if posts != 1 {
		t.Fatal("ambiguous signing was replayed")
	}
}
