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

// Package httptun implements the client and server for the TCP over HTTP tunnel.
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

// Server implements http.Handler for the server (termination point) of a TCP over HTTP tunnel.
type Server struct {
	dst      string
	upgrader *websocket.Upgrader
	logger   *zap.SugaredLogger
	streams  map[uuid.UUID]*Stream
	mu       sync.Mutex

	isClosed    bool
	done        chan struct{}
	janitorDone chan struct{}

	onClose func(streamID uuid.UUID, startTime time.Time, bytesRead, bytesWritten int64)
}

// ErrorCode represents an error code in a handshake.
type ErrorCode int64

// List of error codes.
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

// janitor is a goroutine that periodically closes idle streams whose lastFlowTime was more than timeout ago.
func (s *Server) janitor(timeout time.Duration) {
	t := time.NewTicker(timeout)
	defer close(s.janitorDone)
	defer t.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}

		s.mu.Lock()
		if s.isClosed {
			s.mu.Unlock()
			return
		}

		for id, stream := range s.streams {
			stream.cond.L.Lock()
			lastFlowTime := stream.lastFlowTime
			stream.cond.L.Unlock()
			if !lastFlowTime.IsZero() && time.Since(lastFlowTime) > timeout {
				stream.closeInternal()
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

// OnStreamClose sets a callback handler for when a stream closes. The callback should never block as it is not
// called in a separate goroutine.
func (s *Server) OnStreamClose(f func(streamID uuid.UUID, startTime time.Time, bytesRead, bytesWritten int64)) {
	s.onClose = f
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

	downstream := NewWebsocketConn(conn, new(sync.Mutex), func() {})
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

	flow.Wait()
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

	if s.onClose != nil {
		stream.OnClose(func(startTime time.Time, bytesRead, bytesWritten int64) {
			s.onClose(id, startTime, bytesRead, bytesWritten)
		})
	}

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
		defer stream.closeInternal()
		defer upstream.Close()
		_, err = io.Copy(stream, upstream)
		if err != nil {
			stream.logger.Warnf("error copying from outbound: %+v", err)
		}
	}()

	go func() {
		defer stream.closeInternal()
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
