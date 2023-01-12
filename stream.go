package httptun

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/gofrs/uuid"
	"go.uber.org/zap"
)

// Stream is a resumable stream, it represents a logical, persistent (extra-reliable) connection.
type Stream struct {
	bytesRead   int64
	readBuffer  []byte
	writeBuffer *Buffer

	wg           *sync.WaitGroup
	lastActivity time.Time

	flowID uuid.UUID
	cond   *sync.Cond

	logger *zap.SugaredLogger
	err    error
}

// NewStream creates a new stream.
func NewStream(maxTotal, minBehind int64, logger *zap.SugaredLogger) *Stream {
	return &Stream{
		writeBuffer:  NewBuffer(maxTotal, minBehind, logger),
		wg:           new(sync.WaitGroup),
		cond:         sync.NewCond(new(sync.Mutex)),
		logger:       logger,
		lastActivity: time.Now(),
	}
}

// Flow is an active "instance" of a stream, which represents an unreliable connection such as a WebSocket
// connection.
type Flow struct {
	*Stream
	id uuid.UUID
}

// NewFlow prepares the stream for resumption with a new flow. It preempts any existing flows
// and returns the ResumeFrom value of the stream that should be given to the other side in a handshake.
func (s *Stream) NewFlow() (*Flow, int64, error) {
	flowID := uuid.Must(uuid.NewV4())

	s.cond.L.Lock()

	if s.err != nil {
		s.cond.L.Unlock()
		return nil, 0, s.err
	}

	resumeFrom := s.bytesRead + int64(len(s.readBuffer))
	s.flowID = flowID
	s.lastActivity = time.Now()

	s.cond.L.Unlock()
	s.cond.Broadcast()

	// TakeReader preempts any in-flight calls to the stream and blocks new calls to the stream that don't match
	// the given flow ID.
	s.writeBuffer.TakeReader(flowID)

	return &Flow{
		Stream: s,
		id:     flowID,
	}, resumeFrom, nil
}

// IsValid returns true if the flow is still valid (i.e. the stream's current flow ID is this flow's, and there is
// no underlying error reported.
func (f *Flow) IsValid() bool {
	return f.flowID == f.id && f.err == nil
}

// Close causes a panic to prevent misuse. Closing the flow is prohibited but *Flow wraps a *Stream, so the
// Close method is implemented to prevent accidental misuse of the (*Stream).Close method.
func (f *Flow) Close() {
	panic("flows can't be closed and don't need to be closed, did you mean to close the stream instead?")
}

// Read implements io.Reader. Stream's Read method is behind a buffer and is interruption-free when flows are
// interrupted.
func (s *Stream) Read(p []byte) (n int, err error) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	// we only return an error if there is no data to read and the stream is closed.
	if len(s.readBuffer) == 0 && s.err != nil {
		return 0, s.err
	}

	for len(s.readBuffer) == 0 && s.err == nil {
		s.cond.Wait()
	}

	n = copy(p, s.readBuffer)
	s.readBuffer = s.readBuffer[n:]
	s.bytesRead += int64(n)
	s.cond.Broadcast()

	if n == 0 && s.err != nil {
		return 0, s.err
	}

	return n, nil
}

// Write implements io.Writer. Stream's Write method writes into the *Buffer (see the godoc on *Buffer for more details)
// and is interruption-free when flows are interrupted.
func (s *Stream) Write(p []byte) (n int, err error) {
	return s.writeBuffer.Write(p)
}

// Close closes the stream, immediately preempting all Read, Write calls and flows. It is safe to call Close multiple
// times.
func (s *Stream) Close() error {
	s.logger.Debugf("closing stream %p", s)
	s.cond.L.Lock()
	s.err = io.EOF
	s.cond.L.Unlock()
	s.cond.Broadcast()
	s.writeBuffer.Close()
	return nil
}

// IsClosed returns true if the stream is closed.
func (s *Stream) IsClosed() bool {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	return s.err != nil
}

// Resume attempts to resume the flow's stream with the given "unreliable" connection (typically a WebSocket
// connection) and resumeFrom value from the handshake. Resume will automatically close the unreliable connection
// when complete.
func (f *Flow) Resume(unreliable net.Conn, resumeFrom int64) error {
	defer unreliable.Close()
	defer func() {
		f.logger.Debugf("closing connection in flow %q, stream: %p", f.id, f.Stream)
	}()

	f.logger.Debugf("resuming flow from %d", resumeFrom)

	reader := f.writeBuffer.TakeReader(f.id)

	err := reader.SetReaderPos(resumeFrom)
	if err != nil {
		return errors.Newf("cannot resume flow from position %v, please increase minBehindBuffer", resumeFrom)
	}

	f.wg.Add(1)
	// read from client
	go func() {
		defer f.wg.Done()
		defer unreliable.Close()
		// We call TakeReader to preempt the buffer to make any in-flight read calls return immediately.
		defer f.writeBuffer.TakeReader(uuid.Nil)

		buf := make([]byte, defaultByteBufferSize)

		for {
			n, err := unreliable.Read(buf)
			if err != nil {
				f.logger.Infof("error reading from client: %v", err)
				return
			}

			f.logger.Debugf("read %d bytes from the unreliable connection", n)

			f.cond.L.Lock()

			for len(f.readBuffer) > 0 && f.IsValid() {
				f.cond.Wait()
			}

			if !f.IsValid() {
				f.cond.L.Unlock()
				f.logger.Debugf("read from unreliable: flow %v is no longer valid, exiting", f.id)
				return
			}

			output := make([]byte, n)
			copy(output, buf[:n])
			f.readBuffer = output

			f.cond.L.Unlock()
			f.cond.Broadcast()
		}
	}()

	f.wg.Add(1)
	// write to client
	go func() {
		defer f.wg.Done()
		defer unreliable.Close()
		// We call TakeReader to preempt the buffer to make any in-flight read calls return immediately.
		defer f.writeBuffer.TakeReader(uuid.Nil)

		buf := make([]byte, defaultByteBufferSize)

		for {
			// reader will automatically preempt with an error, so we don't need to do the f.IsValid checking.\
			n, err := reader.Read(buf)
			if err != nil {
				f.logger.Infof("error reading from buffer: %v", err)
				return
			}

			i := 0

			f.logger.Debugf("writing %d bytes to the unreliable connection", n)

			for i < n {
				bytesWritten, err := unreliable.Write(buf[i:n])
				if err != nil {
					f.logger.Infof("error writing to client: %v", err)
					return
				}

				i += bytesWritten
			}
		}
	}()

	f.wg.Wait()

	return nil
}

type streamAddr struct {
	address string
}

func (a streamAddr) Network() string {
	return "httptun"
}

func (a streamAddr) Address() string {
	return a.address
}

func (a streamAddr) String() string {
	return "httptun_dummy_addr://" + a.address
}

// RemoteAddr implements net.Conn. It returns a dummy but valid net.Addr value.
func (s *Stream) RemoteAddr() net.Addr {
	return streamAddr{
		address: "remote",
	}
}

// LocalAddr implements net.Conn. It returns a dummy but valid net.Addr value.
func (s *Stream) LocalAddr() net.Addr {
	return streamAddr{
		address: "local",
	}
}

// SetWriteDeadline implements net.Conn. It is an unimplemented no-op.
func (s *Stream) SetWriteDeadline(t time.Time) error {
	return nil
}

// SetReadDeadline implements net.Conn. It is an unimplemented no-op.
func (s *Stream) SetReadDeadline(t time.Time) error {
	return nil
}

// SetDeadline implements net.Conn. It is an unimplemented no-op.
func (s *Stream) SetDeadline(t time.Time) error {
	return nil
}
