package ibgo

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
	"github.com/kevinzhao-dev/go-ib-async/journal"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

// openOrderEnd carries no request id: the late end marker of an abandoned
// request must not complete the next one (which would return the first
// request's orders, or none, as if TWS had answered it).
func TestAbandonedOpenOrdersRequestKeepsItsSlot(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	srv.Handle(16, func([]string) [][]byte { return nil }) // reqAllOpenOrders: answer comes later

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := ib.ReqAllOpenOrders(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first request: %v", err)
	}
	ctx2 := ctx5s(t)
	got := make(chan error, 1)
	go func() { _, err := ib.ReqOpenOrders(ctx2); got <- err }()
	time.Sleep(50 * time.Millisecond)
	if n := srv.Received(5); n != 0 {
		t.Fatal("reqOpenOrders was sent while the abandoned request's answer was outstanding")
	}
	select {
	case err := <-got:
		t.Fatalf("second request ended before TWS answered it: %v", err)
	default:
	}

	srv.Send(fakegw.Msg(53, 1)) // the late openOrderEnd of the abandoned request
	waitFor(t, "second request sent", func() bool { return srv.Received(5) == 1 })
	if err := <-got; err != nil {
		t.Fatalf("second request: %v", err)
	}
}

// An end marker that never comes holds the slot for lateEndTimeout only.
func TestAbandonedRequestSlotIsReleasedAfterTimeout(t *testing.T) {
	old := lateEndTimeout
	lateEndTimeout = 50 * time.Millisecond
	t.Cleanup(func() { lateEndTimeout = old })

	srv := startFake(t)
	ib := connectFake(t, srv)
	srv.Handle(16, func([]string) [][]byte { return nil }) // never answered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := ib.ReqAllOpenOrders(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first request: %v", err)
	}
	if _, err := ib.ReqOpenOrders(ctx5s(t)); err != nil {
		t.Fatalf("request after the timeout: %v", err)
	}
}

// OpenOrdersSnapshot reports other clients' orders without the client
// tracking them: TWS sends no updates for those, so tracked copies would go
// stale in Trades, the journal and the risk checks.
func TestOpenOrdersSnapshotDetachesOtherClientsOrders(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	jpath := filepath.Join(t.TempDir(), "orders.jsonl")
	j, err := journal.Open(jpath, journal.Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	ib.SetJournal(j)

	aapl := fakegw.Contract{ConID: 265598, Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	manual := srv.AddOrder(fakegw.Order{Contract: aapl, Action: "SELL", Qty: 3, OrderType: "LMT", LmtPrice: 200, TIF: "GTC", Account: "DU123456", Filled: 1})
	other := srv.AddOrder(fakegw.Order{ClientID: 3, OrderID: 12, Contract: aapl, Action: "BUY", Qty: 8, OrderType: "LMT", LmtPrice: 140, TIF: "DAY", Account: "DU123456"})
	con := &contract.Contract{ConID: 265598, Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	mine, err := ib.PlaceOrder(con, order.LimitOrder("BUY", 2, 150))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "submitted", func() bool { return mine.Status() == order.StatusSubmitted })

	snap, err := ib.OpenOrdersSnapshot(ctx5s(t))
	if err != nil {
		t.Fatal(err)
	}
	byPerm := map[int64]*order.Trade{}
	for _, tr := range snap {
		byPerm[tr.PermID()] = tr
	}
	if len(snap) != 3 || byPerm[mine.PermID()] != mine || byPerm[manual] == nil || byPerm[other] == nil {
		t.Fatalf("snapshot = %d trades %v", len(snap), byPerm)
	}
	m := byPerm[manual]
	if m.Status() != order.StatusSubmitted || m.StatusSnapshot().Filled != 1 || m.OrderSnapshot().TotalQuantity != 3 {
		t.Fatalf("detached manual order: status %s, %+v", m.Status(), m.StatusSnapshot())
	}
	for _, tr := range ib.Trades() {
		if tr.PermID() == manual || tr.PermID() == other {
			t.Fatalf("snapshot order %d is tracked by the client", tr.PermID())
		}
	}

	// The journal holds only this client's order, and replaying it against
	// the client's trades finds no discrepancy.
	recs, err := journal.Load(jpath)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if r.PermID == manual || r.PermID == other || r.ClientID == 3 {
			t.Fatalf("journal has a record of another client's order: %+v", r)
		}
	}
	if d := journal.Reconcile(journal.Replay(recs), ib.Trades(), ib.ClientID()); len(d) != 0 {
		t.Fatalf("discrepancies = %+v", d)
	}

	// The Master API client gets updates for every client's orders, so an
	// order it tracks that way stays a live trade in the snapshot.
	srv.NotifyOrder(other)
	waitFor(t, "tracked", func() bool {
		for _, tr := range ib.OpenTrades() {
			if tr.PermID() == other {
				return true
			}
		}
		return false
	})
	snap, err = ib.OpenOrdersSnapshot(ctx5s(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range snap {
		if tr.PermID() == other {
			if found := ib.OpenTrades(); !containsTrade(found, tr) {
				t.Fatal("a tracked order must come back as the client's own trade")
			}
		}
	}
}

func containsTrade(ts []*order.Trade, t *order.Trade) bool {
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}
