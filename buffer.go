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
	"io"
	"sync"

	"github.com/cockroachdb/errors"
	"github.com/gofrs/uuid"
	"go.uber.org/zap"
)

// Buffer provides the flexibility to recover from an interrupted connection. Each part of a
// working connection is referred to as a "flow". When a flow is resumed with (*Flow).Resume, a handshake is
// performed that adjusts the (*Buffer).readerPos on the other side to the appropriate position to continue the TCP
// connection based on the number of bytes last successfully received. Thus the job of the buffer is to provide
// enough backwards space for the readerPos to jump back to. This is represented by (*Buffer).minBehindBuffer. The
// forward space of the buffer isn't technically necessary, it's there to improve throughput and to keep connections
// from stalling when a connection is interrupted.
//
// The journey of a "packet" looks like this:
//
// 1. The underlying connection is read by the caller, or inside server.go
// 2. (*Buffer).Write() is called, which appends the data to the buffer.
// 3. (*Buffer).Read() is called, which reads from the buffer from (*Buffer).readerPos.
// 4. (net.Conn).Write() is called on the "unreliable" connection.
// --- You're now on the other side ---
// 5. (net.Conn).Read() is called on the "unreliable" connection from within (*Flow).Resume.
// 6. Inside (*Flow).Resume, the read data is written to a small, simple buffer (*Stream).writeBuffer.
// 7. (*Stream).Read() is called, which reads from (*Stream).writeBuffer and provides the "reliable" connection.
//
// The steady state of the Buffer looks like this:
//
//	                          maxTotalBuffer (combined buffer)
//	                  <----------------------------------------------->
//
//	minBehindBuffer (buffer for recovering a lost connection)
//	                  <----------------->
//
//	                  bufferHead      readerPos (position in buffer consumer is reading from)
//	                  v                  v
//
// (*Buffer).buffer = []byte{.................................................}
type Buffer struct {
	buffer []byte

	bufferHead int64 // the absolute position of the head of the floating buffer.
	readerPos  int64 // the absolute position in the floating buffer reads are being performed at.

	maxTotalBuffer  int64
	minBehindBuffer int64

	cond *sync.Cond

	err             error
	hasActiveReader bool
	flowID          uuid.UUID
	logger          *zap.SugaredLogger

	bytesWritten int64
}

// NewBuffer constructs a new buffer with the given size and logger. See Buffer for more information.
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

// BufferReader is a reader that reads from a Buffer. It wraps Buffer with a flow ID that is used to manage the flows
// being read from the buffer.
type BufferReader struct {
	*Buffer
	flowID uuid.UUID
}

// Read implements io.Reader. It will block until there is data to read. ErrPreempted is returned if the flow is
// preempted by another flow.
func (b *BufferReader) Read(p []byte) (n int, err error) {
	return b.Buffer.Read(b.flowID, p)
}

func (b *Buffer) tail() int64 {
	return b.bufferHead + int64(len(b.buffer))
}

func (b *Buffer) relativeReaderPos() int64 {
	return b.readerPos - b.bufferHead
}

// SetReaderPos sets the reader position of the buffer, it used during the handshake of httptun to resume a TCP
// connection.
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

// WriterPos returns the current position of the writer in the buffer.
func (b *Buffer) WriterPos() int64 {
	return b.tail()
}

// ErrPreempted (preemption) is a mechanism that makes Read calls of buffers return immediately, and notify the caller
// they have been preempted. The caller should immediately exit as it indicates that their flow is no longer valid.
var ErrPreempted = errors.New("buffer: preempted")

// Read reads from the buffer. It will block until there is data to read. ErrPreempted is returned if the flow is
// preempted by another flow.
func (b *Buffer) Read(flowID uuid.UUID, p []byte) (n int, err error) {
	b.cond.L.Lock()
	defer b.cond.L.Unlock()

	if b.flowID != flowID {
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

	for b.tail()-b.readerPos == 0 && b.err == nil && b.flowID == flowID {
		b.cond.Wait()
	}

	if b.flowID != flowID {
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

	return n, nil
}

// Write writes to the buffer. It will block until there is space to write.
func (b *Buffer) Write(p []byte) (n int, err error) {
	b.cond.L.Lock()
	defer b.cond.L.Unlock()

	for int64(len(b.buffer)) >= b.maxTotalBuffer && b.err == nil {
		b.cond.Wait()
	}

	if b.err != nil {
		return 0, err
	}

	availableCapacity := b.maxTotalBuffer - int64(len(b.buffer))
	if int64(len(p)) < availableCapacity {
		availableCapacity = int64(len(p))
	}

	b.buffer = append(b.buffer, p[:availableCapacity]...)
	b.bytesWritten += availableCapacity

	b.cond.Broadcast()

	return int(availableCapacity), nil
}

// Close closes the buffer. It will cause all readers and writers to return with an error.
func (b *Buffer) Close() {
	b.CloseWithErr(io.EOF)
}

// CloseWithErr closes the buffer with a specific error. It will cause all readers and writers to return with the
// specified error.
func (b *Buffer) CloseWithErr(err error) {
	b.cond.L.Lock()
	defer b.cond.L.Unlock()

	if b.err != nil {
		return
	}

	b.err = err
	b.cond.Broadcast()
}

// TakeReader returns a wrapped io.Reader from the *Buffer that is tied to a specific flow ID.
func (b *Buffer) TakeReader(flowID uuid.UUID) *BufferReader {
	b.cond.L.Lock()
	defer b.cond.L.Unlock()

	b.flowID = flowID
	b.cond.Broadcast()

	for b.hasActiveReader {
		b.cond.Wait()
	}

	return &BufferReader{
		Buffer: b,
		flowID: flowID,
	}
}
