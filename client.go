package httptun

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/gofrs/uuid"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// Client opens Websocket connections, returning them as a [net.Conn].
// A zero Client is ready for use without initialization.
// If Dialer is nil, [websocket.DefaultDialer] is used.
type Client struct {
	*websocket.Dialer
	Addr           string
	RequestHeaders func() http.Header
	Logger         *zap.SugaredLogger
}

func (c *Client) dialWebsocket(ctx context.Context) (net.Conn, error) {
	if c.Dialer == nil {
		// For callers that didn't pick a dialer, pick a default one on the first call to this function.
		c.Dialer = websocket.DefaultDialer
	}

	var ws *websocket.Conn

	if c.RequestHeaders == nil {
		var err error
		ws, _, err = c.Dialer.DialContext(ctx, c.Addr, nil)
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		ws, _, err = c.Dialer.DialContext(ctx, c.Addr, c.RequestHeaders())
		if err != nil {
			return nil, err
		}
	}

	return NewWebsocketConn(ws), nil
}

// Dial forms a tunnel to the backend TCP endpoint and returns a net.Conn.
//
// Callers are responible for closing the returned connection.
func (c *Client) Dial(ctx context.Context) (net.Conn, error) {
	connectionResult := make(chan error, 1)

	established := false
	stream := NewStream(maxBufferSize, minBufferBehindSize, c.Logger)

	go func() {
		var streamID uuid.UUID

		for !stream.IsClosed() {
			var wsConn net.Conn
			var err error

			c.Logger.Debug("dialing websocket")
			if established {
				wsConn, err = c.dialWebsocket(context.Background())
			} else {
				wsConn, err = c.dialWebsocket(ctx)
			}

			if err != nil {
				c.Logger.Errorw("failed to dial connection", "err", err)
				time.Sleep(100 * time.Millisecond)

				continue
			}

			c.Logger.Debug("websocket successfully dialed")

			err = func() error {
				defer wsConn.Close()

				flow, resumeFrom, err := stream.NewFlow()
				if err != nil {
					return errors.WithMessage(err, "create flow")
				}

				err = binary.Write(wsConn, binary.LittleEndian, Handshake{
					ID:         streamID,
					ResumeFrom: resumeFrom,
				})
				if err != nil {
					return errors.WithMessage(err, "write handshake")
				}

				var handshake Handshake
				err = binary.Read(wsConn, binary.LittleEndian, &handshake)
				if err != nil {
					return errors.WithMessage(err, "read handshake")
				}

				if handshake.ErrorCode != 0 {
					if handshake.ErrorCode == CodeCannotResume || handshake.ErrorCode == CodeSessionNotFound {
						// Stream is not resumable with these errors.
						stream.Close()
					}
					return errors.Newf("handshake error code: %d", handshake.ErrorCode)
				}

				streamID = handshake.ID

				if !established {
					established = true
					select {
					case connectionResult <- nil:
					default:
					}
				}

				err = flow.Resume(wsConn, handshake.ResumeFrom)

				return nil
			}()
			if err != nil {
				c.Logger.Errorf("failed to handshake: %+v\n", err)

				if !established {
					select {
					case connectionResult <- err:
					default:
					}
				}

				time.Sleep(100 * time.Millisecond)
				continue
			}
		}
	}()

	err := <-connectionResult
	if err != nil {
		stream.Close()
		return nil, err
	}

	return stream, nil
}
