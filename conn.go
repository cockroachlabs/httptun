package httptun

import (
	"net"
	"time"

	"github.com/gorilla/websocket"
)

// WebsocketConn wraps *websocket.Conn into implementing net.Conn
type WebsocketConn struct {
	buf  []byte
	conn *websocket.Conn
}

// NewWebsocketConn creates a new WebsocketConn from an open websocket connection which implements net.Conn
func NewWebsocketConn(conn *websocket.Conn) *WebsocketConn {
	return &WebsocketConn{
		conn: conn,
	}
}

// Read implements net.Conn
func (c *WebsocketConn) Read(b []byte) (int, error) {
	if len(c.buf) > 0 {
		n := copy(b, c.buf)
		c.buf = c.buf[n:]
		return n, nil
	}

	for {
		t, data, err := c.conn.ReadMessage()
		if err != nil {
			return 0, err
		}

		if t != websocket.BinaryMessage {
			continue
		}

		c.buf = data

		return c.Read(b)
	}
}

// Write implements net.Conn
func (c *WebsocketConn) Write(b []byte) (int, error) {
	err := c.conn.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		return 0, err
	}

	return len(b), nil
}

// Close implements net.Conn
func (c *WebsocketConn) Close() error {
	return c.conn.Close()
}

// LocalAddr implements net.Conn
func (c *WebsocketConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

// RemoteAddr implements net.Conn
func (c *WebsocketConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// SetDeadline implements net.Conn
func (c *WebsocketConn) SetDeadline(t time.Time) error {
	// not supported
	return nil
}

// SetReadDeadline implements net.Conn
func (c *WebsocketConn) SetReadDeadline(t time.Time) error {
	// not supported
	return nil
}

// SetWriteDeadline implements net.Conn
func (c *WebsocketConn) SetWriteDeadline(t time.Time) error {
	// not supported
	return nil
}
