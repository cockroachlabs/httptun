package httptun

import (
	"context"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/cockroachlabs/httptun/testutils"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zaptest"
)

func TestTunnel(t *testing.T) {
	//
	// Server setup
	//

	// dst is a TCP listener that acts as a destination backend.
	dst := testutils.NewAssertListener(t)
	l := zaptest.NewLogger(t)

	// tunServer is a httptun server that proxies connections to dst.
	tunServer := NewServer(dst.ListenAddr(), l.Sugar().Named("server"))
	httpServer := httptest.NewServer(tunServer)

	//
	// Client setup
	//
	u, err := url.Parse(httpServer.URL)
	assert.NoError(t, err)
	u.Scheme = "ws"

	dialer := func(ctx context.Context) (*websocket.Conn, error) {
		ws, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
		if err != nil {
			return nil, err
		}

		return ws, nil
	}

	client := NewClient(context.Background(), dialer, 3, l.Sugar().Named("client"))
	assert.NoError(t, client.Connect(context.Background()))

	// Connect to the backend, expect some data to be transferred.
	bareConn, err := client.Dial(context.Background())
	assert.NoError(t, err)
	sentConn := &testutils.AssertConn{bareConn}

	receivedConn, err := dst.ExpectAccept()
	assert.NoError(t, err)

	_, err = sentConn.Write([]byte("ping"))
	assert.NoError(t, err)

	assert.NoError(t, receivedConn.ExpectRead([]byte("ping")))

	_, err = receivedConn.Write([]byte("pong"))
	assert.NoError(t, err)

	assert.NoError(t, sentConn.ExpectRead([]byte("pong")))

	// Have two connections going at once, verify both work.
	bareConn2, err := client.Dial(context.Background())
	assert.NoError(t, err)
	sentConn2 := &testutils.AssertConn{bareConn2}

	receivedConn2, err := dst.ExpectAccept()
	assert.NoError(t, err)

	_, err = sentConn.Write([]byte("ping again"))
	assert.NoError(t, err)

	assert.NoError(t, receivedConn.ExpectRead([]byte("ping again")))

	_, err = sentConn2.Write([]byte("ping on new conn"))
	assert.NoError(t, err)

	assert.NoError(t, receivedConn2.ExpectRead([]byte("ping on new conn")))

	receivedConn2.Close()
	assert.NoError(t, sentConn2.ExpectClose())

	sentConn.Close()
	assert.NoError(t, receivedConn.ExpectClose())

	assert.NoError(t, dst.ExpectNoAccept())
}
