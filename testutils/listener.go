package testutils

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/cockroachdb/errors"
)

type EventType int

const (
	EventRead EventType = iota
	EventClose
)

type ConnEvent struct {
	Type EventType
	Data []byte
}

type AssertListener struct {
	listener net.Listener
	accepts  chan *AssertConn
}

type AssertConn struct {
	net.Conn
}

func (l *AssertListener) ExpectAccept() (*AssertConn, error) {
	select {
	case event := <-l.accepts:
		return event, nil
	case <-time.After(1 * time.Second):
		return nil, errors.New("timeout waiting for accept event")
	}
}

func (l *AssertListener) Close() {
	l.listener.Close()
	close(l.accepts)
}

func (l *AssertListener) ListenAddr() string {
	return l.listener.Addr().String()
}

func (l *AssertListener) ExpectNoAccept() error {
	select {
	case _, ok := <-l.accepts:
		if ok {
			return errors.New("unexpected accept event")
		}
	default:
	}
	return nil
}

func (c *AssertConn) ExpectRead(data []byte) error {
	buf := make([]byte, len(data))
	_, err := io.ReadFull(c, buf)
	if err != nil {
		return err
	}

	if !bytes.Equal(buf, data) {
		return errors.Errorf("expected to read %q, got %q", string(data), string(buf))
	}

	return nil
}

func (c *AssertConn) ExpectClose() error {
	w, _ := io.Copy(io.Discard, c)
	if w != 0 {
		return errors.Errorf("expected to read 0 bytes, got %d", w)
	}

	return nil
}

func NewAssertListener(t *testing.T) *AssertListener {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	accepts := make(chan *AssertConn, 5)

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			accepts <- &AssertConn{conn}
		}
	}()

	return &AssertListener{
		accepts:  accepts,
		listener: listener,
	}
}
