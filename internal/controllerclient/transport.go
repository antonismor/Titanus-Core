// Package controllerclient discovers available leaders among explicitly trusted
// API origins. It never forwards credentials to a server-supplied redirect URL.
package controllerclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Transport struct {
	Base      http.RoundTripper
	origins   []*url.URL
	mu        sync.Mutex
	preferred int
}

// Discover probes a read before a new mutation; it never replays that mutation
// after an ambiguous transport error. Only configured origins can be selected.
func (t *Transport) Discover(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/identity/crl", nil)
	if err != nil {
		return err
	}
	resp, err := t.RoundTrip(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("controller discovery: %s", resp.Status)
	}
	return nil
}

func New(base http.RoundTripper, endpoints string) (*Transport, error) {
	if transport, ok := base.(*http.Transport); ok {
		transport = transport.Clone()
		transport.DialContext = (&net.Dialer{Timeout: time.Second}).DialContext
		transport.TLSHandshakeTimeout = 2 * time.Second
		transport.ResponseHeaderTimeout = 3 * time.Second
		base = transport
	}
	t := &Transport{Base: base}
	seen := map[string]bool{}
	for _, value := range strings.Split(endpoints, ",") {
		u, err := url.Parse(strings.TrimSpace(value))
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("controller endpoint requires HTTPS origin")
		}
		origin := u.Scheme + "://" + u.Host
		if seen[origin] {
			continue
		}
		seen[origin] = true
		t.origins = append(t.origins, u)
	}
	if len(t.origins) == 0 || len(t.origins) > 5 {
		return nil, fmt.Errorf("1 to 5 controller endpoints required")
	}
	return t, nil
}
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	path := req.URL.Path
	if !strings.HasPrefix(path, "/v1/realm/") && !strings.HasPrefix(path, "/v1/identity/") {
		return t.Base.RoundTrip(req)
	}
	if req.Body != nil {
		defer req.Body.Close()
	}
	// Do not rewrite unrelated outbound requests or permit response-based SSRF.
	trusted := false
	for _, u := range t.origins {
		if req.URL.Scheme == u.Scheme && req.URL.Host == u.Host {
			trusted = true
		}
	}
	if !trusted {
		return nil, fmt.Errorf("request origin is outside configured controller set")
	}
	t.mu.Lock()
	start := t.preferred
	t.mu.Unlock()
	safe := req.Method == http.MethodGet || req.Method == http.MethodHead || path == "/v1/realm/pulse" || path == "/v1/realm/nodes"
	var last error
	for i := 0; i < len(t.origins); i++ {
		origin := t.origins[(start+i)%len(t.origins)]
		copy := req.Clone(req.Context())
		target := *req.URL
		target.Scheme = origin.Scheme
		target.Host = origin.Host
		copy.URL = &target
		copy.Host = ""
		if req.Body != nil && req.Body != http.NoBody {
			if req.GetBody == nil {
				return nil, fmt.Errorf("controller retry requires replayable body")
			}
			body, e := req.GetBody()
			if e != nil {
				return nil, e
			}
			copy.Body = body
		}
		resp, err := t.Base.RoundTrip(copy)
		if err != nil {
			last = err
			if safe {
				continue
			}
			return nil, fmt.Errorf("controller write outcome unknown: %w", err)
		}
		rejected := resp.StatusCode == http.StatusServiceUnavailable && resp.Header.Get("X-Titanus-Rejected") == "true"
		if rejected {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			last = errors.New("controller rejected operation before execution")
			continue
		}
		if resp.StatusCode/100 == 2 {
			t.mu.Lock()
			t.preferred = (start + i) % len(t.origins)
			t.mu.Unlock()
		}
		return resp, nil
	}
	return nil, fmt.Errorf("no available Realm controller: %w", last)
}
