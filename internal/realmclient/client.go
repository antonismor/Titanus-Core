package realmclient

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/antonismor/Titanus-Core/internal/identity"
	"github.com/antonismor/Titanus-Core/internal/source"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
)

type Client struct {
	http *http.Client
}

func New(ca, cert, key string) (*Client, error) {
	tlsConfig, err := identity.TLSConfig(ca, cert, key, false)
	if err != nil {
		return nil, err
	}
	return &Client{
		http: &http.Client{
			Transport: &http.Transport{TLSClientConfig: tlsConfig},
			Timeout:   20 * time.Second,
		},
	}, nil
}

func (c *Client) EnsureSource(address, name string, local *source.Manager) error {
	head, err := http.NewRequest(http.MethodHead, endpoint(address)+"/v1/node/sources/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(head)
	if err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
			return nil
		}
		if resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("Source probe on %s: %s", address, resp.Status)
		}
	}
	if local == nil {
		return fmt.Errorf("local Source manager unavailable")
	}

	reader, writer := io.Pipe()
	exportErr := make(chan error, 1)
	go func() {
		err := local.Export(name, writer)
		_ = writer.CloseWithError(err)
		exportErr <- err
	}()

	req, err := http.NewRequest(http.MethodPut, endpoint(address)+"/v1/node/sources/"+url.PathEscape(name), reader)
	if err != nil {
		_ = reader.Close()
		return err
	}
	req.Header.Set("Content-Type", "application/vnd.titanus.source+gzip")
	resp, err = c.http.Do(req)
	if err != nil {
		_ = reader.Close()
		return err
	}
	defer resp.Body.Close()
	if export := <-exportErr; export != nil {
		return export
	}
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return fmt.Errorf("Source upload to %s: %s: %s", address, resp.Status, strings.TrimSpace(string(data)))
	}
	return nil
}

func (c *Client) EnsureUnit(address string, spec unitruntime.Spec) (unitruntime.State, error) {
	var state unitruntime.State
	err := c.doJSON(http.MethodPost, endpoint(address)+"/v1/node/units", spec, &state)
	return state, err
}

func (c *Client) StartUnit(address, id string) (unitruntime.State, error) {
	var state unitruntime.State
	err := c.doJSON(http.MethodPost, endpoint(address)+"/v1/node/units/"+url.PathEscape(id)+"/start", nil, &state)
	return state, err
}

func (c *Client) StopUnit(address, id string) (unitruntime.State, error) {
	var state unitruntime.State
	err := c.doJSON(http.MethodPost, endpoint(address)+"/v1/node/units/"+url.PathEscape(id)+"/stop", nil, &state)
	return state, err
}

func (c *Client) DeleteUnit(address, id string) error {
	return c.doJSON(http.MethodDelete, endpoint(address)+"/v1/node/units/"+url.PathEscape(id), nil, nil)
}

func (c *Client) InspectUnit(address, id string) (unitruntime.Spec, unitruntime.State, error) {
	var result struct {
		Spec  unitruntime.Spec  `json:"spec"`
		State unitruntime.State `json:"state"`
	}
	err := c.doJSON(http.MethodGet, endpoint(address)+"/v1/node/units/"+url.PathEscape(id), nil, &result)
	return result.Spec, result.State, err
}

func (c *Client) doJSON(method, target string, requestBody, responseBody any) error {
	var body io.Reader
	if requestBody != nil {
		data, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, target, body)
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
		return fmt.Errorf("%s %s: %s", method, target, strings.TrimSpace(string(data)))
	}
	if responseBody != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(responseBody); err != nil {
			return err
		}
	}
	return nil
}

func endpoint(address string) string {
	text := strings.TrimSpace(address)
	if strings.HasPrefix(text, "https://") {
		return strings.TrimRight(text, "/")
	}
	if host, port, err := net.SplitHostPort(text); err == nil && host != "" && port != "" {
		return "https://" + text
	}
	if strings.Count(text, ":") > 1 {
		return "https://" + net.JoinHostPort(text, "9443")
	}
	if strings.Contains(text, ":") {
		return "https://" + text
	}
	return "https://" + net.JoinHostPort(text, "9443")
}
