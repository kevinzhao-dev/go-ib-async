package ibgo

import (
	"errors"
	"strings"

	"github.com/kevinzhao-dev/go-ib-async/protocol"
)

// HandshakeError is the reason TWS/Gateway gave for refusing a connection
// during the API handshake. Connect returns it wrapped; use errors.As.
type HandshakeError = protocol.HandshakeError

var (
	// ErrClientIDInUse is matched (errors.Is) by connection errors caused by
	// TWS refusing the client id because another API client uses it (326).
	ErrClientIDInUse = protocol.ErrClientIDInUse

	// ErrNotConnected indicates no active connection to TWS/Gateway.
	ErrNotConnected = errors.New("ibgo: not connected")

	// ErrDisconnected indicates the connection was lost.
	ErrDisconnected = errors.New("ibgo: connection lost")

	// ErrTimeout indicates a request timed out.
	ErrTimeout = errors.New("ibgo: request timeout")

	// ErrRequestCancelled indicates a pending request was abandoned.
	ErrRequestCancelled = errors.New("ibgo: request cancelled")

	// ErrOrderDone is returned when trying to modify a filled/cancelled order.
	ErrOrderDone = errors.New("ibgo: order is already done")

	// ErrOrderIdentity means the supplied order does not identify the order
	// that this connection could modify or cancel.
	ErrOrderIdentity = errors.New("ibgo: order identity does not match this client and order ID")

	// ErrUseWhatIfOrder is returned by PlaceOrder for orders with WhatIf set.
	ErrUseWhatIfOrder = errors.New("ibgo: order has WhatIf set; use WhatIfOrder")

	// ErrReadOnlyAPI is matched (errors.Is) by errors caused by the
	// "Read-Only API" setting of TWS/Gateway. In that mode TWS rejects
	// orders, reqOpenOrders and reqCompletedOrders; reqAllOpenOrders,
	// executions, positions, account and market data keep working.
	ErrReadOnlyAPI = errors.New("ibgo: TWS/Gateway API is in read-only mode")
)

// RequestError is the error returned by a failed request. It is the same
// type that ErrorEvent delivers, so errors.As(err, &*IBError) works for both.
type RequestError = IBError

// IsReadOnly reports whether the error was caused by the Read-Only API
// setting of TWS/Gateway.
func (e *IBError) IsReadOnly() bool {
	return e.Code == 321 && strings.Contains(strings.ToLower(e.Message), "read-only")
}

// Unwrap lets errors.Is(err, ErrReadOnlyAPI) detect read-only rejections.
func (e *IBError) Unwrap() error {
	if e.IsReadOnly() {
		return ErrReadOnlyAPI
	}
	return nil
}
