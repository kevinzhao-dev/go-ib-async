// Package state manages the internal state of the IB connection.
package state

import "sync"

// Result represents the outcome of a request-response cycle.
type Result struct {
	Value interface{}
	Err   error
}

// RequestMap tracks pending request-response pairs using channels.
type RequestMap struct {
	mu      sync.Mutex
	pending map[int64]chan Result
}

// NewRequestMap creates a new RequestMap.
func NewRequestMap() *RequestMap {
	return &RequestMap{
		pending: make(map[int64]chan Result),
	}
}

// Start registers a new pending request and returns a channel to wait on.
func (rm *RequestMap) Start(reqID int64) <-chan Result {
	ch := make(chan Result, 1) // buffered: avoid blocking the reader goroutine
	rm.mu.Lock()
	rm.pending[reqID] = ch
	rm.mu.Unlock()
	return ch
}

// Complete resolves a pending request with a result.
func (rm *RequestMap) Complete(reqID int64, result Result) {
	rm.mu.Lock()
	ch, ok := rm.pending[reqID]
	if ok {
		delete(rm.pending, reqID)
	}
	rm.mu.Unlock()
	if ok {
		ch <- result
		close(ch)
	}
}

// Cancel removes a pending request without sending a result.
func (rm *RequestMap) Cancel(reqID int64) {
	rm.mu.Lock()
	ch, ok := rm.pending[reqID]
	if ok {
		delete(rm.pending, reqID)
		close(ch)
	}
	rm.mu.Unlock()
}

// Has returns true if a request is pending for the given ID.
func (rm *RequestMap) Has(reqID int64) bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	_, ok := rm.pending[reqID]
	return ok
}

// Len returns the number of pending requests.
func (rm *RequestMap) Len() int {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return len(rm.pending)
}

// DrainAll cancels all pending requests with the given error.
func (rm *RequestMap) DrainAll(err error) {
	rm.mu.Lock()
	pending := rm.pending
	rm.pending = make(map[int64]chan Result)
	rm.mu.Unlock()

	for _, ch := range pending {
		ch <- Result{Err: err}
		close(ch)
	}
}
