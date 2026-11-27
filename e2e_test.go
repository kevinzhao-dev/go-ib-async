package ibgo

// End-to-end trading tests: the real client (protocol, decoders, state,
// events, risk manager, journal) against internal/fakegw, a fake TWS that
// speaks the v178 wire protocol and simulates order handling and fills.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
	"github.com/kevinzhao-dev/go-ib-async/journal"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/risk"
)

// Compile-time checks that the packages plug into the client.
var (
	_ OrderJournal       = (*journal.Journal)(nil)
	_ PreTradeChecker    = (*risk.Manager)(nil)
	_ CommissionObserver = (*risk.Manager)(nil)
	_ risk.StateSource   = (*IB)(nil)
)

const e2eClientID = 7

func startFake(t *testing.T) *fakegw.Server {
	t.Helper()
	srv, err := fakegw.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func connectFake(t *testing.T, srv *fakegw.Server) *IB {
	t.Helper()
	ib := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ib.Connect(ctx, "127.0.0.1", srv.Port(), e2eClientID); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(ib.Disconnect)
	return ib
}

func aaplE2E() *contract.Contract {
	c := contract.Stock("AAPL", "SMART", "USD")
	c.ConID = 265598
	c.LocalSymbol = "AAPL"
	c.TradingClass = "NMS"
	return c
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitStatus(t *testing.T, tr *order.Trade, status string) {
	t.Helper()
	waitFor(t, "status "+status+" (have "+tr.Status()+")", func() bool { return tr.Status() == status })
}

func ctx5s(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestE2ELimitOrderPartialFillsWithJournalAndRisk(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)

	jpath := filepath.Join(t.TempDir(), "orders.jsonl")
	j, err := journal.Open(jpath, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ib.SetJournal(j)
	rm := risk.New(risk.Limits{MaxOrderQuantity: 1000, MaxPositionQuantity: 1000}, ib)
	ib.SetRiskManager(rm)

	var execEvents, commEvents atomic.Int32
	ib.ExecDetailsEvent.Subscribe(func(*order.Fill) { execEvents.Add(1) })
	ib.CommissionReportEvent.Subscribe(func(*order.Fill) { commEvents.Add(1) })

	con := aaplE2E()
	tr, err := ib.PlaceOrder(con, order.LimitOrder("BUY", 100, 150))
	if err != nil {
		t.Fatal(err)
	}
	filled := tr.FilledEvent.Once()
	waitStatus(t, tr, order.StatusSubmitted)
	if tr.PermID() == 0 {
		t.Fatal("permId not recorded from openOrder/orderStatus")
	}

	if err := srv.Fill(e2eClientID, tr.Order.OrderID, 40, 150.0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "first partial fill", func() bool { return tr.FilledQty() == 40 })
	waitFor(t, "status filled=40", func() bool { return tr.StatusSnapshot().Filled == 40 })
	if tr.Status() != order.StatusSubmitted || tr.RemainingQty() != 60 {
		t.Fatalf("after partial: status %s remaining %g", tr.Status(), tr.RemainingQty())
	}

	if err := srv.Fill(e2eClientID, tr.Order.OrderID, 60, 151.0); err != nil {
		t.Fatal(err)
	}
	select {
	case <-filled:
	case <-time.After(3 * time.Second):
		t.Fatalf("FilledEvent not fired, status %s", tr.Status())
	}
	waitFor(t, "commission reports", func() bool { return commEvents.Load() == 2 })

	st := tr.StatusSnapshot()
	if st.Filled != 100 || st.Remaining != 0 {
		t.Fatalf("final status %+v", st)
	}
	fills := tr.FillsSnapshot()
	if len(fills) != 2 || tr.FilledQty() != 100 {
		t.Fatalf("fills = %d, filled = %g", len(fills), tr.FilledQty())
	}
	ex := fills[0].Execution
	if ex.Shares != 40 || ex.Price != 150 || ex.Side != "BOT" || ex.PermID != tr.PermID() || ex.OrderID != tr.Order.OrderID {
		t.Fatalf("execution decoded wrong: %+v", ex)
	}
	if ex.Time.Location().String() != "America/New_York" || ex.Time.Year() < 2000 {
		t.Fatalf("execution time not parsed: %v", ex.Time)
	}
	if fills[0].CommissionReport.Commission != 1 {
		t.Fatalf("commission = %+v", fills[0].CommissionReport)
	}
	if execEvents.Load() != 2 {
		t.Fatalf("ExecDetailsEvent fired %d times, want 2", execEvents.Load())
	}

	positions, err := ib.ReqPositions(ctx5s(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 1 || positions[0].Position != 100 || positions[0].Contract.Symbol != "AAPL" {
		t.Fatalf("positions = %+v", positions)
	}
	if got := positions[0].AvgCost; got < 150.59 || got > 150.61 {
		t.Fatalf("avg cost = %g, want 150.6", got)
	}
	if positions[0].Contract.TradingClass != "NMS" {
		t.Fatalf("position contract not fully decoded: %+v", positions[0].Contract)
	}

	// The journal has the full lifecycle and replays to the same state.
	ib.SetJournal(nil)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	recs, err := journal.Load(jpath)
	if err != nil {
		t.Fatal(err)
	}
	if recs[0].Type != journal.TypeSubmit || recs[0].OrderID != tr.Order.OrderID || recs[0].ClientID != e2eClientID {
		t.Fatalf("first record must be the write-ahead submit, got %+v", recs[0])
	}
	e := journal.Replay(recs)[journal.Key{ClientID: e2eClientID, OrderID: tr.Order.OrderID}]
	if e == nil || e.Status != order.StatusFilled || e.Filled != 100 || len(e.Fills) != 2 || e.Commission != 2 {
		t.Fatalf("replayed entry = %+v", e)
	}
	if d := journal.Reconcile(journal.Replay(recs), ib.Trades(), e2eClientID); len(d) != 0 {
		t.Fatalf("journal and broker disagree: %+v", d)
	}
}

func TestE2EModifyAndCancel(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	con := aaplE2E()

	var modifyEvents atomic.Int32
	ib.OrderModifyEvent.Subscribe(func(*order.Trade) { modifyEvents.Add(1) })

	tr, err := ib.PlaceOrder(con, order.LimitOrder("BUY", 10, 100))
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, tr, order.StatusSubmitted)

	mod := *tr.Order
	mod.LmtPrice = 101
	tr2, err := ib.PlaceOrder(con, &mod)
	if err != nil {
		t.Fatal(err)
	}
	if tr2 != tr {
		t.Fatal("modification must return the existing trade")
	}
	waitFor(t, "modify acknowledged", func() bool {
		for _, l := range tr.LogSnapshot() {
			if l.Message == "Modified" {
				return true
			}
		}
		return false
	})
	if o, _ := srv.Order(e2eClientID, tr.Order.OrderID); o.LmtPrice != 101 {
		t.Fatalf("broker limit = %g, want 101", o.LmtPrice)
	}
	if modifyEvents.Load() != 1 {
		t.Fatalf("OrderModifyEvent = %d", modifyEvents.Load())
	}

	cancelled := tr.CancelledEvent.Once()
	var errCodes []int
	var mu sync.Mutex
	ib.ErrorEvent.Subscribe(func(e *IBError) {
		mu.Lock()
		errCodes = append(errCodes, e.Code)
		mu.Unlock()
	})
	got, err := ib.CancelOrder(tr.Order)
	if err != nil || got != tr {
		t.Fatalf("CancelOrder = %v, %v", got, err)
	}
	if s := tr.Status(); s != order.StatusPendingCancel && s != order.StatusCancelled {
		t.Fatalf("status right after cancel = %s", s)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatalf("CancelledEvent not fired, status %s", tr.Status())
	}
	waitFor(t, "error 202", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range errCodes {
			if c == 202 {
				return true
			}
		}
		return false
	})
	if tr.Status() != order.StatusCancelled || len(ib.OpenTrades()) != 0 {
		t.Fatalf("status %s, open trades %d", tr.Status(), len(ib.OpenTrades()))
	}

	if _, err := ib.PlaceOrder(con, tr.Order); !errors.Is(err, ErrOrderDone) {
		t.Fatalf("modifying a cancelled order: err = %v, want ErrOrderDone", err)
	}
}

func TestE2ERejectedOrderIsCancelled(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	release := make(chan struct{})
	reject := sync.OnceFunc(func() { close(release) })
	defer reject()
	srv.SetReject(func(o *fakegw.Order) string {
		if o.Qty > 500 {
			<-release
			return "size exceeds exchange limit"
		}
		return ""
	})

	var gotErr atomic.Pointer[IBError]
	ib.ErrorEvent.Subscribe(func(e *IBError) { gotErr.Store(e) })

	tr, err := ib.PlaceOrder(aaplE2E(), order.LimitOrder("BUY", 1000, 100))
	if err != nil {
		t.Fatal(err)
	}
	cancelled := tr.CancelledEvent.Once()
	reject()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatalf("rejected order not cancelled, status %s", tr.Status())
	}
	if e := gotErr.Load(); e == nil || e.Code != 201 || e.IsWarning || !strings.Contains(e.Message, "size") {
		t.Fatalf("error event = %+v", e)
	}
	logs := tr.LogSnapshot()
	if last := logs[len(logs)-1]; last.ErrorCode != 201 || last.Status != order.StatusCancelled {
		t.Fatalf("last log entry = %+v", last)
	}
	if len(ib.OpenTrades()) != 0 {
		t.Fatal("rejected order must not stay open")
	}
}

func TestE2ERiskAndJournalBlockBeforeSend(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	con := aaplE2E()

	ib.SetRiskManager(risk.New(risk.Limits{MaxOrderQuantity: 50}, ib))
	_, err := ib.PlaceOrder(con, order.LimitOrder("BUY", 100, 10))
	if v, ok := risk.IsViolation(err); !ok || v.Rule != risk.RuleOrderQuantity {
		t.Fatalf("err = %v, want order_quantity violation", err)
	}

	// A journal that cannot write blocks the order (fail closed).
	j, err := journal.Open(filepath.Join(t.TempDir(), "j.jsonl"), journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	ib.SetJournal(j)
	_, err = ib.PlaceOrder(con, order.LimitOrder("BUY", 10, 10))
	if err == nil || !errors.Is(err, journal.ErrClosed) {
		t.Fatalf("err = %v, want journal failure", err)
	}

	// Give any stray message time to arrive, then verify nothing was sent.
	if _, err := ib.ReqPositions(ctx5s(t)); err != nil {
		t.Fatal(err)
	}
	if n := srv.Received(3); n != 0 {
		t.Fatalf("broker received %d placeOrder messages, want 0", n)
	}
	if len(ib.Trades()) != 0 {
		t.Fatalf("blocked orders must not create trades, have %d", len(ib.Trades()))
	}
}

func TestE2EBracketOrder(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	rm := risk.New(risk.Limits{MaxPositionQuantity: 10}, ib)
	ib.SetRiskManager(rm)
	con := aaplE2E()

	if _, err := ib.BracketOrder("BUY", 10, 100, 95, 110); err == nil {
		t.Fatal("inverted bracket prices must be rejected")
	}
	b, err := ib.BracketOrder("BUY", 10, 100, 110, 95)
	if err != nil {
		t.Fatal(err)
	}
	trades, err := ib.PlaceBracketOrder(con, b)
	if err != nil {
		t.Fatal(err)
	}
	parent, tp, sl := trades[0], trades[1], trades[2]
	if tp.Order.ParentID != parent.Order.OrderID || sl.Order.ParentID != parent.Order.OrderID || !sl.Order.Transmit || parent.Order.Transmit {
		t.Fatal("bracket legs not linked")
	}
	waitStatus(t, parent, order.StatusSubmitted)
	waitStatus(t, tp, order.StatusPreSubmitted)
	waitStatus(t, sl, order.StatusPreSubmitted)

	if err := srv.Fill(e2eClientID, parent.Order.OrderID, 10, 100); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, parent, order.StatusFilled)
	waitStatus(t, tp, order.StatusSubmitted)
	waitStatus(t, sl, order.StatusSubmitted)

	slCancelled := sl.CancelledEvent.Once()
	if err := srv.Fill(e2eClientID, tp.Order.OrderID, 10, 110); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, tp, order.StatusFilled)
	select {
	case <-slCancelled:
	case <-time.After(3 * time.Second):
		t.Fatalf("stop-loss not cancelled, status %s", sl.Status())
	}

	// Realized P&L flowed from commission reports into the risk manager:
	// (110 - 100) * 10 - 1 commission on the closing fill.
	waitFor(t, "realized pnl", func() bool { return rm.RealizedToday() == 99 })
	positions, err := ib.ReqPositions(ctx5s(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(positions) != 0 {
		t.Fatalf("flat position must be removed, got %+v", positions)
	}
}

func TestE2EWhatIf(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	con := aaplE2E()

	o := order.LimitOrder("BUY", 10, 100)
	st, err := ib.WhatIfOrder(ctx5s(t), con, o)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := order.ParseValue(st.InitMarginChange); !ok || v != 1500.25 {
		t.Fatalf("initMarginChange = %q", st.InitMarginChange)
	}
	if st.Commission != 1.25 || st.CommissionCurrency != "USD" {
		t.Fatalf("commission = %g %s", st.Commission, st.CommissionCurrency)
	}
	if o.WhatIf || o.OrderID != 0 {
		t.Fatal("WhatIfOrder must not modify the caller's order")
	}
	if len(ib.Trades()) != 0 {
		t.Fatal("what-if must not create a trade")
	}

	o.WhatIf = true
	if _, err := ib.PlaceOrder(con, o); !errors.Is(err, ErrUseWhatIfOrder) {
		t.Fatalf("PlaceOrder(whatIf) err = %v", err)
	}
}

func TestE2EMarketOrderFillsImmediately(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	tr, err := ib.PlaceOrder(aaplE2E(), order.MarketOrder("BUY", 5))
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, tr, order.StatusFilled)
	waitFor(t, "fill", func() bool { return tr.FilledQty() == 5 })
}

func TestE2EConcurrentIdlessRequests(t *testing.T) {
	srv := startFake(t)
	srv.SetPosition(fakegw.Contract{ConID: 265598, Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD", LocalSymbol: "AAPL", TradingClass: "NMS"}, 25, 120)
	ib := connectFake(t, srv)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			pos, err := ib.ReqPositions(ctx5s(t))
			if err == nil && (len(pos) != 1 || pos[0].Position != 25) {
				err = errors.New("bad positions")
			}
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, err := ib.ReqOpenOrders(ctx5s(t))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestE2EReconnectSyncRecoversMissedFills(t *testing.T) {
	srv := startFake(t)
	ib := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	connected := make(chan struct{}, 4)
	var disconnects atomic.Int32
	ib.DisconnectedEvent.Subscribe(func(struct{}) { disconnects.Add(1) })
	cfg := ReconnectConfig{RetryDelay: 50 * time.Millisecond, SyncOrders: true, SyncTimeout: 5 * time.Second}
	loopDone := make(chan error, 1)
	go func() {
		loopDone <- ib.ConnectWithReconnect(ctx, "127.0.0.1", srv.Port(), e2eClientID, cfg, func() { connected <- struct{}{} })
	}()
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("initial connect failed")
	}

	con := aaplE2E()
	a, err := ib.PlaceOrder(con, order.LimitOrder("BUY", 10, 99))
	if err != nil {
		t.Fatal(err)
	}
	b, err := ib.PlaceOrder(con, order.LimitOrder("BUY", 5, 98))
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, a, order.StatusSubmitted)
	waitStatus(t, b, order.StatusSubmitted)
	aFilled := a.FilledEvent.Once()

	srv.DropConnection()
	waitFor(t, "disconnect", func() bool { return disconnects.Load() >= 1 })

	// Fills happen at the broker while we're disconnected.
	if err := srv.Fill(e2eClientID, a.Order.OrderID, 10, 99); err != nil {
		t.Fatal(err)
	}
	if err := srv.Fill(e2eClientID, b.Order.OrderID, 2, 98); err != nil {
		t.Fatal(err)
	}

	select {
	case <-connected: // onConnect runs after the sync
	case <-time.After(5 * time.Second):
		t.Fatal("reconnect failed")
	}

	// The same Trade objects caught up.
	if a.Status() != order.StatusFilled || a.FilledQty() != 10 {
		t.Fatalf("order A after sync: status %s filled %g", a.Status(), a.FilledQty())
	}
	select {
	case <-aFilled:
	case <-time.After(time.Second):
		t.Fatal("FilledEvent for A not fired by sync")
	}
	waitFor(t, "B partial fill", func() bool { return b.FilledQty() == 2 && b.StatusSnapshot().Filled == 2 })
	if b.Status() != order.StatusSubmitted {
		t.Fatalf("order B status %s", b.Status())
	}
	found := false
	for _, tr := range ib.Trades() {
		if tr == a {
			found = true
		}
	}
	if !found {
		t.Fatal("reconnect must keep existing Trade objects")
	}

	// A second sync is idempotent.
	rep, err := ib.SyncOrders(ctx5s(t), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.NewFills != 0 || len(rep.OpenTrades) != 1 || rep.OpenTrades[0] != b || len(rep.Unresolved) != 0 {
		t.Fatalf("second sync report = %+v", rep)
	}

	// New orders after reconnect get fresh ids.
	c, err := ib.PlaceOrder(con, order.LimitOrder("SELL", 1, 120))
	if err != nil {
		t.Fatal(err)
	}
	if c.Order.OrderID <= b.Order.OrderID {
		t.Fatalf("order id reused after reconnect: %d <= %d", c.Order.OrderID, b.Order.OrderID)
	}

	cancel()
	select {
	case <-loopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("ConnectWithReconnect did not return after cancel")
	}
}

func TestE2ERestartRecoveryWithJournal(t *testing.T) {
	srv := startFake(t)
	jpath := filepath.Join(t.TempDir(), "orders.jsonl")
	con := aaplE2E()

	// Process #1 places two orders, then dies.
	ib1 := New()
	if err := ib1.Connect(ctx5s(t), "127.0.0.1", srv.Port(), e2eClientID); err != nil {
		t.Fatal(err)
	}
	j1, err := journal.Open(jpath, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ib1.SetJournal(j1)
	filledLater, err := ib1.PlaceOrder(con, order.LimitOrder("BUY", 10, 99))
	if err != nil {
		t.Fatal(err)
	}
	stillOpen, err := ib1.PlaceOrder(con, order.LimitOrder("BUY", 3, 90))
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, filledLater, order.StatusSubmitted)
	waitStatus(t, stillOpen, order.StatusSubmitted)
	ib1.Disconnect()
	j1.Close()

	// While the process is down the first order fills.
	if err := srv.Fill(e2eClientID, filledLater.Order.OrderID, 10, 99); err != nil {
		t.Fatal(err)
	}

	// Process #2 reopens the journal and resynchronises.
	j2, err := journal.Open(jpath, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	recs, err := journal.Load(jpath)
	if err != nil {
		t.Fatal(err)
	}
	if open := journal.OpenEntries(journal.Replay(recs)); len(open) != 2 {
		t.Fatalf("journal should show 2 open orders before sync, got %d", len(open))
	}

	ib2 := connectFake(t, srv)
	ib2.SetJournal(j2)
	rep, err := ib2.SyncOrders(ctx5s(t), SyncOptions{IncludeCompleted: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.OpenTrades) != 1 || rep.OpenTrades[0].Order.OrderID != stillOpen.Order.OrderID {
		t.Fatalf("open trades after restart: %+v", rep.OpenTrades)
	}
	if rep.NewFills != 1 {
		t.Fatalf("new fills = %d, want 1", rep.NewFills)
	}

	// The missed fill was journaled during sync; journal and broker agree.
	recs, err = journal.Load(jpath)
	if err != nil {
		t.Fatal(err)
	}
	entries := journal.Replay(recs)
	if e := entries[journal.Key{ClientID: e2eClientID, OrderID: filledLater.Order.OrderID}]; e == nil || e.Filled != 10 {
		t.Fatalf("journal entry for filled order = %+v", e)
	}
	if d := journal.Reconcile(entries, ib2.Trades(), e2eClientID); len(d) != 0 {
		t.Fatalf("discrepancies after restart sync: %+v", d)
	}
}
