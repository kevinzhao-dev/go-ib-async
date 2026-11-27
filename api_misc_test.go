package ibgo

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

func TestReqGlobalCancelAndAllOpenOrders(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	a, err := ib.PlaceOrder(aaplE2E(), order.LimitOrder("BUY", 1, 10))
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, a, order.StatusSubmitted)

	all, err := ib.ReqAllOpenOrders(ctx5s(t))
	if err != nil || len(all) != 1 || all[0] != a {
		t.Fatalf("ReqAllOpenOrders = %v, %v", all, err)
	}
	if err := ib.ReqAutoOpenOrders(true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reqAutoOpenOrders", func() bool { return srv.Received(15) == 1 })

	if err := ib.ReqGlobalCancel(); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, a, order.StatusCancelled)
}

func TestRequestsFailFastWhenDisconnected(t *testing.T) {
	ib := New()
	ctx := ctx5s(t)
	checks := map[string]error{}
	_, checks["ReqContractDetails"] = ib.ReqContractDetails(ctx, aaplE2E())
	_, checks["ReqPositions"] = ib.ReqPositions(ctx)
	_, checks["ReqAccountSummary"] = ib.ReqAccountSummary(ctx, "All", "NetLiquidation")
	_, checks["ReqHistoricalData"] = ib.ReqHistoricalData(ctx, aaplE2E(), "", "1 D", "1 day", "TRADES", true, 1)
	_, checks["ReqSecDefOptParams"] = ib.ReqSecDefOptParams(ctx, "AAPL", "", "STK", 1)
	_, checks["ReqOpenOrders"] = ib.ReqOpenOrders(ctx)
	_, checks["ReqExecutions"] = ib.ReqExecutions(ctx, order.ExecutionFilter{})
	_, checks["ReqCompletedOrders"] = ib.ReqCompletedOrders(ctx, false)
	_, checks["ReqMktData"] = ib.ReqMktData(aaplE2E(), "", false, false)
	_, checks["ReqRealTimeBars"] = ib.ReqRealTimeBars(aaplE2E(), 5, "TRADES", true)
	_, checks["WhatIfOrder"] = ib.WhatIfOrder(ctx, aaplE2E(), order.LimitOrder("BUY", 1, 1))
	_, checks["PlaceOrder"] = ib.PlaceOrder(aaplE2E(), order.LimitOrder("BUY", 1, 1))
	_, checks["SyncOrders"] = ib.SyncOrders(ctx, SyncOptions{})
	b, _ := order.NewBracket("BUY", 1, 10, 11, 9)
	_, checks["PlaceBracketOrder"] = ib.PlaceBracketOrder(aaplE2E(), b)
	for name, err := range checks {
		if err == nil {
			t.Errorf("%s must fail without a connection", name)
		}
	}
	if ib.state.Requests.Len() != 0 {
		t.Fatalf("failed requests leaked %d pending entries", ib.state.Requests.Len())
	}
}

func TestInputValidation(t *testing.T) {
	ib := New()
	if _, err := ib.PlaceOrder(nil, order.MarketOrder("BUY", 1)); err == nil {
		t.Error("PlaceOrder(nil contract)")
	}
	if _, err := ib.CancelOrder(&order.Order{}); err == nil {
		t.Error("CancelOrder without orderId")
	}
	if _, err := ib.WhatIfOrder(ctx5s(t), aaplE2E(), nil); err == nil {
		t.Error("WhatIfOrder(nil order)")
	}
	if _, err := ib.PlaceBracketOrder(aaplE2E(), &order.BracketOrder{}); err == nil {
		t.Error("PlaceBracketOrder with missing legs")
	}
	b, _ := order.NewBracket("BUY", 1, 10, 11, 9)
	b.StopLoss.WhatIf = true
	if _, err := ib.PlaceBracketOrder(aaplE2E(), b); !errors.Is(err, ErrUseWhatIfOrder) {
		t.Errorf("bracket with whatIf leg: %v", err)
	}
}

func TestPlaceBracketRollsBackWhenALegFails(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	failing := &failOnNthJournal{failAt: 3} // parent and take-profit pass, stop-loss fails
	ib.SetJournal(failing)

	b, err := ib.BracketOrder("BUY", 1, 10, 11, 9)
	if err != nil {
		t.Fatal(err)
	}
	trades, err := ib.PlaceBracketOrder(aaplE2E(), b)
	if err == nil || len(trades) != 2 {
		t.Fatalf("trades=%d err=%v, want 2 sent legs and an error", len(trades), err)
	}
	for _, tr := range trades {
		if s := tr.Status(); s != order.StatusCancelled && s != order.StatusPendingCancel {
			t.Fatalf("sent leg not rolled back: %s", s)
		}
	}
	waitFor(t, "cancels sent", func() bool { return srv.Received(4) == 2 })
}

// failOnNthJournal is an OrderJournal whose failAt-th RecordSubmit fails.
type failOnNthJournal struct {
	mu        sync.Mutex
	n, failAt int
}

func (j *failOnNthJournal) RecordSubmit(*contract.Contract, *order.Order, bool) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.n++
	if j.n == j.failAt {
		return errors.New("disk full")
	}
	return nil
}
func (j *failOnNthJournal) RecordCancelRequest(*order.Trade) error { return nil }
func (j *failOnNthJournal) RecordStatus(*order.Trade) error        { return nil }
func (j *failOnNthJournal) RecordFill(*order.Trade, *order.Fill) error {
	return nil
}
func (j *failOnNthJournal) RecordCommission(*order.Trade, *order.Fill) error { return nil }
func (j *failOnNthJournal) RecordError(*order.Trade, int, string) error      { return nil }

func TestConnectWithReconnectGivesUpAfterMaxRetries(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close() // nothing listens: every attempt is refused

	ib := New()
	start := time.Now()
	err = ib.ConnectWithReconnect(ctx5s(t), "127.0.0.1", port, 1, ReconnectConfig{RetryDelay: 10 * time.Millisecond, MaxRetries: 2}, nil)
	if err == nil {
		t.Fatal("expected the last connection error")
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("retries took too long")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ib.ConnectWithReconnect(ctx, "127.0.0.1", port, 1, DefaultReconnectConfig(), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context: %v", err)
	}
}

func TestDefaultReconnectConfig(t *testing.T) {
	c := DefaultReconnectConfig()
	if c.RetryDelay <= 0 || c.ConnectTimeout <= 0 || c.ProbeInterval <= 0 || c.ProbeTimeout <= 0 || !c.SyncOrders || c.SyncTimeout <= 0 || c.MaxRetries != 0 {
		t.Fatalf("defaults = %+v", c)
	}
}

func TestHeartbeatStopsOnContextAndDisconnect(t *testing.T) {
	srv, err := fakegw.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ib := connectFake(t, srv)

	// Peer answers reqCurrentTime: heartbeat keeps the connection alive.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go ib.heartbeatLoop(ctx, ReconnectConfig{ProbeInterval: 20 * time.Millisecond, ProbeTimeout: 200 * time.Millisecond}, ib.client.Done(), done)
	time.Sleep(150 * time.Millisecond)
	if !ib.IsConnected() {
		t.Fatal("responsive peer must not be disconnected by the heartbeat")
	}
	if srv.Received(49) == 0 {
		t.Fatal("heartbeat sent no probe")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not stop on context cancel")
	}

	// Connection loss stops the loop as well.
	done = make(chan struct{})
	go ib.heartbeatLoop(context.Background(), ReconnectConfig{ProbeInterval: time.Hour}, ib.client.Done(), done)
	srv.DropConnection()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat did not stop on disconnect")
	}
}
