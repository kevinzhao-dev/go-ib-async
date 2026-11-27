package ibgo

// Regression tests for the second live-gateway review: lock order between
// subscription restore and cancellation, start/cancel ordering, one ticker
// per contract across reconnects, market depth books, tick-by-tick
// backfill, account update lifecycle and QualifyContracts aliasing.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
	"github.com/kevinzhao-dev/go-ib-async/market"
)

// startReconnecting runs ConnectWithReconnect against srv; every
// (re)connection sends on the returned channel. It returns after the first
// connection.
func startReconnecting(t *testing.T, srv *fakegw.Server, ib *IB) <-chan struct{} {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cfg := DefaultReconnectConfig()
	cfg.RetryDelay = 20 * time.Millisecond
	cfg.ProbeInterval = 0
	cfg.SyncOrders = false
	connected := make(chan struct{}, 8)
	done := make(chan struct{})
	go func() {
		_ = ib.ConnectWithReconnect(ctx, "127.0.0.1", srv.Port(), e2eClientID, cfg, func() { connected <- struct{}{} })
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("ConnectWithReconnect did not stop")
		}
	})
	waitConnected(t, connected)
	return connected
}

func waitConnected(t *testing.T, connected <-chan struct{}) {
	t.Helper()
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("not (re)connected")
	}
}

// roundTrip makes the reader process everything the fake sent before.
func roundTrip(t *testing.T, ib *IB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := ib.ReqCurrentTime(ctx); err != nil {
		t.Fatal(err)
	}
}

// An application may hold a ticker's or bar list's read lock while calling
// Cancel*. The restore after a reconnect must not wait for that lock while
// holding the lock Cancel* needs.
func TestCancelWhileHoldingReadLocksDuringRestore(t *testing.T) {
	srv := startFake(t)
	ib := New()
	connected := startReconnecting(t, srv, ib)
	bars, err := ib.ReqRealTimeBars(aaplE2E(), 5, "TRADES", false)
	if err != nil {
		t.Fatal(err)
	}
	depthCon := msftE2E()
	tk, err := ib.ReqMktDepth(depthCon, 5, false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "requests", func() bool { return srv.Received(50) == 1 && srv.Received(10) == 1 })

	bars.RLock()
	tk.RLock()
	srv.DropConnection()
	waitFor(t, "re-sent requests", func() bool { return srv.Received(50) == 2 && srv.Received(10) == 2 })
	done := make(chan struct{})
	go func() {
		ib.CancelRealTimeBars(bars)
		ib.CancelMktDepth(depthCon, false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Cancel* blocked while the caller holds read locks (lock cycle with the restore)")
	}
	tk.RUnlock()
	bars.RUnlock()
	waitConnected(t, connected)
	waitFor(t, "cancels", func() bool { return srv.Received(51) == 1 && srv.Received(11) == 1 })
	if n := numSubs(ib); n != 0 {
		t.Fatalf("%d subscriptions left", n)
	}
}

// A cancel racing with a stalled initial send must reach TWS after the
// request, never before it.
func TestCancelWaitsForInitialSend(t *testing.T) {
	ib := newTestIB()
	ib.connEpoch.Store(1)
	var mu sync.Mutex
	var events []string
	record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	entered, release := make(chan struct{}), make(chan struct{})
	con := aaplE2E()
	s := &subscription{
		kind: subMktData, con: con,
		send:   func(int64) error { close(entered); <-release; record("start"); return nil },
		cancel: func(int64) error { record("cancel"); return nil },
	}
	started := make(chan error, 1)
	go func() { _, err := ib.startSub(s); started <- err }()
	<-entered
	cancelled := make(chan bool, 1)
	go func() { cancelled <- ib.endSubByContract(subMktData, "", con) }()
	time.Sleep(20 * time.Millisecond) // the cancel is now waiting for the send
	close(release)
	if err := <-started; err != nil {
		t.Fatal(err)
	}
	if !<-cancelled {
		t.Fatal("cancel found no subscription")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(events, []string{"start", "cancel"}) {
		t.Fatalf("events = %v, want [start cancel]", events)
	}
	if n := numSubs(ib); n != 0 {
		t.Fatalf("%d subscriptions left", n)
	}
}

// Requests made from a ConnectedEvent handler before the restore, and
// concurrent requests, share the contract's one ticker.
func TestOneTickerPerContractAcrossReconnect(t *testing.T) {
	srv := startFake(t)
	ib := New()
	var conns atomic.Int32
	second := make(chan *market.Ticker, 1)
	ib.ConnectedEvent.Subscribe(func(struct{}) {
		if conns.Add(1) == 2 {
			tk, err := ib.ReqTickByTickData(aaplE2E(), TickByTickBidAsk, 0, false)
			if err != nil {
				t.Error(err)
			}
			second <- tk
		}
	})
	connected := startReconnecting(t, srv, ib)
	first, err := ib.ReqMktData(aaplE2E(), "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	srv.DropConnection()
	waitConnected(t, connected)
	if tk := <-second; tk != first {
		t.Fatal("tick-by-tick request after the reconnect got a second ticker")
	}
	ib.state.Mu.RLock()
	n := len(ib.state.ReqID2Ticker)
	for id, tk := range ib.state.ReqID2Ticker {
		if tk != first {
			t.Errorf("request %d updates another ticker", id)
		}
	}
	if ib.state.Tickers[aaplE2E().Key()] != first {
		t.Error("contract index lost the ticker")
	}
	ib.state.Mu.RUnlock()
	if n != 2 {
		t.Fatalf("%d ticker requests, want the restored market data and the tick-by-tick", n)
	}

	var wg sync.WaitGroup
	got := make([]*market.Ticker, 8)
	for i := range got {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _ = ib.ReqMktData(msftE2E(), "", false, false)
		}(i)
	}
	wg.Wait()
	for _, tk := range got {
		if tk == nil || tk != got[0] {
			t.Fatal("concurrent requests for one contract got different tickers")
		}
	}
}

func depthTestIB(t *testing.T) (*IB, *market.Ticker) {
	t.Helper()
	ib := newTestIB()
	s := &subscription{kind: subMktDepth, con: aaplE2E()}
	ib.registerSub(5, s, 1)
	ib.depthStarted(5)
	return ib, s.ticker
}

func bidPrices(tk *market.Ticker) []float64 {
	var out []float64
	for _, l := range tk.Snapshot().DomBids {
		out = append(out, l.Price)
	}
	return out
}

// The book is emptied on the reader goroutine: when the first update of a
// (re)request arrives and when TWS resets it with error 317.
func TestDepthBookResets(t *testing.T) {
	ib, tk := depthTestIB(t)
	tk.Lock()
	tk.DomBids = []market.DOMLevel{{Price: 1}, {Price: 2}} // stale book of an earlier request
	tk.Unlock()

	feedMsg(ib, "12|1|5|0|0|1|100|10")
	feedMsg(ib, "12|1|5|1|0|1|99|10")
	if got := bidPrices(tk); !slices.Equal(got, []float64{100, 99}) {
		t.Fatalf("book after the first updates = %v, want [100 99]", got)
	}

	var ibErr *IBError
	ib.ErrorEvent.Subscribe(func(e *IBError) { ibErr = e })
	feedMsg(ib, "4|2|5|317|Market depth data has been RESET. Please empty deep book contents before applying any new entries.|")
	if got := bidPrices(tk); len(got) != 0 {
		t.Fatalf("book after error 317 = %v, want empty", got)
	}
	if ibErr == nil || !ibErr.IsWarning {
		t.Fatalf("317 should be a warning (the stream goes on): %+v", ibErr)
	}
	feedMsg(ib, "12|1|5|0|0|1|98|10")
	if got := bidPrices(tk); !slices.Equal(got, []float64{98}) {
		t.Fatalf("book after the reset = %v, want [98]", got)
	}
}

// One ticker holds one book: a second depth stream for the contract, SMART
// or not, is refused until the first is cancelled.
func TestOneDepthSubscriptionPerContract(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	if _, err := ib.ReqMktDepth(aaplE2E(), 5, false); err != nil {
		t.Fatal(err)
	}
	if _, err := ib.ReqMktDepth(aaplE2E(), 5, true); !errors.Is(err, ErrDepthSubscribed) {
		t.Fatalf("second depth request: err = %v, want ErrDepthSubscribed", err)
	}
	ib.CancelMktDepth(aaplE2E(), false)
	if _, err := ib.ReqMktDepth(aaplE2E(), 5, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "depth requests", func() bool { return srv.Received(10) == 2 && srv.Received(11) == 1 })
}

// Ticks requested with numberOfTicks > 0 arrive as historicalTicks*
// messages with the tick-by-tick request id, before the live ticks.
func TestTickByTickBackfill(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	tk, err := ib.ReqTickByTickData(aaplE2E(), TickByTickBidAsk, 1, false)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var bids []float64
	tk.UpdateEvent.Subscribe(func(t *market.Ticker) {
		mu.Lock()
		defer mu.Unlock()
		for _, x := range t.TickByTicks {
			bids = append(bids, x.(market.TickByTickBidAsk).BidPrice)
		}
	})
	waitFor(t, "tick-by-tick request", func() bool { return srv.Received(97) == 1 })
	id := srv.LastRequest(97)[1]
	srv.Handle(49, func([]string) [][]byte {
		return [][]byte{
			fakegw.Msg(97, id, 1, 1795753100, 0, 100.25, 100.5, 3, 4, 1), // backfill
			fakegw.Msg(99, id, 3, 1795753110, 100.3, 100.55, 5, 6, 0),    // live
			fakegw.Msg(49, 1, 1795753114),
		}
	})
	roundTrip(t, ib)
	waitFor(t, "ticks", func() bool { mu.Lock(); defer mu.Unlock(); return len(bids) == 2 })
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(bids, []float64{100.25, 100.3}) {
		t.Fatalf("bids = %v, want the backfill then the live tick", bids)
	}
	if d := tk.Snapshot(); d.Bid != 100.3 || d.Ask != 100.55 {
		t.Fatalf("bid/ask = %v/%v", d.Bid, d.Ask)
	}
}

// Error 2101 (another client holds an account subscription) fails the
// pending ReqAccountUpdates instead of letting it time out.
func TestAccountUpdatesRejectedByTWS(t *testing.T) {
	srv := startFake(t)
	srv.Handle(6, func(req []string) [][]byte {
		if req[2] != "1" {
			return nil
		}
		return [][]byte{fakegw.Msg(4, 2, -1, 2101, "Unable to subscribe to account as the following clients are subscribed to a different account.", "")}
	})
	ib := connectFake(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := ib.ReqAccountUpdates(ctx, "")
	var ibe *IBError
	if !errors.As(err, &ibe) || ibe.Code != 2101 || ibe.IsWarning {
		t.Fatalf("err = %v, want error 2101", err)
	}
	ib.acctMu.Lock()
	defer ib.acctMu.Unlock()
	if ib.acctUpdates != nil {
		t.Fatal("rejected subscription is registered (would be re-sent after a reconnect)")
	}
}

// A cancel while the initial download is running ends the request and is
// not undone by the late accountDownloadEnd.
func TestCancelAccountUpdatesDuringDownload(t *testing.T) {
	srv := startFake(t)
	srv.Handle(6, func([]string) [][]byte { return nil }) // download never ends by itself
	ib := connectFake(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- ib.ReqAccountUpdates(ctx, "") }()
	waitFor(t, "subscribe", func() bool { return srv.Received(6) == 1 })
	if err := ib.CancelAccountUpdates(""); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; !errors.Is(err, ErrRequestCancelled) {
		t.Fatalf("ReqAccountUpdates err = %v, want ErrRequestCancelled", err)
	}
	srv.Handle(49, func([]string) [][]byte {
		return [][]byte{fakegw.Msg(54, 1, "DU123456"), fakegw.Msg(49, 1, 1795753114)}
	})
	roundTrip(t, ib)
	ib.acctMu.Lock()
	defer ib.acctMu.Unlock()
	if ib.acctUpdates != nil {
		t.Fatal("cancelled subscription registered again")
	}
}

// A position closed while disconnected is missing from the download after
// the reconnect: it must leave Portfolio() when that download ends.
func TestPortfolioReplacedByReconnectDownload(t *testing.T) {
	srv := startFake(t)
	var subscribes atomic.Int32
	srv.Handle(6, func(req []string) [][]byte {
		if req[2] != "1" {
			return nil
		}
		var out [][]byte
		if subscribes.Add(1) == 1 {
			out = append(out, fakegw.Msg(7, 8, 815824257, "MES", "FUT", "20261218", 0, "0", "5", "CME", "USD", "MESZ6", "MES", 3, 7312.13, 109681.88, 36540.25, 60.13, 0.0, req[3]))
		}
		return append(out, fakegw.Msg(54, 1, req[3]))
	})
	ib := New()
	connected := startReconnecting(t, srv, ib)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ib.ReqAccountUpdates(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if n := len(ib.Portfolio()); n != 1 {
		t.Fatalf("portfolio = %d items, want 1", n)
	}
	var removed atomic.Pointer[account.PortfolioItem]
	ib.PortfolioEvent.Subscribe(func(it *account.PortfolioItem) {
		if it.Position == 0 {
			removed.Store(it)
		}
	})
	srv.DropConnection()
	waitConnected(t, connected)
	waitFor(t, "stale position removed", func() bool { return len(ib.Portfolio()) == 0 })
	if it := removed.Load(); it == nil || it.Contract.ConID != 815824257 {
		t.Fatalf("no PortfolioEvent for the removed position: %+v", it)
	}
}

// The market data type chosen before a reconnect applies to requests made
// from ConnectedEvent handlers, which run before the restore.
func TestMarketDataTypeRestoredBeforeConnectedEvent(t *testing.T) {
	srv := startFake(t)
	mdTypesSeen := make(chan int, 4)
	srv.Handle(1, func([]string) [][]byte {
		mdTypesSeen <- srv.Received(59)
		return nil
	})
	ib := New()
	var conns atomic.Int32
	ib.ConnectedEvent.Subscribe(func(struct{}) {
		if conns.Add(1) == 2 {
			if _, err := ib.ReqMktData(msftE2E(), "", false, false); err != nil {
				t.Error(err)
			}
		}
	})
	connected := startReconnecting(t, srv, ib)
	if err := ib.ReqMarketDataType(MarketDataDelayed); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "market data type", func() bool { return srv.Received(59) == 1 })
	srv.DropConnection()
	waitConnected(t, connected)
	select {
	case n := <-mdTypesSeen:
		if n != 2 {
			t.Fatalf("reqMktData arrived after %d reqMarketDataType messages, want 2 (re-applied first)", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no reqMktData")
	}
	if got := srv.LastRequest(59); len(got) < 3 || got[2] != "3" {
		t.Fatalf("reqMarketDataType = %v, want type 3", got)
	}
	roundTrip(t, ib)
	if n := srv.Received(59); n != 2 {
		t.Fatalf("reqMarketDataType sent %d times, want 2", n)
	}
}

// QualifyContracts may get the same pointer twice (no data race: run with
// -race) and keeps IncludeExpired, a request option.
func TestQualifyContractsAliasedAndIncludeExpired(t *testing.T) {
	srv := startFake(t)
	srv.Handle(9, func(req []string) [][]byte {
		return [][]byte{contractDetailsMsg(req[2], "STK", ""), fakegw.Msg(52, 1, req[2])}
	})
	ib := connectFake(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := contract.Stock("AAPL", "SMART", "USD")
	c.IncludeExpired = true
	if err := ib.QualifyContracts(ctx, c, c); err != nil {
		t.Fatal(err)
	}
	if c.ConID != 265598 || c.Exchange != "SMART" || !c.IncludeExpired {
		t.Fatalf("qualified contract = %+v", c)
	}
}

// Errors of bar subscriptions carry the request's contract pointer, so a
// caller can tell which request failed (cmd/mdcheck relies on it).
func TestSubscriptionErrorsCarryContract(t *testing.T) {
	srv := startFake(t)
	srv.Handle(50, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(4, 2, req[2], 354, "Requested market data is not subscribed.", "")}
	})
	ib := connectFake(t, srv)
	errs := make(chan *IBError, 1)
	ib.ErrorEvent.Subscribe(func(e *IBError) {
		if e.Code == 354 {
			errs <- e
		}
	})
	con := aaplE2E()
	if _, err := ib.ReqRealTimeBars(con, 5, "TRADES", false); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-errs:
		if e.Contract != con {
			t.Fatalf("error contract = %v, want the request's pointer", e.Contract)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no error")
	}
}

// With client id 0, reqOpenOrders would bind manual TWS orders: SyncOrders
// reads them with reqAllOpenOrders instead.
func TestSyncOrdersClientZeroDoesNotBind(t *testing.T) {
	srv := startFake(t)
	ib := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ib.Connect(ctx, "127.0.0.1", srv.Port(), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ib.Disconnect)
	if _, err := ib.SyncOrders(ctx, SyncOptions{SkipPositions: true}); err != nil {
		t.Fatal(err)
	}
	if n := srv.Received(5); n != 0 {
		t.Fatalf("reqOpenOrders sent %d times from client 0", n)
	}
	if n := srv.Received(16); n != 1 {
		t.Fatalf("reqAllOpenOrders sent %d times, want 1", n)
	}
}
