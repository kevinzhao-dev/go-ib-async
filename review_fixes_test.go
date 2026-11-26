package ibgo

// Regression tests for issues found in the code review.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/state"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/protocol"
)

func feed(ib *IB, handler func(*protocol.FieldReader), fields ...string) {
	r := protocol.NewFieldReader(fields)
	r.Skip(1) // msgId
	handler(r)
}

func liveTrade(ib *IB, orderID int64, status string) *order.Trade {
	o := order.LimitOrder("BUY", 10, 100)
	o.OrderID = orderID
	o.ClientID = ib.ClientID()
	tr := &order.Trade{Contract: contract.Stock("AAPL", "SMART", "USD"), Order: o,
		OrderStatus: &order.OrderStatus{OrderID: orderID, Status: status}}
	ib.state.Trades[state.OrderKey{ClientID: o.ClientID, OrderID: orderID}] = tr
	return tr
}

func TestParseIBDatetime(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	cases := []struct {
		in   string
		want time.Time
	}{
		{"20231019", time.Date(2023, 10, 19, 0, 0, 0, 0, time.UTC)},
		{"1697722200", time.Unix(1697722200, 0)},
		{"20231019 09:30:00 US/Eastern", time.Date(2023, 10, 19, 9, 30, 0, 0, ny)},
		{"20231019 09:30:00 America/New_York", time.Date(2023, 10, 19, 9, 30, 0, 0, ny)},
		{"20231019  09:30:00", time.Date(2023, 10, 19, 9, 30, 0, 0, time.UTC)},
		{"2023-10-19 09:30:00.0", time.Date(2023, 10, 19, 9, 30, 0, 0, time.UTC)},
		{"20231019 09:30:00 Not/AZone", time.Date(2023, 10, 19, 9, 30, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		if got := parseIBDatetime(c.in); !got.Equal(c.want) {
			t.Errorf("parseIBDatetime(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, bad := range []string{"", "garbage", "2023"} {
		if got := parseIBDatetime(bad); !got.IsZero() && bad != "2023" {
			t.Errorf("parseIBDatetime(%q) = %v, want zero", bad, got)
		}
	}
}

func TestErrorWarningKeepsOrderLive(t *testing.T) {
	ib := New()
	ib.client.SetServerVersion(178)
	ib.client.ClientID = 1
	tr := liveTrade(ib, 5, order.StatusSubmitted)

	var events atomic.Int32
	tr.StatusEvent.Subscribe(func(*order.Trade) { events.Add(1) })
	feed(ib, ib.handleError, "4", "2", "5", "399", "Order Message: Warning: your order will not be placed at the exchange until ...", "")

	if tr.Status() != order.StatusValidationError || !tr.IsActive() || tr.IsDone() {
		t.Fatalf("warning must keep the order active, status %s", tr.Status())
	}
	if events.Load() != 1 {
		t.Fatalf("status events = %d", events.Load())
	}
}

func TestErrorRejectCancelsOrder(t *testing.T) {
	ib := New()
	ib.client.SetServerVersion(178)
	ib.client.ClientID = 1
	tr := liveTrade(ib, 6, order.StatusPendingSubmit)
	cancelled := tr.CancelledEvent.Once()

	feed(ib, ib.handleError, "4", "2", "6", "201", "Order rejected - reason: no margin", `{"reason":"margin"}`)
	select {
	case <-cancelled:
	default:
		t.Fatal("CancelledEvent not fired")
	}
	if tr.Status() != order.StatusCancelled || tr.AdvancedError == "" {
		t.Fatalf("status %s advanced %q", tr.Status(), tr.AdvancedError)
	}

	// 110 (price increment) is a warning, except for a not-yet-acked order.
	tr2 := liveTrade(ib, 7, order.StatusPendingSubmit)
	feed(ib, ib.handleError, "4", "2", "7", "110", "The price does not conform to the minimum price variation", "")
	if tr2.Status() != order.StatusCancelled {
		t.Fatalf("110 on PendingSubmit order: status %s", tr2.Status())
	}
}

func TestErrorWarningDoesNotFailRequest(t *testing.T) {
	ib := New()
	ib.client.SetServerVersion(178)
	ch := ib.state.StartReq(42, nil)

	var got atomic.Pointer[IBError]
	ib.ErrorEvent.Subscribe(func(e *IBError) { got.Store(e) })

	// Info code: logged only.
	feed(ib, ib.handleError, "4", "2", "-1", "2104", "Market data farm connection is OK:usfarm", "")
	if got.Load() != nil {
		t.Fatal("info codes must not reach ErrorEvent")
	}
	// Delayed-data notice (10167) is a warning: the request stays pending.
	feed(ib, ib.handleError, "4", "2", "42", "10167", "Requested market data is not subscribed. Displaying delayed market data.", "")
	if e := got.Load(); e == nil || !e.IsWarning {
		t.Fatalf("warning event = %+v", e)
	}
	if !ib.state.Requests.Has(42) {
		t.Fatal("warning must not end the request")
	}
	// A real error ends it with a RequestError.
	feed(ib, ib.handleError, "4", "2", "42", "200", "No security definition has been found", "")
	res := <-ch
	var re *RequestError
	if !errors.As(res.Err, &re) || re.Code != 200 {
		t.Fatalf("request result err = %v", res.Err)
	}
}

func TestOrderStatusDuplicateIsIgnored(t *testing.T) {
	ib := New()
	ib.client.SetServerVersion(178)
	ib.client.ClientID = 1
	tr := liveTrade(ib, 8, order.StatusPendingSubmit)
	var events atomic.Int32
	tr.StatusEvent.Subscribe(func(*order.Trade) { events.Add(1) })

	status := []string{"3", "8", "Submitted", "0", "10", "0", "777", "0", "0", "1", "", "0"}
	feed(ib, ib.handleOrderStatus, status...)
	feed(ib, ib.handleOrderStatus, status...)
	if events.Load() != 1 {
		t.Fatalf("duplicate orderStatus fired %d events, want 1", events.Load())
	}
	if tr.PermID() != 777 {
		t.Fatalf("permId from orderStatus not recorded: %d", tr.PermID())
	}
	if len(tr.LogSnapshot()) != 1 {
		t.Fatalf("log entries = %d, want 1", len(tr.LogSnapshot()))
	}
}

func TestExecDetailsDuplicateFiresOneEvent(t *testing.T) {
	ib := New()
	ib.client.SetServerVersion(178)
	tr := liveTrade(ib, 42, order.StatusSubmitted)
	tr.Order.ClientID = 1
	ib.state.Trades[state.OrderKey{ClientID: 1, OrderID: 42}] = tr
	var fills atomic.Int32
	tr.FillEvent.Subscribe(func(*order.Fill) { fills.Add(1) })

	sendExecDetails(t, ib, "dup1", 5, 100, 999, 1, 42)
	sendExecDetails(t, ib, "dup1", 5, 100, 999, 1, 42)
	if fills.Load() != 1 || tr.FilledQty() != 5 {
		t.Fatalf("fill events = %d, filled = %g", fills.Load(), tr.FilledQty())
	}
}

func TestConnectionStateAfterRemoteClose(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)

	if err := ib.Connect(ctx5s(t), "127.0.0.1", srv.Port(), 1); !errors.Is(err, protocol.ErrAlreadyConnected) {
		t.Fatalf("second Connect err = %v, want ErrAlreadyConnected", err)
	}
	srv.DropConnection()
	waitFor(t, "IsConnected false after peer closed", func() bool { return !ib.IsConnected() })
	if _, err := ib.PlaceOrder(aaplE2E(), order.LimitOrder("BUY", 1, 1)); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("PlaceOrder while disconnected: %v", err)
	}
	// Reconnecting the same client works.
	if err := ib.Connect(ctx5s(t), "127.0.0.1", srv.Port(), e2eClientID); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
}

func TestConnectHonoursContext(t *testing.T) {
	ib := New()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	// 10.255.255.1 is non-routable: the dial hangs unless the context aborts it.
	err := ib.Connect(ctx, "10.255.255.1", 4002, 1)
	if err == nil {
		t.Fatal("expected connect error")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("connect ignored context, took %v", d)
	}
	if ib.client.State() != protocol.Disconnected {
		t.Fatal("state must return to Disconnected after a failed connect")
	}
}

func TestUnqualifiedTickersDoNotCollide(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	c1 := contract.Stock("AAPL", "SMART", "USD") // conId 0
	c2 := contract.Stock("MSFT", "SMART", "USD") // conId 0
	t1, err := ib.ReqMktData(c1, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := ib.ReqMktData(c2, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	ib.CancelMktData(c1)

	ib.state.Mu.RLock()
	defer ib.state.Mu.RUnlock()
	if len(ib.state.ReqID2Ticker) != 1 {
		t.Fatalf("subscriptions left = %d, want 1", len(ib.state.ReqID2Ticker))
	}
	for _, tk := range ib.state.ReqID2Ticker {
		if tk != t2 || tk == t1 {
			t.Fatal("CancelMktData cancelled the wrong subscription")
		}
	}
}
