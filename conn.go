package httptun

import (
	"net"
	"time"

	"github.com/gorilla/websocket"
	"github.com/xtaci/smux"
)

func DefaultSmuxConfig() *smux.Config {
	return &smux.Config{
		Version:           1,
		KeepAliveInterval: 5 * time.Second,
		KeepAliveTimeout:  15 * time.Second,
		MaxFrameSize:      32768,
		MaxReceiveBuffer:  4194304,
		MaxStreamBuffer:   65536,
	}
}

type WebsocketConn struct {
	buf  []byte
	conn *websocket.Conn
}

func NewWebsocketConn(conn *websocket.Conn) *WebsocketConn {
	return &WebsocketConn{
		conn: conn,
	}
}

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

func (c *WebsocketConn) Write(b []byte) (int, error) {
	err := c.conn.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		return 0, err
	}

	return len(b), nil
}

func (c *WebsocketConn) Close() error {
	return c.conn.Close()
}

func (c *WebsocketConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *WebsocketConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

func (c *WebsocketConn) SetDeadline(t time.Time) error {
	// not supported
	return nil
}

func (c *WebsocketConn) SetReadDeadline(t time.Time) error {
	// not supported
	return nil
}

func (c *WebsocketConn) SetWriteDeadline(t time.Time) error {
	// not supported
	return nil
}
