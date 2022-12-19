package httptun

import (
	"io"
	"net"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/xtaci/smux"
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

		go s.handleStream(stream)
	}
}

func (s *Server) handleStream(conn *smux.Stream) {
	defer conn.Close()

	out, err := net.Dial("tcp", s.dst)
	if err != nil {
		s.logger.Warnf("failed to dial %q: %+v", s.dst, err)
		return
	}

	defer out.Close()

	go func() {
		defer conn.Close()
		defer out.Close()
		_, _ = io.Copy(conn, out)
	}()

	_, _ = io.Copy(out, conn)
}
