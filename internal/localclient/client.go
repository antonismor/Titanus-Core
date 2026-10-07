package localclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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
		return fmt.Errorf("%s %s: %s", method, path, string(data))
	}
	if response != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(response)
	}
	return nil
}
