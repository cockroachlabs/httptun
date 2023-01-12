package httptun

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gofrs/uuid"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

const (
	maxBufferSize = 16 * 1e6 // 16 MB

	// 10 MB, this should be more than the total number of bytes that are possibly in-flight in the network and
	// kernels of the client and server.
	minBufferBehindSize = 10 * 1e6

	defaultByteBufferSize = 65535
)

type Server struct {
	dst      string
	upgrader *websocket.Upgrader
	logger   *zap.SugaredLogger
	streams  map[uuid.UUID]*Stream
	mu       sync.Mutex

	isClosed    bool
	done        chan struct{}
	janitorDone chan struct{}
}

// ErrorCode represents an error code in a handshake.
type ErrorCode int64

const (
	CodeNoError = ErrorCode(iota)
	CodeSessionNotFound
	CodeCannotResume
	CodeDialError
)

// Handshake is the handshake message sent by both the client and server to negotiate connection resumption.
type Handshake struct {
	ID         uuid.UUID
	ResumeFrom int64
	ErrorCode  ErrorCode
}

// janitor is a goroutine that periodically closes idle streams whose lastActivity was more than timeout ago.
func (s *Server) janitor(timeout time.Duration) {
	t := time.NewTicker(timeout)
	defer close(s.janitorDone)
	defer t.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			break
		}

		s.mu.Lock()
		if s.isClosed {
			s.mu.Unlock()
			return
		}

		for id, stream := range s.streams {
			if time.Since(stream.lastActivity) > timeout {
				stream.Close()
				delete(s.streams, id)
			}
		}
		s.mu.Unlock()
	}
}

// NewServer creates a new server that can be used to serve HTTP requests over a websocket connection.
// timeout specifies the maximum time that a stream can be idle with no active flows before it is closed.
func NewServer(dst string, timeout time.Duration, logger *zap.SugaredLogger) *Server {
	s := &Server{
		dst: dst,
		upgrader: &websocket.Upgrader{
			ReadBufferSize:  256000,
			WriteBufferSize: 256000,
		},
		streams:     make(map[uuid.UUID]*Stream),
		logger:      logger,
		done:        make(chan struct{}),
		janitorDone: make(chan struct{}),
	}

	go s.janitor(timeout)

	return s
}

// Close shuts down the server, closing existing streams and rejects new connections.
func (s *Server) Close() {
	s.mu.Lock()

	if s.isClosed {
		s.mu.Unlock()
		return
	}

	s.isClosed = true
	close(s.done)

	for id, stream := range s.streams {
		stream.Close()
		delete(s.streams, id)
	}

	s.mu.Unlock()
	<-s.janitorDone
}

// ServeHTTP implements the http.Handler interface.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Warnf("error upgrading connection: %+v", err)
		return
	}

	downstream := NewWebsocketConn(conn)
	defer downstream.Close()

	// Read the handshake from the client.
	var receivedHandshake Handshake
	err = binary.Read(downstream, binary.LittleEndian, &receivedHandshake)
	if err != nil {
		s.logger.Warnf("error reading handshake: %+v", err)
		return
	}

	flow := s.handleHandshake(r.RemoteAddr, downstream, &receivedHandshake)
	if flow == nil {
		return
	}

	flow.logger.Infof("flow started")
	err = flow.Resume(downstream, receivedHandshake.ResumeFrom)
	if err != nil {
		s.logger.Warnf("error resuming flow: %+v", err)
		return
	}
}

// handleHandshake handles the handshake message from the client and responds to it as appropriate.
// If the stream doesn't already exist, it connects to the upstream server and starts goroutines to copy the
// buffered stream to and from the upstream connection. It returns a flow of the stream that was resumed or nil
// if the handshake failed.
func (s *Server) handleHandshake(remoteAddr string, downstream net.Conn, receivedHandshake *Handshake) *Flow {
	if !receivedHandshake.ID.IsNil() {
		// We received a non-empty ID in the handshake, which indicates that the client wants to resume a previous
		// stream.
		var flow *Flow

		func() {
			s.mu.Lock()
			defer s.mu.Unlock()

			if s.isClosed {
				flow = nil
				return
			}

			var found bool
			stream, found := s.streams[receivedHandshake.ID]
			if !found {
				s.logger.Warnf("received handshake for non-existent session: %s", receivedHandshake.ID)
				// Session does not exist, respond with error.
				err := binary.Write(downstream, binary.LittleEndian, Handshake{
					ID:        uuid.Nil,
					ErrorCode: CodeSessionNotFound,
				})
				if err != nil {
					s.logger.Warnf("error writing handshake: %+v", err)
				}
				return
			}

			var resumeFrom int64
			var err error
			flow, resumeFrom, err = stream.NewFlow()
			if err != nil {
				// The stream is not resumable, respond with error.
				writeErr := binary.Write(downstream, binary.LittleEndian, Handshake{
					ID:        receivedHandshake.ID,
					ErrorCode: CodeCannotResume,
				})
				if writeErr != nil {
					s.logger.Warnf("error writing handshake: %+v", err)
				}

				s.logger.Warnf("error creating new flow: %+v", err)
				return
			}

			// A new flow was successfully started, respond with the resume position.
			err = binary.Write(downstream, binary.LittleEndian, Handshake{
				ID:         receivedHandshake.ID,
				ResumeFrom: resumeFrom,
			})
			if err != nil {
				s.logger.Warnf("error writing handshake: %+v", err)
				flow = nil
				return
			}

			s.logger.Infof("resuming session from %d", resumeFrom)
		}()

		return flow
	}

	// We received an empty ID in the handshake, which indicates that the client wants to start a new stream.
	// Dial the destination server.
	upstream, err := net.Dial("tcp", s.dst)
	if err != nil {
		s.logger.Warnf("failed to dial %q: %+v", s.dst, err)
		binary.Write(downstream, binary.LittleEndian, Handshake{
			ErrorCode: CodeDialError,
		})
		return nil
	}

	// Start the new stream.
	id := uuid.Must(uuid.NewV4())

	s.mu.Lock()
	if s.isClosed {
		return nil
	}

	stream := NewStream(maxBufferSize, minBufferBehindSize, s.logger.With(
		zap.String("stream", id.String()),
		zap.String("remote_ip", remoteAddr),
	))

	s.streams[id] = stream
	s.mu.Unlock()

	stream.logger.Infof("opened new stream")
	err = binary.Write(downstream, binary.LittleEndian, Handshake{
		ID:         id,
		ResumeFrom: 0,
	})
	if err != nil {
		stream.logger.Warnf("error writing handshake: %+v", err)
		stream.Close()
		upstream.Close()
		return nil
	}

	// Copy the data from the stream to the destination server and vice versa. These goroutines are long-lived for the
	// lifetime of the stream.
	go func() {
		defer stream.Close()
		defer upstream.Close()
		_, err = io.Copy(stream, upstream)
		if err != nil {
			stream.logger.Warnf("error copying from outbound: %+v", err)
		}
	}()

	go func() {
		defer stream.Close()
		defer upstream.Close()
		_, err = io.Copy(upstream, stream)
		if err != nil {
			stream.logger.Warnf("error copying from inbound: %+v", err)
		}
	}()

	flow, resumeFrom, err := stream.NewFlow()
	if err != nil {
		stream.logger.Errorf("error creating new flow (this should never happen): %+v", err)
		return nil
	}

	if resumeFrom != 0 {
		stream.logger.Errorf("resume from should be 0 for new sessions (this should never happen)")
		return nil
	}

	return flow
}
