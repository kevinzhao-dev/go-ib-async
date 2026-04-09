package ibgo

import (
	"errors"
	"fmt"
)

var (
	// ErrNotConnected indicates no active connection to TWS/Gateway.
	ErrNotConnected = errors.New("ibgo: not connected")

	// ErrDisconnected indicates the connection was lost.
	ErrDisconnected = errors.New("ibgo: connection lost")

	// ErrTimeout indicates a request timed out.
	ErrTimeout = errors.New("ibgo: request timeout")
)

// RequestError represents an error returned by the IB API.
type RequestError struct {
	ReqID   int64
	Code    int
	Message string
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("ibgo: API error %d (reqId %d): %s", e.Code, e.ReqID, e.Message)
}
