package localclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/controlapi"
	"github.com/antonismor/Titanus-Core/internal/realm"
)

type Client struct {
	http *http.Client
	base string
}

func New(socketPath string) *Client {
	if socketPath == "" {
		socketPath = "/run/titanus/titanus.sock"
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{
		http: &http.Client{Transport: transport, Timeout: 30 * time.Second},
		base: "http://titanus.local",
	}
}

func (c *Client) Observe(kind string) ([]byte, error) {
	if kind != "metrics" && kind != "diagnostics" && kind != "events" {
		return nil, fmt.Errorf("invalid observation endpoint")
	}
	resp, err := c.http.Get(c.base + "/v1/" + kind)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("diagnostics unavailable (HTTP %d): %s", resp.StatusCode, data)
	}
	return data, nil
}

func (c *Client) ListPolicies() ([]realm.NetworkPolicy, error) {
	var policies []realm.NetworkPolicy
	err := c.do(http.MethodGet, "/v1/realm/policies", nil, &policies)
	return policies, err
}

func (c *Client) CreatePolicy(policy realm.NetworkPolicy) (realm.NetworkPolicy, error) {
	var stored realm.NetworkPolicy
	err := c.do(http.MethodPost, "/v1/realm/policies", policy, &stored)
	return stored, err
}

func (c *Client) PolicyStatus(name string) (realm.NetworkPolicy, error) {
	var policy realm.NetworkPolicy
	err := c.do(http.MethodGet, "/v1/realm/policies/"+url.PathEscape(name), nil, &policy)
	return policy, err
}

func (c *Client) DeletePolicy(name string) error {
	return c.do(http.MethodDelete, "/v1/realm/policies/"+url.PathEscape(name), nil, nil)
}

func (c *Client) ListRoutes() ([]realm.Route, error) {
	var routes []realm.Route
	err := c.do(http.MethodGet, "/v1/realm/routes", nil, &routes)
	return routes, err
}

func (c *Client) CreateRoute(route realm.Route) (realm.Route, error) {
	var stored realm.Route
	err := c.do(http.MethodPost, "/v1/realm/routes", route, &stored)
	return stored, err
}

func (c *Client) RouteStatus(name string) (realm.Route, error) {
	var route realm.Route
	err := c.do(http.MethodGet, "/v1/realm/routes/"+url.PathEscape(name), nil, &route)
	return route, err
}

func (c *Client) DeleteRoute(name string) error {
	return c.do(http.MethodDelete, "/v1/realm/routes/"+url.PathEscape(name), nil, nil)
}

func (c *Client) ListFleets() ([]realm.Fleet, error) {
	var fleets []realm.Fleet
	err := c.do(http.MethodGet, "/v1/realm/fleets", nil, &fleets)
	return fleets, err
}

func (c *Client) CreateFleet(fleet realm.Fleet) (map[string]any, error) {
	var result map[string]any
	err := c.do(http.MethodPost, "/v1/realm/fleets", fleet, &result)
	return result, err
}

func (c *Client) FleetStatus(name string) (map[string]any, error) {
	var result map[string]any
	err := c.do(http.MethodGet, "/v1/realm/fleets/"+url.PathEscape(name), nil, &result)
	return result, err
}

func (c *Client) ScaleFleet(name string, instances int) (realm.Fleet, error) {
	var fleet realm.Fleet
	err := c.do(http.MethodPost, "/v1/realm/fleets/"+url.PathEscape(name)+"/scale",
		controlapi.ScaleFleetRequest{Instances: instances}, &fleet)
	return fleet, err
}

func (c *Client) RollbackFleet(name string, g uint64) (realm.Fleet, error) {
	var f realm.Fleet
	err := c.do(http.MethodPost, "/v1/realm/fleets/"+url.PathEscape(name)+"/rollback", map[string]uint64{"generation": g}, &f)
	return f, err
}

func (c *Client) DeleteFleet(name string) error {
	return c.do(http.MethodDelete, "/v1/realm/fleets/"+url.PathEscape(name), nil, nil)
}

func (c *Client) RealmState() (realm.State, error) {
	var state realm.State
	err := c.do(http.MethodGet, "/v1/realm/state", nil, &state)
	return state, err
}

func (c *Client) do(method, path string, requestBody, response any) error {
	var body io.Reader
	if requestBody != nil {
		data, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.base+path, body)
	if err != nil {
		return err
	}
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if method == http.MethodGet && strings.HasPrefix(path, "/v1/realm/") &&
			strings.Contains(string(data), "Realm quorum/leader unavailable") {
			if e := c.leaderRead(path, response); e == nil {
				return nil
			}
		}
		return fmt.Errorf("%s %s: %s", method, path, string(data))
	}
	if response != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(response)
	}
	return nil
}

func (c *Client) ConsensusStatus() (map[string]string, error) {
	var out map[string]string
	err := c.do(http.MethodGet, "/v1/realm/consensus", nil, &out)
	return out, err
}

// Orchestration uses the privileged local management socket, with the same
// admission, durable audit and consensus rules as the authenticated HTTPS API.
func (c *Client) Orchestration(method, path string, body any) (json.RawMessage, error) {
	allowed := false
	for _, collection := range []string{"tasks", "autoscalers", "secrets", "task-schedules"} {
		prefix := "/v1/realm/" + collection
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			allowed = true
		}
	}
	for _, endpoint := range []string{"secret-rotation", "observations", "alerts"} {
		if path == "/v1/realm/"+endpoint {
			allowed = true
		}
	}
	if !allowed {
		return nil, fmt.Errorf("invalid orchestration endpoint")
	}
	var out json.RawMessage
	e := c.do(method, path, body, &out)
	return out, e
}

func (c *Client) Compatibility() (controlapi.CompatibilityInfo, error) {
	var info controlapi.CompatibilityInfo
	e := c.do(http.MethodGet, "/v1/compatibility", nil, &info)
	return info, e
}
func (c *Client) TransitionSchema(id string, expected uint64) (realm.SchemaMigration, error) {
	var m realm.SchemaMigration
	e := c.do(http.MethodPost, "/v1/realm/schema", controlapi.SchemaRequest{ID: id, ExpectedRevision: expected}, &m)
	return m, e
}

func (c *Client) TransitionSchemaTo(id string, expected uint64, target int) (realm.SchemaMigration, error) {
	var m realm.SchemaMigration
	e := c.do(http.MethodPost, "/v1/realm/schema", controlapi.SchemaRequest{ID: id, ExpectedRevision: expected, Target: target}, &m)
	return m, e
}

func (c *Client) leaderRead(path string, result any) error {
	configPath := os.Getenv("TITANUS_HA_CONFIG")
	if configPath == "" {
		configPath = "/etc/titanus/ha.json"
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var conf struct {
		Peers []struct {
			ID  string
			API string
		}
	}
	if err = json.Unmarshal(raw, &conf); err != nil {
		return err
	}
	caPath, certPath, keyPath := os.Getenv("TITANUS_CA"), os.Getenv("TITANUS_CERT"), os.Getenv("TITANUS_KEY")
	if caPath == "" {
		caPath = "/etc/titanus/pki/ca.crt"
	}
	if certPath == "" {
		certPath = "/etc/titanus/pki/node.crt"
	}
	if keyPath == "" {
		keyPath = "/etc/titanus/pki/node.key"
	}
	pem, err := os.ReadFile(caPath)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return fmt.Errorf("invalid Titanus Realm CA")
	}
	clientCert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return err
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		RootCAs: roots, Certificates: []tls.Certificate{clientCert}, MinVersion: tls.VersionTLS13,
	}}
	defer transport.CloseIdleConnections()
	remote := &http.Client{Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	diagnostics, e := c.ConsensusStatus()
	leader := ""
	if e == nil {
		leader = diagnostics["leader_api"]
	}
	sort.SliceStable(conf.Peers, func(i, j int) bool { return conf.Peers[i].API == leader && conf.Peers[j].API != leader })
	var lastErr error
	for _, peer := range conf.Peers {
		origin, e := url.Parse(peer.API)
		if e != nil || origin.Scheme != "https" || origin.Host == "" ||
			origin.User != nil || origin.Path != "" || origin.RawQuery != "" {
			continue
		}
		req, e := http.NewRequest(http.MethodGet, peer.API+path, nil)
		if e != nil {
			lastErr = e
			continue
		}
		resp, e := remote.Do(req)
		if e != nil {
			lastErr = e
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}
		if resp.StatusCode == http.StatusServiceUnavailable {
			lastErr = fmt.Errorf("controller %s not leader", peer.ID)
			continue
		}
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("leader GET %s: HTTP %d: %s", path, resp.StatusCode, string(data))
		}
		if result != nil {
			return json.Unmarshal(data, result)
		}
		return nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no valid controllers configured")
	}
	return lastErr
}
