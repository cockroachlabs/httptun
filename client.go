package httptun

import (
	"context"
	"encoding/binary"
	"net"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/gofrs/uuid"
	"github.com/gorilla/websocket"
	"github.com/xtaci/smux"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"
)

// WebsocketDialer is a function that dials a websocket connection, it is to be implemented by the caller and is
// used in the client.
type WebsocketDialer func(ctx context.Context) (*websocket.Conn, error)

// Client represents a tunnel client that can be used to dial connections on the server.
type Client struct {
	sessionPool *sessionPool
	dialer      WebsocketDialer
	wg          *sync.WaitGroup
	logger      *zap.SugaredLogger

	ctx    context.Context
	cancel func()
}

// NewClient creates a new client that can be used to dial connections to the backend server.
// The context provided must be long-lived as it is used to manage the connection pool.
func NewClient(
	ctx context.Context,
	dialer WebsocketDialer,
	connPoolSize int,
	logger *zap.SugaredLogger,
) *Client {
	ctx, cancel := context.WithCancel(ctx)
	return &Client{
		sessionPool: &sessionPool{
			sessions: make([]*WrappedSession, connPoolSize),
		},
		dialer: dialer,
		wg:     new(sync.WaitGroup),
		logger: logger,

		ctx:    ctx,
		cancel: cancel,
	}
}

// Close closes all the sessions in the connection pool and returns when all the connections have been closed.
func (c *Client) Close() {
	c.cancel()
	c.wg.Wait()
}

// sessionPool is a pool of smux sessions, there is a 1:1 mapping of connections to smux sessions,
// each session can be multiplexed into multiple streams.
type sessionPool struct {
	sessions []*WrappedSession
	mutex    sync.Mutex
	c        int
}

// getSession returns a session from the pool round-robin style using an auto-incrementing counter.
func (p *sessionPool) getSession() *WrappedSession {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	p.c = (p.c + 1) % len(p.sessions)
	return p.sessions[p.c]
}

func (c *Client) Connect(ctx context.Context) error {
	for i := range c.sessionPool.sessions {
		if i == 0 {
			// Block on the first connection to determine connection errors.
			session, err := c.dialSession(ctx, true)
			if err != nil {
				return err
			}

			c.sessionPool.sessions[i] = session
		} else {
			// Assume successive connections are successful, if they are not they will auto attempt to reconnect.
			c.sessionPool.sessions[i], _ = c.dialSession(ctx, false)
		}
	}

	return nil
}

// Dial forms a tunnel to the backend TCP endpoint and returns a net.Conn.
func (c *Client) Dial(ctx context.Context) (net.Conn, error) {
	connectionResult := make(chan error, 1)

	established := false
	stream := NewStream(maxBufferSize, minBufferBehindSize, c.logger)

	go func() {
		c.logger.Debugf("starting dial loop")
		var streamID uuid.UUID

		for !stream.IsClosed() {
			var conn net.Conn
			var err error

			if established {
				conn, err = c.sessionPool.getSession().Dial(context.Background())
			} else {
				conn, err = c.sessionPool.getSession().Dial(ctx)
			}

			if err != nil {
				c.logger.Errorw("failed to dial connection", "err", err)
				time.Sleep(100 * time.Millisecond)

				continue
			}

			err = func() error {
				defer conn.Close()

				flow, resumeFrom, err := stream.NewFlow()
				if err != nil {
					return errors.WithMessage(err, "create flow")
				}

				err = binary.Write(conn, binary.LittleEndian, Handshake{
					ID:         streamID,
					ResumeFrom: resumeFrom,
				})
				if err != nil {
					return errors.WithMessage(err, "write handshake")
				}

				var handshake Handshake
				err = binary.Read(conn, binary.LittleEndian, &handshake)
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

				c.logger.Debugf("dial loop established connection")
				if !established {
					stream.remoteAddr = conn.RemoteAddr()
					stream.localAddr = conn.LocalAddr()

					established = true
					select {
					case connectionResult <- nil:
					default:
					}
				}

				err = flow.Resume(conn, handshake.ResumeFrom)
				c.logger.Debugf("connection closed: %+v", err)

				return nil
			}()
			if err != nil {
				c.logger.Errorf("failed to handshake: %+v\n", err)

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

type WrappedSession struct {
	session      *smux.Session
	sessionMutex *semaphore.Weighted
}

// Dial connects to the backend server and returns a net.Conn.
func (s *WrappedSession) Dial(ctx context.Context) (net.Conn, error) {
	err := s.sessionMutex.Acquire(ctx, 1)
	if err != nil {
		return nil, err
	}
	stream, err := s.session.OpenStream()
	if err != nil {
		// Close the session if it's no longer valid.
		s.session.Close()
	}
	s.sessionMutex.Release(1)

	return stream, err
}

// dialSession connects to the backend server and returns a session. If waitForConn is true, the call to Connect
// will block and return an error if the connection fails, otherwise the connection is asynchronous and errors
// may not be reported.
func (c *Client) dialSession(dialCtx context.Context, waitForConn bool) (*WrappedSession, error) {
	result := make(chan error)

	s := &WrappedSession{
		sessionMutex: semaphore.NewWeighted(1),
	}

	if err := s.sessionMutex.Acquire(dialCtx, 1); err != nil {
		return nil, err
	}

	go func() {
		c.wg.Add(1)
		defer c.wg.Done()

		shouldRelease := true

		defer func() {
			if shouldRelease {
				s.sessionMutex.Release(1)
			}
		}()

		connResultSent := !waitForConn
		for {
			conn, err := c.dialer(dialCtx)
			if err != nil {
				if !connResultSent {
					connResultSent = true
					result <- err
					return
				}

				time.Sleep(time.Second * 3)
				continue
			}

			shouldExit := false

			func() {
				defer conn.Close()

				wsConn := NewWebsocketConn(conn)

				newSession, err := smux.Client(wsConn, DefaultSmuxConfig())
				if err != nil {
					if !connResultSent {
						connResultSent = true
						result <- err
						shouldExit = true
						return
					}

					c.logger.Infof("failed to create session, retrying in 3 seconds: %v", err)
					time.Sleep(time.Second * 3)
					return
				}

				defer newSession.Close()

				s.session = newSession
				if !connResultSent {
					connResultSent = true
					result <- nil
				} else {
					c.logger.Debugf("connected successfully")
				}

				s.sessionMutex.Release(1)
				shouldRelease = false

				select {
				case <-s.session.CloseChan():
					c.logger.Debugf("session closed, reconnecting...")
				case <-c.ctx.Done():
					shouldExit = true
				}
			}()

			if shouldExit {
				return
			}

			if err := s.sessionMutex.Acquire(c.ctx, 1); err != nil {
				return
			}
			shouldRelease = true
		}
	}()

	if waitForConn {
		return s, <-result
	}

	return s, nil
}
