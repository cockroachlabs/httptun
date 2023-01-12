package httptun

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap/zaptest"
)

func TestTunnel(t *testing.T) {
	// Set up a network like this:
	//
	// upstream   tunnel server   tunnel client
	//    TCP <---> TCP....WS <---> WS

	// Start the upstream TCP server.
	ln := listen(t)

	// Start the tunnel server, which uses ln as its upstream.
	srv := NewServer(ln.Addr(), time.Millisecond*200, zaptest.NewLogger(t).Sugar().Named("server"))
	defer srv.Close()
	// Make a test HTTP server with the standard library.
	httpServer := httptest.NewServer(srv)
	t.Logf("serving http at %s", httpServer.URL)

	// Configure a tunnel client that connects to srv.
	u, err := url.Parse(httpServer.URL)
	assertNil(t, err)
	u.Scheme = "ws"
	client := Client{Addr: u.String(), Logger: zaptest.NewLogger(t).Sugar().Named("client")}

	// Establish a connection from the downstream to the upstream.
	src_one, err := client.Dial(context.Background())
	assertNil(t, err)
	// Retrieve the corresponding upstream connection.
	dst_one, err := ln.NextConn(time.Second)
	assertNil(t, err)

	// Test both directions of the connection.
	assertWrite(t, src_one, []byte("ping"))
	assertRead(t, []byte("ping"), dst_one)
	assertWrite(t, dst_one, []byte("pong"))
	assertRead(t, []byte("pong"), src_one)

	// Establish another connection.
	src_two, err := client.Dial(context.Background())
	assertNil(t, err)
	dst_two, err := ln.NextConn(time.Second)
	assertNil(t, err)

	// Test the function of the first connection.
	assertWrite(t, src_one, []byte("ping again"))
	assertRead(t, []byte("ping again"), dst_one)

	// Test the second connection.
	assertWrite(t, src_two, []byte("ping on new conn"))
	assertRead(t, []byte("ping on new conn"), dst_two)

	// Close the connection from the upstream side to test the downstream side is closed.
	dst_two.Close()
	assertClosed(t, src_two)
	// Close the connection from the downstream side to test the upstream side is closed.
	src_one.Close()
	assertClosed(t, dst_one)

	assertEqual(t, 0, ln.UnhandledConns())

	assertNil(t, ln.Close())
}

func assertNil(t *testing.T, got interface{}) {
	t.Helper()
	if got != nil {
		t.Fatalf("want nil, got %v", got)
	}
}

func assertEqual[T comparable](t *testing.T, want, got T) {
	t.Helper()
	if want != got {
		t.Fatalf("want=%v got=%v", want, got)
	}
}

func assertRead(t *testing.T, want []byte, r io.Reader) {
	t.Helper()
	got := make([]byte, len(want))
	_, err := io.ReadFull(r, got)
	if err != nil || !bytes.Equal(want, got) {
		t.Fatalf("want=%s got=%s err=%v", want, got, err)
	}
}

func assertWrite(t *testing.T, w io.Writer, buf []byte) {
	t.Helper()
	n, err := w.Write(buf)
	if err != nil || n != len(buf) {
		t.Fatalf("want=%dB got=%dB err=%v", len(buf), n, err)
	}
}

func assertClosed(t *testing.T, r io.ReadCloser) {
	t.Helper()
	n, err := io.Copy(io.Discard, r)
	t.Logf("reading from a presumed closed io.Reader: %v", err)
	if n != 0 {
		t.Fatalf("want=%dB got=%dB: %v", 0, n, err)
	}
	assertNil(t, r.Close())
}

type listener struct {
	cancel context.CancelFunc
	t      *testing.T
	ln     net.Listener
	conns  chan net.Conn
}

// listen starts a TCP server at an arbitrary port on localhost.
// It also starts an Accept() loop in a goroutine, enqueueing new connections in a buffered channel.
func listen(t *testing.T) *listener {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	assertNil(t, err)

	ctx, cancel := context.WithCancel(context.Background())

	// In the current tests, we only expect one connection at a time to sit in this buffer.
	conns := make(chan net.Conn, 5)
	var i int64
	go func() {
		for {
			i := atomic.AddInt64(&i, 1)
			conn, err := ln.Accept()
			// Check for context cancellation before checking Accept()'s error.
			// During shutdown, Accept() will block until ln has closed.
			// In our Close() method, we cancel the context first, then close ln.
			// Closing ln causes an Accept() error, which we don't care about if we intentionally closed the connection.
			select {
			case <-ctx.Done():
				t.Log("exiting accept loop")
				return
			default:
			}
			if err != nil {
				t.Logf("#%d: could not accept: %v", i, err)
				return
			}
			t.Logf("#%d: accepted new connection", i)
			conns <- conn
		}
	}()

	return &listener{
		t:      t,
		cancel: cancel,
		conns:  conns,
		ln:     ln,
	}
}

func (l *listener) NextConn(wait time.Duration) (net.Conn, error) {
	select {
	case event := <-l.conns:
		return event, nil
	case <-time.After(wait):
		return nil, fmt.Errorf("timed out after %s", wait)
	}
}

func (l *listener) UnhandledConns() int {
	return len(l.conns)
}

func (l *listener) Close() error {
	l.cancel()
	if err := l.ln.Close(); err != nil {
		return err
	}
	close(l.conns)
	return nil
}

func (l *listener) Addr() string {
	return l.ln.Addr().String()
}

func TestConnectionResume(t *testing.T) {
	// Set up a network like this:
	//
	// upstream   tunnel server   tunnel client
	//    TCP <---> TCP....WS <---> WS

	ln := listen(t)

	server := NewServer(ln.Addr(), time.Millisecond*200, zaptest.NewLogger(t).Sugar().Named("server"))
	defer server.Close()
	httpServer := httptest.NewServer(server)

	u, err := url.Parse(httpServer.URL)
	assertNil(t, err)
	u.Scheme = "ws"

	var underlyingConn net.Conn

	dialer := &websocket.Dialer{
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var netDialer net.Dialer
			var err error
			underlyingConn, err = netDialer.DialContext(ctx, network, addr)
			return underlyingConn, err
		},
	}

	client := &Client{
		Dialer: dialer,
		Addr:   u.String(),
		Logger: zaptest.NewLogger(t).Sugar().Named("client"),
	}

	// Establish a connection from the downstream to the upstream.
	src_one, err := client.Dial(context.Background())
	assertNil(t, err)
	// Retrieve the corresponding upstream connection.
	dst_one, err := ln.NextConn(time.Second)
	assertNil(t, err)

	// Test both directions of the connection.
	assertWrite(t, src_one, []byte("ping"))
	assertRead(t, []byte("ping"), dst_one)
	assertWrite(t, dst_one, []byte("pong"))
	assertRead(t, []byte("pong"), src_one)

	// Interrupt the underlying connection
	underlyingConn.Close()

	// Test both directions of the connection again to make sure that it has resumed.
	assertWrite(t, src_one, []byte("ping2"))
	assertRead(t, []byte("ping2"), dst_one)
	assertWrite(t, dst_one, []byte("pong2"))
	assertRead(t, []byte("pong2"), src_one)

	// Close one side of the connection.
	src_one.Close()
	assertClosed(t, dst_one)

	assertEqual(t, 0, ln.UnhandledConns())

	assertNil(t, ln.Close())
}
