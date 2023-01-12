package httptun

import (
	"sync"
	"time"
)

type State struct {
	LastFlowID   string
	HasFlow      bool
	HasFlowState bool
	SessionID    string

	BytesRead         int64
	BytesInReadBuffer int64
	BytesResumeFrom   int64

	WriteBufferHead int64
	WriteBufferPos  int64
	WriteBufferSize int64

	WaitingForBufferRead      *time.Duration
	WaitingForBufferWrite     *time.Duration
	WaitingForStreamRead      *time.Duration
	WaitingForStreamWrite     *time.Duration
	WaitingForUnreliableRead  *time.Duration
	WaitingForUnreliableWrite *time.Duration
	WaitingForHandshake       *time.Duration
}

type timeWrapper struct {
	mu sync.Mutex
	t  time.Time
}

var currentState State
var stateMu sync.Mutex
var waitingForBufferRead = new(timeWrapper)
var waitingForBufferWrite = new(timeWrapper)
var waitingForStreamRead = new(timeWrapper)
var waitingForStreamWrite = new(timeWrapper)
var waitingForUnreliableRead = new(timeWrapper)
var waitingForUnreliableWrite = new(timeWrapper)
var waitingForHandshake = new(timeWrapper)

func (t *timeWrapper) report() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.t.IsZero() {
		t.t = time.Now()
	}
}

func (t *timeWrapper) release() {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.t = time.Time{}
}

func (t *timeWrapper) Duration() *time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.t.IsZero() {
		return nil
	}

	d := time.Since(t.t)
	return &d
}

func GetState() State {
	stateMu.Lock()
	defer stateMu.Unlock()

	currentState.WaitingForBufferRead = waitingForBufferRead.Duration()
	currentState.WaitingForBufferWrite = waitingForBufferWrite.Duration()
	currentState.WaitingForStreamRead = waitingForStreamRead.Duration()
	currentState.WaitingForStreamWrite = waitingForStreamWrite.Duration()
	currentState.WaitingForUnreliableRead = waitingForUnreliableRead.Duration()
	currentState.WaitingForUnreliableWrite = waitingForUnreliableWrite.Duration()
	currentState.WaitingForHandshake = waitingForHandshake.Duration()
	currentState.BytesResumeFrom = currentState.BytesRead + currentState.BytesInReadBuffer

	return currentState
}

func ReportState(state State) {
	stateMu.Lock()
	defer stateMu.Unlock()

	if state.LastFlowID != "" {
		currentState.LastFlowID = state.LastFlowID
	}

	if state.HasFlowState {
		currentState.HasFlow = true
	}

	if state.SessionID != "" {
		currentState.SessionID = state.SessionID
	}

	if state.BytesRead > 0 {
		currentState.BytesRead = state.BytesRead
	}

	if state.BytesInReadBuffer > 0 {
		currentState.BytesInReadBuffer = state.BytesInReadBuffer
	}

	if state.WriteBufferHead > 0 {
		currentState.WriteBufferHead = state.WriteBufferHead
	}

	if state.WriteBufferPos > 0 {
		currentState.WriteBufferPos = state.WriteBufferPos
	}

	if state.WriteBufferSize > 0 {
		currentState.WriteBufferSize = state.WriteBufferSize
	}
}
