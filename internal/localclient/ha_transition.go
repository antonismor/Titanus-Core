package localclient

import (
	"github.com/antonismor/Titanus-Core/internal/controlapi"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"net/http"
)

func (c *Client) ActivateNativeHA(req controlapi.HAActivateRequest) (realm.HAActivation, error) {
	var out realm.HAActivation
	e := c.do(http.MethodPost, "/v1/realm/ha/activate", req, &out)
	return out, e
}
