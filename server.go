package httptun

import (
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/gofrs/uuid"
	"github.com/gorilla/websocket"
	"github.com/xtaci/smux"
	"go.uber.org/zap"
)

const (
	maxBufferSize = 16 * 1e6 // 16 MB

	// 10 MB, this should be more than /proc/sys/net/core/wmem_max,
	// although we don't know the buffer size used by the GCP load balancer, so we can only take a guess.
	minBufferBehindSize = 10 * 1e6

	defaultByteBufferSize = 65535
)

type Session struct {
	outboundConn      net.Conn
	bytesWritten      int64
	writeBuffer       []byte
	clientBoundBuffer *Buffer

	wg              *sync.WaitGroup
	lastActivity    time.Time
	sessionWorkerID uuid.UUID
	cond            *sync.Cond
	logger          *zap.SugaredLogger
	dead            bool
}

func (s *Session) IsValid(workerID uuid.UUID) bool {
	return s.sessionWorkerID == workerID && !s.dead
}

type Server struct {
	dst      string
	upgrader *websocket.Upgrader
	logger   *zap.SugaredLogger
	streams  map[uuid.UUID]*Stream
	mu       sync.Mutex
}

type ErrorCode int64

const (
	CodeNoError = ErrorCode(iota)
	CodeSessionNotFound
	CodeCannotResume
	CodeDialError
)

type Handshake struct {
	ID         uuid.UUID
	ResumeFrom int64
	ErrorCode  ErrorCode
}

func NewServer(dst string, logger *zap.SugaredLogger) *Server {
	//go func() {
	//	sigc := make(chan os.Signal, 1)
	//	signal.Notify(sigc, syscall.SIGUSR1)
	//	go func() {
	//		for range sigc {
	//			log.Println("received SIGUSR1, dropping connections...")
	//			mu.Lock()
	//			for key, conn := range connRegistry {
	//				conn.Close()
	//				delete(connRegistry, key)
	//			}
	//			mu.Unlock()
	//		}
	//	}()
	//}()

	return &Server{
		dst: dst,
		upgrader: &websocket.Upgrader{
			ReadBufferSize:  256000,
			WriteBufferSize: 256000,
		},
		streams: make(map[uuid.UUID]*Stream),
		logger:  logger,
	}
}

//var connRegistry = map[uuid.UUID]*websocket.Conn{}
//var mu sync.Mutex

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		panic(err)
	}

	//mu.Lock()
	//connRegistry[uuid.Must(uuid.NewV4())] = conn
	//mu.Unlock()

	c := NewWebsocketConn(conn)

	session, err := smux.Server(c, DefaultSmuxConfig())
	if err != nil {
		panic(err)
	}

	for {
		stream, err := session.AcceptStream()
		if err != nil {
			return
		}

		go s.handleStream(r.RemoteAddr, stream)
	}
}

func (s *Server) handleStream(remoteAddr string, conn *smux.Stream) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Errorf("handleStream: recovered panic: %+v", errors.Newf("%+v", r))
		}
	}()

	defer conn.Close()

	waitingForHandshake.report()

	// perform handshake
	var receivedHandshake Handshake
	err := binary.Read(conn, binary.LittleEndian, &receivedHandshake)
	if err != nil {
		s.logger.Warnf("error reading handshake: %+v", err)
		return
	}

	flow := s.handleHandshake(remoteAddr, conn, &receivedHandshake)
	waitingForHandshake.release()
	if flow == nil {
		return
	}

	flow.logger.Infof("flow started")
	err = flow.Resume(conn, receivedHandshake.ResumeFrom)
	if err != nil {
		s.logger.Warnf("error resuming flow: %+v", err)
		return
	}
}

func (s *Server) handleHandshake(remoteAddr string, conn *smux.Stream, receivedHandshake *Handshake) *Flow {
	if !receivedHandshake.ID.IsNil() {
		// resume an existing session
		var flow *Flow

		func() {
			s.mu.Lock()
			defer s.mu.Unlock()

			var found bool
			stream, found := s.streams[receivedHandshake.ID]
			if !found {
				s.logger.Warnf("received handshake for non-existent session: %s", receivedHandshake.ID)
				// session does not exist
				err := binary.Write(conn, binary.LittleEndian, Handshake{
					ID:        uuid.Nil,
					ErrorCode: CodeSessionNotFound,
				})
				if err != nil {
					s.logger.Warnf("error writing handshake: %+v", err)
					return
				}
				return
			}

			var resumeFrom int64
			var err error
			flow, resumeFrom, err = stream.NewFlow()
			if err != nil {
				binary.Write(conn, binary.LittleEndian, Handshake{
					ID:        receivedHandshake.ID,
					ErrorCode: CodeCannotResume,
				})
				s.logger.Warnf("error creating new flow: %+v", err)
				return
			}

			err = binary.Write(conn, binary.LittleEndian, Handshake{
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

	// start a new stream
	out, err := net.Dial("tcp", s.dst)
	if err != nil {
		s.logger.Warnf("failed to dial %q: %+v", s.dst, err)
		binary.Write(conn, binary.LittleEndian, Handshake{
			ErrorCode: CodeDialError,
		})
		return nil
	}

	id := uuid.Must(uuid.NewV4())

	stream := NewStream(maxBufferSize, minBufferBehindSize, s.logger.With(
		zap.String("stream", id.String()),
		zap.String("remote_ip", remoteAddr),
	))

	s.mu.Lock()
	s.streams[id] = stream
	s.mu.Unlock()

	stream.logger.Infof("opened new stream")
	err = binary.Write(conn, binary.LittleEndian, Handshake{
		ID:         id,
		ResumeFrom: 0,
	})
	if err != nil {
		stream.logger.Warnf("error writing handshake: %+v", err)
		stream.Close()
		out.Close()
		return nil
	}

	go func() {
		defer stream.Close()
		defer out.Close()
		_, err = io.Copy(stream, out)
		if err != nil {
			stream.logger.Warnf("error copying from outbound: %+v", err)
		}
	}()

	// start the always running goroutine that copies from the server to the buffer.
	go func() {
		defer stream.Close()
		defer out.Close()
		_, err = io.Copy(out, stream)
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

	flow.cond.L.Lock()
	ReportState(State{
		LastFlowID:   flow.id.String(),
		HasFlow:      true,
		HasFlowState: true,

		BytesRead:         flow.bytesRead,
		BytesInReadBuffer: int64(len(flow.readBuffer)),

		WriteBufferHead: flow.writeBuffer.bufferHead,
		WriteBufferPos:  flow.writeBuffer.readerPos,
		WriteBufferSize: int64(len(flow.writeBuffer.buffer)),
	})
	flow.cond.L.Unlock()

	return flow
}
