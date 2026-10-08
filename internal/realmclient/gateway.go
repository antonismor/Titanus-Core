package realmclient

import (
	"fmt"
	"github.com/antonismor/Titanus-Core/internal/realm"
	"net/http"
)

func (c *Client) WithdrawGateway(address string, g realm.Gateway) error {
	var ack realm.Gateway
	if err := c.doJSON(http.MethodPost, endpoint(address)+"/v1/node/gateway/withdraw", g, &ack); err != nil {
		return err
	}
	if ack != g {
		return fmt.Errorf("Gateway withdrawal acknowledgement differs from exact owner/epoch")
	}
	return nil
}
