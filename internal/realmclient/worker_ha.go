package realmclient

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/unitruntime"
	"net/http"
)

func (c *Client) ListUnits(address string) ([]unitruntime.State, error) {
	var units []unitruntime.State
	err := c.doJSON(http.MethodGet, endpoint(address)+"/v1/node/units?strict=1", nil, &units)
	if err == nil && units == nil {
		return nil, fmt.Errorf("missing strict Unit recovery inventory")
	}
	return units, err
}
