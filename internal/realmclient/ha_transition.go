package realmclient

import (
	"github.com/antonismor/Titanus-Core/internal/controlapi"
	"net/http"
)

func (c *Client) HAReadiness(address, id string) (controlapi.HAReadiness, error) {
	var out controlapi.HAReadiness
	e := c.doJSON(http.MethodPost, endpoint(address)+"/v1/node/ha-readiness", map[string]string{"id": id}, &out)
	return out, e
}
