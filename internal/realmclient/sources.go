package realmclient

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"github.com/antonismor/Titanus-Core/internal/source"
	"io"
	"net/url"
)

// RecoverSource pulls verified immutable content from another configured
// controller. A conflicting local object is never overwritten online.
func (c *Client) RecoverSource(ref realm.SourceRecord, peers []string, local *source.Manager) error {
	if local == nil {
		return fmt.Errorf("Source manager unavailable")
	}
	if exists, e := local.Exists(ref.Name); e != nil {
		return e
	} else if exists {
		digest, e := local.Identity(ref.Name)
		if e != nil {
			return e
		}
		if digest != ref.Digest {
			return fmt.Errorf("local Source differs from committed identity")
		}
		return nil
	}
	var last error
	for _, peer := range peers {
		resp, e := c.http.Get(endpoint(peer) + "/v1/node/sources/" + url.PathEscape(ref.Name))
		if e != nil {
			last = e
			continue
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			last = fmt.Errorf("Source recovery: %s", resp.Status)
			continue
		}
		_, e = local.ImportBundleExpected(io.LimitReader(resp.Body, 64<<30), ref.Name, ref.Digest)
		resp.Body.Close()
		if e == nil {
			return nil
		}
		last = e
	}
	return fmt.Errorf("no controller can supply committed Source %s: %v", ref.Name, last)
}
