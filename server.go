package httptun

import (
	"io"
	"net"
	"net/http"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

type Server struct {
	dst      string
	upgrader *websocket.Upgrader
	logger   *zap.SugaredLogger
}

func NewServer(dst string, logger *zap.SugaredLogger) *Server {
	return &Server{
		dst: dst,
		upgrader: &websocket.Upgrader{
			ReadBufferSize:  256000,
			WriteBufferSize: 256000,
		},
		logger: logger,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		panic(err)
	}

	downstream := NewWebsocketConn(conn)
	defer downstream.Close()

	upstream, err := net.Dial("tcp", s.dst)
	if err != nil {
		s.logger.Warnf("failed to dial %q: %+v", s.dst, err)
		return
	}
	defer upstream.Close()

	go func() {
		defer downstream.Close()
		defer upstream.Close()
		_, _ = io.Copy(downstream, upstream)
	}()

	_, _ = io.Copy(upstream, downstream)
}
