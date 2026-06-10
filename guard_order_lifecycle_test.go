package ibgo

// Order-lifecycle guardrail tests for the error path.
//
// Expected behavior is transcribed from ib_async wrapper.py Wrapper.error():
//
//	warningCodes = frozenset({105, 110, 165, 321, 329, 399, 404, 434, 492, 10167})
//	isWarning = errorCode in warningCodes or 2100 <= errorCode < 2200
//	...
//	if isWarning and trade:
//	    status = trade.orderStatus.status = OrderStatus.ValidationError
//	    trade.log.append(TradeLogEntry(now, status, msg, errorCode))
//	    ib.orderStatusEvent.emit(trade); trade.statusEvent.emit(trade)
//	elif not isWarning:
//	    if isRequest: endReq with error
//	    elif trade and not trade.isDone():
//	        status = trade.orderStatus.status = OrderStatus.Cancelled
//	        trade.log.append(TradeLogEntry(now, status, msg, errorCode))
//	        ib.orderStatusEvent.emit(trade); trade.statusEvent.emit(trade)
//	        trade.cancelledEvent.emit(trade)
//	ib.errorEvent.emit(...)   # always, warnings included
//
// The happy path (orderStatus transitions) was already covered; the error
// path was not, which is how "rejected orders stay PendingSubmit forever"
// shipped.

import (
	"testing"

	"github.com/kevinzhao-dev/go-ib-async/order"
)

func errMsgFields(reqID, code, msg string) []string {
	// msgId, version, reqId, errorCode, errorString, advancedOrderRejectJson (srv >= 166)
	return []string{"4", "2", reqID, code, msg, ""}
}

// A rejected order (non-warning error addressed to its orderId) must be
// cancelled: status, log, events, and removal from OpenTrades.
func TestGuardOrderRejectionCancelsTrade(t *testing.T) {
	ib := newTestIB(178)
	trade := seedTrade(ib, 0, 7, 0, order.StatusPendingSubmit) // clientID 0 == ib.client.ClientID

	cancelled := 0
	trade.CancelledEvent.Subscribe(func(*order.Trade) { cancelled++ })
	statusEvents := 0
	ib.OrderStatusEvent.Subscribe(func(*order.Trade) { statusEvents++ })

	r := dispatchWire(t, ib, errMsgFields("7", "201", "Order rejected - reason:insufficient funds"))
	assertFullyConsumed(t, r)

	if trade.OrderStatus.Status != order.StatusCancelled {
		t.Fatalf("status = %q, want Cancelled — rejected order must not stay pending", trade.OrderStatus.Status)
	}
	if !trade.IsDone() {
		t.Fatal("rejected trade should be done")
	}
	if cancelled != 1 {
		t.Fatalf("CancelledEvent fired %d times, want 1", cancelled)
	}
	if statusEvents != 1 {
		t.Fatalf("OrderStatusEvent fired %d times, want 1", statusEvents)
	}

	foundLog := false
	for _, e := range trade.Log {
		if e.ErrorCode == 201 && e.Status == order.StatusCancelled {
			foundLog = true
		}
	}
	if !foundLog {
		t.Fatalf("trade log missing rejection entry, log = %+v", trade.Log)
	}

	if open := ib.OpenTrades(); len(open) != 0 {
		t.Fatalf("OpenTrades should be empty after rejection, got %d", len(open))
	}
}

// A warning code (e.g. 399 "order message error") addressed to an order must
// NOT cancel it — the order is still live at the broker. ib_async marks it
// ValidationError and logs.
func TestGuardOrderWarningKeepsTradeAlive(t *testing.T) {
	ib := newTestIB(178)
	trade := seedTrade(ib, 0, 7, 0, order.StatusSubmitted)

	cancelled := 0
	trade.CancelledEvent.Subscribe(func(*order.Trade) { cancelled++ })

	r := dispatchWire(t, ib, errMsgFields("7", "399", "Order Message: Warning: your order will not be placed at the exchange until 2026-06-10 09:30:00 US/Eastern"))
	assertFullyConsumed(t, r)

	if cancelled != 0 {
		t.Fatal("warning must not cancel a live order")
	}
	if trade.IsDone() {
		t.Fatal("warning must not terminate the trade")
	}
	if trade.OrderStatus.Status != order.StatusValidationError {
		t.Fatalf("status = %q, want ValidationError", trade.OrderStatus.Status)
	}
	foundLog := false
	for _, e := range trade.Log {
		if e.ErrorCode == 399 {
			foundLog = true
		}
	}
	if !foundLog {
		t.Fatalf("trade log missing warning entry, log = %+v", trade.Log)
	}
}

// Informational codes (2100-2199) are warnings but must still reach
// ErrorEvent subscribers (ib_async emits errorEvent unconditionally) —
// connectivity monitoring depends on codes like 2104/2106/1100.
func TestGuardInfoCodeEmitsErrorEvent(t *testing.T) {
	ib := newTestIB(178)

	got := make(chan *IBError, 1)
	ib.ErrorEvent.Subscribe(func(e *IBError) { got <- e })

	r := dispatchWire(t, ib, errMsgFields("-1", "2104", "Market data farm connection is OK:usfarm"))
	assertFullyConsumed(t, r)

	select {
	case e := <-got:
		if e.Code != 2104 {
			t.Fatalf("code = %d, want 2104", e.Code)
		}
	default:
		t.Fatal("ErrorEvent not emitted for informational code")
	}
}

// A non-warning error addressed to a pending request must fail that request
// (locks in current behavior).
func TestGuardRequestErrorFailsPendingRequest(t *testing.T) {
	ib := newTestIB(178)
	ch := ib.state.StartReq(42, nil)

	r := dispatchWire(t, ib, errMsgFields("42", "200", "No security definition has been found"))
	assertFullyConsumed(t, r)

	select {
	case result := <-ch:
		reqErr, ok := result.Err.(*RequestError)
		if !ok {
			t.Fatalf("err = %v, want *RequestError", result.Err)
		}
		if reqErr.Code != 200 {
			t.Fatalf("code = %d, want 200", reqErr.Code)
		}
	default:
		t.Fatal("pending request not completed by error")
	}
}
