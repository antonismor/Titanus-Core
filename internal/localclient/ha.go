package localclient

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func (c *Client) PublishSource(name string) (realm.SourceRecord, error) {
	var ref realm.SourceRecord
	err := c.do(http.MethodPost, "/v1/realm/sources/"+url.PathEscape(name), nil, &ref)
	return ref, err
}
func (c *Client) PutGateway(g realm.Gateway) error {
	return c.do(http.MethodPost, "/v1/realm/gateways/"+url.PathEscape(g.Name), g, nil)
}
func (c *Client) TransferGateway(name, destination string) error {
	return c.do(http.MethodPost, "/v1/realm/gateways/"+url.PathEscape(name)+"/transfer", map[string]string{"destination": destination}, nil)
}
func (c *Client) RevokeCertificate(serial string) ([]byte, error) {
	// Preserve the CRL's PEM response instead of treating it as JSON.
	body := fmt.Sprintf(`{"serial":%q}`, serial)
	req, err := http.NewRequest(http.MethodPost, c.base+"/v1/identity/crl", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("revocation failed: HTTP %d", resp.StatusCode)
	}
	return data, nil
}
