package realmclient

import (
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"net/http"
)

func (c *Client) ListUnits(address string) ([]unitruntime.State, error) {
	var units []unitruntime.State
	err := c.doJSON(http.MethodGet, endpoint(address)+"/v1/node/units", nil, &units)
	return units, err
}
