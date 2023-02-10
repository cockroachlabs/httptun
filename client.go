package httptun

import (
	"context"
	"net"

	"github.com/gorilla/websocket"
)

// Client opens Websocket connections, returning them as a [net.Conn].
// A zero Client is ready for use without initialization.
// If Dialer is nil, [websocket.DefaultDialer] is used.
type Client struct {
	*websocket.Dialer
	Addr string
}

// Dial forms a tunnel to the backend TCP endpoint and returns a net.Conn.
//
// Callers are responible for closing the returned connection.
func (c *Client) Dial(ctx context.Context) (net.Conn, error) {
	if c.Dialer == nil {
		// For callers that didn't pick a dialer, pick a default one on the first call to this function.
		c.Dialer = websocket.DefaultDialer
	}
	ws, _, err := c.Dialer.DialContext(ctx, c.Addr, nil)
	if err != nil {
		return nil, err
	}
	return NewWebsocketConn(ws), nil
}
