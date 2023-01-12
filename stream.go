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

// Stream is a resumable stream.
// An active "instance" of a stream is a flow.
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

	remoteAddr net.Addr
	localAddr  net.Addr
}

func NewStream(maxTotal, minBehind int64, logger *zap.SugaredLogger) *Stream {
	return &Stream{
		writeBuffer:  NewBuffer(maxTotal, minBehind, logger),
		wg:           new(sync.WaitGroup),
		cond:         sync.NewCond(new(sync.Mutex)),
		logger:       logger,
		lastActivity: time.Now(),
	}
}

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

func (f *Flow) IsValid() bool {
	return f.flowID == f.id && f.err == nil
}

func (f *Flow) Close() {
	panic("flows can't be closed and don't need to be closed, did you mean to close the stream instead?")
}

func (s *Stream) Read(p []byte) (n int, err error) {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	// we only return an error if there is no data to read and the stream is closed.
	if len(s.readBuffer) == 0 && s.err != nil {
		return 0, s.err
	}

	for len(s.readBuffer) == 0 && s.err == nil {
		waitingForStreamRead.report()
		s.cond.Wait()
	}
	waitingForStreamRead.release()

	s.logger.Debugf("read %d bytes from stream %p", len(s.readBuffer), s)

	n = copy(p, s.readBuffer)
	s.readBuffer = s.readBuffer[n:]
	s.bytesRead += int64(n)
	s.cond.Broadcast()

	ReportState(State{
		BytesRead:         s.bytesRead,
		BytesInReadBuffer: int64(len(s.readBuffer)),
	})

	if n == 0 && s.err != nil {
		return 0, s.err
	}

	return n, nil
}

func (s *Stream) Write(p []byte) (n int, err error) {
	return s.writeBuffer.Write(p)
}

func (s *Stream) Close() error {
	s.logger.Debugf("closing stream %p", s)
	s.cond.L.Lock()
	s.err = io.EOF
	s.cond.L.Unlock()
	s.cond.Broadcast()
	s.writeBuffer.Close()
	return nil
}

func (s *Stream) IsClosed() bool {
	s.cond.L.Lock()
	defer s.cond.L.Unlock()

	return s.err != nil
}

// Resume attempts to resume the flow's stream with the given connection and resumeFrom value from the handshake.
// Resume will automatically close the unreliable connection when complete.
func (f *Flow) Resume(unreliable net.Conn, resumeFrom int64) error {
	defer unreliable.Close()
	defer func() {
		f.logger.Debugf("closing connection in flow %q, stream: %p", f.id, f.Stream)
	}()

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
		defer f.writeBuffer.TakeReader(uuid.Nil)

		buf := make([]byte, defaultByteBufferSize)

		for {
			waitingForUnreliableRead.report()
			n, err := unreliable.Read(buf)
			waitingForUnreliableRead.release()
			if err != nil {
				f.logger.Infof("error reading from client: %v", err)
				return
			}

			f.cond.L.Lock()

			for len(f.readBuffer) > 0 && f.IsValid() {
				waitingForStreamWrite.report()
				f.cond.Wait()
			}
			waitingForStreamWrite.release()

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
		defer f.writeBuffer.TakeReader(uuid.Nil)

		buf := make([]byte, defaultByteBufferSize)

		for {
			f.logger.Debugf("write to client: reading")

			// reader will automatically preempt with an error, so we don't need to do the f.IsValid checking.\
			n, err := reader.Read(buf)
			if err != nil {
				f.logger.Infof("error reading from buffer: %v", err)
				return
			}

			f.logger.Debugf("write to client: read complete")

			i := 0

			for i < n {
				waitingForUnreliableWrite.report()
				bytesWritten, err := unreliable.Write(buf[i:n])
				waitingForUnreliableWrite.release()
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

func (s *Stream) RemoteAddr() net.Addr {
	return s.remoteAddr
}

func (s *Stream) LocalAddr() net.Addr {
	return s.localAddr
}

func (s *Stream) SetWriteDeadline(t time.Time) error {
	return nil
}

func (s *Stream) SetReadDeadline(t time.Time) error {
	return nil
}

func (s *Stream) SetDeadline(t time.Time) error {
	return nil
}
