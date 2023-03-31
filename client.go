// Copyright 2023 Cockroach Labs, Inc.

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package httptun

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/gofrs/uuid"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

const defaultKeepAlive = 5 * time.Second

// Client opens Websocket connections, returning them as a [net.Conn].
// A zero Client is ready for use without initialization.
// If Dialer is nil, [websocket.DefaultDialer] is used.
type Client struct {
	*websocket.Dialer
	Addr           string
	RequestHeaders func() http.Header
	Logger         *zap.SugaredLogger
	// KeepAlive sets the interval and timeout (2*KeepAlive) of WebSocket keep alive messages.
	// If unset, defaults to 5 seconds.
	KeepAlive time.Duration
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

	writeMu := new(sync.Mutex)
	var lastRespPtr atomic.Pointer[time.Time]
	now := time.Now()
	lastRespPtr.Store(&now)
	keepAliveStopped := make(chan struct{})
	stopKeepAlive := make(chan struct{})

	c.Logger.Debugf("constructed keep alive: %p", &stopKeepAlive)

	ws.SetPongHandler(func(string) error {
		c.Logger.Debugf("received keep alive pong")
		now := time.Now()
		lastRespPtr.Store(&now)
		return nil
	})

	go func() {
		logger := c.Logger.With("ws", fmt.Sprintf("%p", ws))
		logger.Debugf("starting keep alive")
		defer close(keepAliveStopped)

		keepAlive := c.KeepAlive

		if keepAlive == 0 {
			keepAlive = defaultKeepAlive
		}

		t := time.NewTicker(keepAlive)

		var writeFailure atomic.Bool

		defer t.Stop()
		defer ws.Close()

		for {
			logger.Debugf("waiting on keep alive: %p", &stopKeepAlive)
			select {
			case <-stopKeepAlive:
				logger.Debugf("keep alive stopping")
				return
			case <-t.C:
			}
			if writeFailure.Load() {
				return
			}

			lastResp := lastRespPtr.Load()
			if time.Since(*lastResp) > 2*c.KeepAlive {
				logger.Warnf("keep alive timeout, closing websocket connection")
				return
			}

			logger.Debugf("sending keep alive")
			go func() {
				writeMu.Lock()
				defer writeMu.Unlock()
				err := ws.WriteMessage(websocket.PingMessage, []byte("ka"))
				if err != nil {
					writeFailure.Store(true)
				}
			}()
		}
	}()

	return NewWebsocketConn(ws, writeMu, func() {
		// Signal to stop keep alives.
		close(stopKeepAlive)
		// Wait until keep alives are stopped.
		<-keepAliveStopped
	}), nil
}

var errWebsocketDial = errors.New("websocket dial error")

// Dial forms a tunnel to the backend TCP endpoint and returns a net.Conn.
//
// Callers are responsible for closing the returned connection.
func (c *Client) Dial(ctx context.Context) (net.Conn, error) {
	connectionResult := make(chan error, 1)

	firstAttempt := true
	stream := NewStream(maxBufferSize, minBufferBehindSize, c.Logger)

	go func() {
		var streamID uuid.UUID

		for !stream.IsClosed() {
			var wsConn net.Conn
			var err error

			waitFlowFinish, err := func() (func(), error) {
				c.Logger.Debug("dialing websocket")
				if firstAttempt {
					wsConn, err = c.dialWebsocket(ctx)
				} else {
					wsConn, err = c.dialWebsocket(context.Background())
				}
				if err != nil {
					return nil, errors.Mark(err, errWebsocketDial)
				}

				flow, resumeFrom, err := stream.NewFlow()
				if err != nil {
					return nil, errors.WithMessage(err, "create flow")
				}

				err = binary.Write(wsConn, binary.LittleEndian, Handshake{
					ID:         streamID,
					ResumeFrom: resumeFrom,
				})
				if err != nil {
					return nil, errors.WithMessage(err, "write handshake")
				}

				var handshake Handshake
				err = binary.Read(wsConn, binary.LittleEndian, &handshake)
				if err != nil {
					return nil, errors.WithMessage(err, "read handshake")
				}

				if handshake.ErrorCode != 0 {
					if handshake.ErrorCode == CodeCannotResume || handshake.ErrorCode == CodeSessionNotFound {
						// Stream is not resumable with these errors.
						stream.closeInternal()
					}
					return nil, errors.Newf("handshake error code: %d", handshake.ErrorCode)
				}

				streamID = handshake.ID

				err = flow.Resume(wsConn, handshake.ResumeFrom)
				if err != nil {
					return nil, errors.WithMessage(err, "resume flow")
				}

				return flow.Wait, nil
			}()
			if err != nil {
				c.Logger.Errorf("failed to handshake: %+v\n", err)

				if !errors.Is(err, errWebsocketDial) {
					wsConn.Close()
				}

				if firstAttempt {
					connectionResult <- err
					return
				}

				time.Sleep(100 * time.Millisecond)
				continue
			} else {
				if firstAttempt {
					connectionResult <- nil
				}

				c.Logger.Debugf("flow established")
				waitFlowFinish()
			}

			c.Logger.Debugf("flow finished, reconnecting (firstAttempt=%v)", firstAttempt)

			firstAttempt = false
		}
	}()

	err := <-connectionResult
	if err != nil {
		stream.Close()
		return nil, err
	}

	return stream, nil
}
