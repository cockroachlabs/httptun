package httptun

import (
	"io"
	"sync"

	"github.com/cockroachdb/errors"
	"github.com/gofrs/uuid"
	"go.uber.org/zap"
)

// The steady state of the Buffer looks like this:
//
//                           maxTotalBuffer
// <--------------------------------------------------------------->
//
//   minBehindBuffer
// <----------------->
//
// bufferHead      readerPos
// v                  v
// [BUFFER BUFFER BUFFER BUFFER BUFFER BUFFER BUFFER BUFFER]
//

func NewBuffer(maxTotal, minBehind int64, logger *zap.SugaredLogger) *Buffer {
	return &Buffer{
		buffer:          make([]byte, 0, maxTotal),
		bufferHead:      0,
		readerPos:       0,
		maxTotalBuffer:  maxTotal,
		minBehindBuffer: minBehind,
		cond:            sync.NewCond(new(sync.Mutex)),

		err:             nil,
		hasActiveReader: false,
		logger:          logger,
	}
}

type Buffer struct {
	buffer []byte

	bufferHead int64 // the absolute position of the head of the floating buffer.
	readerPos  int64 // the absolute position in the floating buffer reads are being performed at.

	maxTotalBuffer  int64
	minBehindBuffer int64

	cond *sync.Cond

	err             error
	hasActiveReader bool
	readerID        uuid.UUID
	logger          *zap.SugaredLogger
}

type BufferReader struct {
	*Buffer
	readerID uuid.UUID
}

func (b *BufferReader) Read(p []byte) (n int, err error) {
	return b.Buffer.Read(b.readerID, p)
}

func (b *Buffer) tail() int64 {
	return b.bufferHead + int64(len(b.buffer))
}

func (b *Buffer) relativeReaderPos() int64 {
	return b.readerPos - b.bufferHead
}

func (b *Buffer) SetReaderPos(pos int64) error {
	b.cond.L.Lock()
	defer b.cond.L.Unlock()

	if pos < b.bufferHead {
		return errors.New("buffer: read out of range")
	}

	b.readerPos = pos
	b.cond.Broadcast()

	return nil
}

func (b *Buffer) WriterPos() int64 {
	return b.tail()
}

// Preemption is a mechanism that makes Read calls of buffers return immediately, and notify the caller they
// have been preempted. The caller should immediately exit.
var ErrPreempted = errors.New("buffer: preempted")

func (b *Buffer) Read(readerID uuid.UUID, p []byte) (n int, err error) {
	b.logger.Debugf("read called in %p", b)

	b.cond.L.Lock()
	defer b.cond.L.Unlock()

	if b.readerID != readerID {
		return 0, ErrPreempted
	}

	if b.hasActiveReader {
		panic("simultaneous reader detected")
	}

	b.hasActiveReader = true
	defer func() {
		b.hasActiveReader = false
		b.cond.Broadcast()
	}()

	// If the reader wants to read data already evicted of the buffer, it can't.
	if b.readerPos < b.bufferHead {
		return 0, errors.New("buffer: read out of range")
	}

	for b.tail()-b.readerPos == 0 && b.err == nil && b.readerID == readerID {
		b.logger.Debugf("tail and reader is %d, %d in %p", b.tail(), b.readerPos, b)
		waitingForBufferRead.report()
		b.cond.Wait()
	}
	waitingForBufferRead.release()

	b.logger.Debugf("wait completed")

	if b.readerID != readerID {
		return 0, ErrPreempted
	}

	n = copy(p, b.buffer[b.relativeReaderPos():])
	b.readerPos += int64(n)

	if n == 0 && b.err != nil {
		return 0, b.err
	}

	// Calculate the furthest back we can move the buffer head.
	maxBufferHead := b.readerPos - b.minBehindBuffer
	if b.bufferHead < maxBufferHead {
		// If we can move it back, move it.
		delta := maxBufferHead - b.bufferHead
		b.bufferHead = maxBufferHead
		b.buffer = b.buffer[delta:]
	}

	b.logger.Debugf("buffer read %d bytes in %p: %q", n, b, string(p[:n]))
	ReportState(State{
		WriteBufferHead: b.bufferHead,
		WriteBufferPos:  b.readerPos,
		WriteBufferSize: int64(len(b.buffer)),
	})

	return n, nil
}

func (b *Buffer) Write(p []byte) (n int, err error) {
	b.logger.Debugf("write called in %p", b)

	b.cond.L.Lock()
	defer b.cond.L.Unlock()

	for int64(len(b.buffer)) >= b.maxTotalBuffer && b.err == nil {
		waitingForBufferWrite.report()
		b.cond.Wait()
	}
	waitingForBufferWrite.release()

	if b.err != nil {
		return 0, err
	}

	availableCapacity := b.maxTotalBuffer - int64(len(b.buffer))
	if int64(len(p)) < availableCapacity {
		availableCapacity = int64(len(p))
	}

	b.buffer = append(b.buffer, p[:availableCapacity]...)

	b.cond.Broadcast()

	b.logger.Debugf("buffer wrote %d bytes in %p", availableCapacity, b)

	ReportState(State{
		WriteBufferHead: b.bufferHead,
		WriteBufferPos:  b.readerPos,
		WriteBufferSize: int64(len(b.buffer)),
	})

	return int(availableCapacity), nil
}

func (b *Buffer) Close() {
	b.CloseWithErr(io.EOF)
}

func (b *Buffer) CloseWithErr(err error) {
	b.cond.L.Lock()
	defer b.cond.L.Unlock()

	if b.err != nil {
		return
	}

	b.logger.Debugf("buffer closed: %p", b)

	b.err = err
	b.cond.Broadcast()
}

func (b *Buffer) TakeReader(readerID uuid.UUID) *BufferReader {
	b.cond.L.Lock()
	defer b.cond.L.Unlock()

	if b.readerID.IsNil() {
		ReportState(State{
			HasFlow:      false,
			HasFlowState: true,
		})
	}

	b.readerID = readerID
	b.cond.Broadcast()
	b.logger.Debugf("preempted reader with id: %s", readerID)

	for b.hasActiveReader {
		b.cond.Wait()
	}

	return &BufferReader{
		Buffer:   b,
		readerID: readerID,
	}
}
