package ibgo

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
	"github.com/kevinzhao-dev/go-ib-async/journal"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/protocol"
	"github.com/kevinzhao-dev/go-ib-async/risk"
)

// reqAllOpenOrders can return two different orders with the same orderId.
// A write on this connection addresses only the current client's orderId.
func TestForeignOrderWithSameID(t *testing.T) {
	for _, peerID := range []int{0, 12} {
		for _, operation := range []string{"modify", "cancel"} {
			t.Run(fmt.Sprintf("client%d/%s", peerID, operation), func(t *testing.T) {
				srv := startFake(t)
				peer := New()
				if err := peer.Connect(ctx5s(t), "127.0.0.1", srv.Port(), peerID); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(peer.Disconnect)
				con := aaplE2E()
				theirs, err := peer.PlaceOrder(con, order.LimitOrder("BUY", 2, 90))
				if err != nil {
					t.Fatal(err)
				}
				roundTrip(t, peer)
				peer.Disconnect()

				ib := connectFake(t, srv)
				ownOrder := order.LimitOrder("BUY", 1, 95)
				ownOrder.OrderID = theirs.Order.OrderID
				own, err := ib.PlaceOrder(con, ownOrder)
				if err != nil {
					t.Fatal(err)
				}
				all, err := ib.ReqAllOpenOrders(ctx5s(t))
				if err != nil || len(all) != 2 {
					t.Fatalf("all open orders: count %d, err %v", len(all), err)
				}
				var foreign *order.Trade
				for _, tr := range all {
					if tr.Order.ClientID == int64(peerID) {
						foreign = tr
					}
				}
				if foreign == nil || foreign.PermID() == 0 || foreign.PermID() == own.PermID() {
					t.Fatal("expected distinct broker orders sharing one API order ID")
				}

				if operation == "cancel" {
					_, err = ib.CancelOrder(foreign.Order)
				} else {
					mod := *foreign.Order
					mod.LmtPrice = 91
					_, err = ib.PlaceOrder(con, &mod)
				}
				if !errors.Is(err, ErrOrderIdentity) {
					t.Errorf("%s for client %d: got %v, want ErrOrderIdentity", operation, peerID, err)
				}
				roundTrip(t, ib)
				if n := srv.Received(protocol.MsgPlaceOrder); n != 2 {
					t.Errorf("gateway received %d placements, want only the two original orders", n)
				}
				if n := srv.Received(protocol.MsgCancelOrder); n != 0 {
					t.Errorf("gateway received %d cancels, want 0", n)
				}
				broker, ok := srv.Order(e2eClientID, ownOrder.OrderID)
				if !ok || broker.Status != order.StatusSubmitted || broker.Qty != 1 || broker.LmtPrice != 95 {
					t.Errorf("current client's broker order changed: %+v", broker)
				}
				if own.Status() != order.StatusSubmitted || foreign.Status() != order.StatusSubmitted {
					t.Errorf("local statuses: own %s, foreign %s", own.Status(), foreign.Status())
				}
				// An order constructed by this client may omit ClientID.
				if operation == "cancel" {
					_, err = ib.CancelOrder(&order.Order{OrderID: ownOrder.OrderID})
				} else {
					mod := order.LimitOrder("BUY", 1, 96)
					mod.OrderID, mod.PermID = ownOrder.OrderID, own.PermID()
					_, err = ib.PlaceOrder(con, mod)
				}
				if err != nil {
					t.Fatalf("valid %s with default ClientID: %v", operation, err)
				}
				roundTrip(t, ib)
				broker, _ = srv.Order(e2eClientID, ownOrder.OrderID)
				peerOrder, _ := srv.Order(int64(peerID), ownOrder.OrderID)
				if (operation == "cancel" && broker.Status != order.StatusCancelled) || (operation == "modify" && broker.LmtPrice != 96) ||
					peerOrder.Status != order.StatusSubmitted || peerOrder.LmtPrice != 90 {
					t.Errorf("after valid %s: own %+v, foreign %+v", operation, broker, peerOrder)
				}
			})
		}
	}
}

func TestExplicitOrderIDBeforeAcknowledgement(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	con := aaplE2E()
	first, err := ib.PlaceOrder(con, order.LimitOrder("BUY", 1, 90))
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, ib)

	// The staged order gets no openOrder callback to advance the sequence.
	staged := order.LimitOrder("BUY", 2, 91)
	staged.OrderID = first.Order.OrderID + 1
	staged.Transmit = false
	held, err := ib.PlaceOrder(con, staged)
	if err != nil {
		t.Fatal(err)
	}
	next, err := ib.PlaceOrder(con, order.LimitOrder("BUY", 3, 92))
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, ib)
	if next == held || next.Order.OrderID <= staged.OrderID {
		t.Errorf("automatic order ID %d after explicit ID %d", next.Order.OrderID, staged.OrderID)
	}
	if o, ok := srv.Order(e2eClientID, staged.OrderID); !ok || o.Qty != 2 || o.LmtPrice != 91 {
		t.Errorf("staged broker order was overwritten: %+v", o)
	}
	if n := len(ib.Trades()); n != 3 {
		t.Errorf("local trades = %d, want 3", n)
	}
}

func TestBracketWithExplicitParentID(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	con := aaplE2E()
	first, err := ib.PlaceOrder(con, order.LimitOrder("BUY", 1, 90))
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, ib)
	b, err := order.NewBracket("BUY", 2, 100, 105, 95)
	if err != nil {
		t.Fatal(err)
	}
	b.Parent.OrderID = first.Order.OrderID + 1
	if _, err := ib.PlaceBracketOrder(con, b); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, ib)
	if b.TakeProfit.OrderID <= b.Parent.OrderID || b.StopLoss.OrderID <= b.TakeProfit.OrderID {
		t.Fatalf("bracket order IDs in transmission order: %d, %d, %d", b.Parent.OrderID, b.TakeProfit.OrderID, b.StopLoss.OrderID)
	}
	for _, leg := range b.Orders() {
		if o, ok := srv.Order(e2eClientID, leg.OrderID); !ok || o.Action != leg.Action || o.ParentID != leg.ParentID {
			t.Fatalf("broker order %d: %+v", leg.OrderID, o)
		}
	}
}

func TestBracketConflictingIDsSendNothing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stopID     int64
		stopClient int64
	}{
		{"duplicate", 2001, 0},
		{"decreasing", 1999, 0},
		{"foreign stop", 2002, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFake(t)
			ib := connectFake(t, srv)
			b, err := order.NewBracket("SELL", 2, 100, 95, 105)
			if err != nil {
				t.Fatal(err)
			}
			b.Parent.OrderID, b.TakeProfit.OrderID = 2000, 2001
			b.StopLoss.OrderID, b.StopLoss.ClientID = tc.stopID, tc.stopClient
			_, err = ib.PlaceBracketOrder(aaplE2E(), b)
			roundTrip(t, ib)
			if n := srv.Received(protocol.MsgPlaceOrder); err == nil || n != 0 || len(ib.Trades()) != 0 {
				t.Fatalf("conflicting bracket: err %v, %d placements, %d local trades", err, n, len(ib.Trades()))
			}
		})
	}
}

func TestBracketRiskLimitsBeforeTransmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits risk.Limits
		rule   string
	}{
		{"three slots", risk.Limits{MaxOpenOrders: 3}, ""},
		{"two slots", risk.Limits{MaxOpenOrders: 2}, risk.RuleOpenOrders},
		{"two submissions", risk.Limits{MaxOrdersPerWindow: 2, OrderWindow: time.Hour, DuplicateWindow: time.Hour}, risk.RuleOrderRate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := startFake(t)
			ib := connectFake(t, srv)
			ib.SetRiskManager(risk.New(tc.limits, ib))
			b, err := ib.BracketOrder("BUY", 2, 100, 105, 95)
			if err != nil {
				t.Fatal(err)
			}
			con := aaplE2E()
			trades, err := ib.PlaceBracketOrder(con, b)
			roundTrip(t, ib)
			n := srv.Received(protocol.MsgPlaceOrder)
			if tc.rule == "" {
				if err != nil || n != 3 || len(trades) != 3 {
					t.Fatalf("accepted bracket: err %v, %d broker orders, %d trades", err, n, len(trades))
				}
				return
			}
			if v, ok := risk.IsViolation(err); !ok || v.Rule != tc.rule || n != 0 || len(ib.Trades()) != 0 {
				t.Fatalf("rejected bracket: err %v, %d broker orders, %d trades; want %s before every send", err, n, len(ib.Trades()), tc.rule)
			}
			// The rejected group didn't consume the duplicate or rate budget.
			if _, err := ib.PlaceOrder(con, order.LimitOrder("BUY", 2, 100)); err != nil {
				t.Fatalf("standalone parent after rejected bracket: %v", err)
			}
			roundTrip(t, ib)
			if n := srv.Received(protocol.MsgPlaceOrder); n != 1 {
				t.Fatalf("gateway received %d orders, want only the standalone parent", n)
			}
		})
	}
}

// A checker implementing only CheckOrder applies the same rule to each leg.
type noStopOrders struct{ base *risk.Manager }

func (m noStopOrders) CheckOrder(con *contract.Contract, ord *order.Order, modify bool) error {
	if ord.OrderType == "STP" {
		return &risk.Violation{Rule: "no_stop", Message: "stop orders disabled"}
	}
	return m.base.CheckOrder(con, ord, modify)
}

type twoLegLimit struct{ *risk.Manager }

func (m twoLegLimit) CheckBracketLeg(con *contract.Contract, ord *order.Order, acceptedLegs int) error {
	if acceptedLegs == 2 {
		return &risk.Violation{Rule: "two_legs", Message: "third bracket leg disabled"}
	}
	return m.Manager.CheckBracketLeg(con, ord, acceptedLegs)
}

func TestBracketHonorsCustomPerLegRisk(t *testing.T) {
	base := risk.New(risk.Limits{}, nil)
	for _, tc := range []struct {
		rule    string
		checker PreTradeChecker
	}{
		{"no_stop", noStopOrders{base}},
		{"two_legs", twoLegLimit{base}},
	} {
		t.Run(tc.rule, func(t *testing.T) {
			srv := startFake(t)
			ib := connectFake(t, srv)
			ib.SetRiskManager(tc.checker)
			b, err := ib.BracketOrder("BUY", 1, 100, 105, 95)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ib.PlaceBracketOrder(aaplE2E(), b)
			roundTrip(t, ib)
			if v, ok := risk.IsViolation(err); !ok || v.Rule != tc.rule || srv.Received(protocol.MsgPlaceOrder) != 0 {
				t.Fatalf("custom risk check: err %v, %d orders sent", err, srv.Received(protocol.MsgPlaceOrder))
			}
		})
	}
}

func TestOrderIDAfterUnsolicitedOrderStatus(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	const otherOrder = 5000
	srv.Handle(protocol.MsgReqCurrentTime, func([]string) [][]byte {
		return [][]byte{
			fakegw.Msg(protocol.InMsgOrderStatus, otherOrder, "Submitted", 0, 1, 0, 900999, 0, 0, 12, "", 0),
			fakegw.Msg(protocol.InMsgCurrentTime, 1, 1795753114),
		}
	})
	roundTrip(t, ib)
	tr, err := ib.PlaceOrder(aaplE2E(), order.LimitOrder("BUY", 1, 90))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Order.OrderID <= otherOrder {
		t.Fatalf("new order ID %d after broker reported %d", tr.Order.OrderID, otherOrder)
	}
}

func TestConcurrentOrdersRespectPositionLimit(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	ib.SetRiskManager(risk.New(risk.Limits{MaxPositionQuantity: 1.5}, ib))
	con := aaplE2E()
	const attempts = 12
	start := make(chan struct{})
	results := make(chan error, attempts)
	for range attempts {
		go func() {
			<-start
			_, err := ib.PlaceOrder(con, order.LimitOrder("BUY", 0.5, 100))
			results <- err
		}()
	}
	close(start)
	accepted := 0
	for range attempts {
		if err := <-results; err == nil {
			accepted++
		} else if v, ok := risk.IsViolation(err); !ok || v.Rule != risk.RulePosition {
			t.Errorf("unexpected rejection: %v", err)
		}
	}
	roundTrip(t, ib)
	if n := srv.Received(protocol.MsgPlaceOrder); accepted != 3 || n != 3 || len(ib.OpenTrades()) != 3 {
		t.Fatalf("accepted %d, gateway received %d, locally open %d; want 3 each", accepted, n, len(ib.OpenTrades()))
	}
}

func TestPartialFillCancelAndRepeatedSync(t *testing.T) {
	srv := startFake(t)
	con := aaplE2E()
	srv.SetPosition(fakegw.Contract{ConID: con.ConID, Symbol: con.Symbol, SecType: con.SecType}, 2, 90)
	ib := connectFake(t, srv)
	path := filepath.Join(t.TempDir(), "orders.jsonl")
	j, err := journal.Open(path, journal.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ib.SetJournal(nil)
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	ib.SetJournal(j)
	tr, err := ib.PlaceOrder(con, order.LimitOrder("SELL", 2.5, 100))
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, ib)
	id := tr.Order.OrderID
	if err := srv.Fill(e2eClientID, id, 0.25, 100); err != nil {
		t.Fatal(err)
	}
	if err := srv.Fill(e2eClientID, id, 0.75, 102); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, ib)
	if _, err := ib.CancelOrder(tr.Order); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, ib)
	for range 2 {
		report, err := ib.SyncOrders(ctx5s(t), SyncOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if report.NewFills != 0 || len(report.Positions) != 1 || report.Positions[0].Position != 1 {
			t.Fatalf("unexpected sync report: %+v", report)
		}
	}
	if st := tr.StatusSnapshot(); st.Status != order.StatusCancelled || st.Filled != 1 || st.Remaining != 1.5 || st.AvgFillPrice != 101.5 {
		t.Errorf("order status after cancel and sync: %+v", st)
	}
	if len(ib.Fills()) != 2 || tr.FilledQty() != 1 || len(ib.OpenTrades()) != 0 {
		t.Errorf("fills %d, filled %g, open trades %d", len(ib.Fills()), tr.FilledQty(), len(ib.OpenTrades()))
	}
	records, err := journal.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	e := journal.Replay(records)[journal.Key{ClientID: e2eClientID, OrderID: id}]
	if e == nil || e.Filled != 1 || e.AvgFillPrice != 101.5 || len(e.Fills) != 2 || !e.CancelRequested || e.Status != order.StatusCancelled {
		t.Fatalf("replayed order after partial fills and cancel: %+v", e)
	}
	// Gross P&L is 11.5. The gateway reports it net of the two $1 commissions.
	if e.Commission != 2 || e.RealizedPNL != 9.5 {
		t.Errorf("after repeated sync: commission %g, realized P&L %g; want 2 and 9.5", e.Commission, e.RealizedPNL)
	}
}

func TestFillSnapshotBeforeCommission(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	tr, err := ib.PlaceOrder(aaplE2E(), order.LimitOrder("BUY", 1, 100))
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, ib)
	snapshots := make(chan []*order.Fill, 1)
	tr.FillEvent.Subscribe(func(*order.Fill) { snapshots <- tr.FillsSnapshot() })
	if err := srv.Fill(e2eClientID, tr.Order.OrderID, 1, 100); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, ib) // both the execution and its later commission were processed
	var before []*order.Fill
	select {
	case before = <-snapshots:
	default:
		t.Fatal("no fill event")
	}
	after := tr.FillsSnapshot()
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("fill counts before and after commission: %d, %d", len(before), len(after))
	}
	if before[0].CommissionReport.ExecID != "" || after[0].CommissionReport.Commission != 1 ||
		after[0].CommissionReport.ExecID != before[0].Execution.ExecID {
		t.Fatalf("commission changed a published snapshot: before %+v, after %+v", before[0].CommissionReport, after[0].CommissionReport)
	}
}

func TestFillWhileCancelIsPending(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	tr, err := ib.PlaceOrder(aaplE2E(), order.LimitOrder("BUY", 2, 100))
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, ib)
	cancelled := tr.CancelledEvent.Once()
	srv.Handle(protocol.MsgCancelOrder, func([]string) [][]byte { return nil })
	if _, err := ib.CancelOrder(tr.Order); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, ib)
	if tr.Status() != order.StatusPendingCancel || len(ib.OpenTrades()) != 1 {
		t.Fatalf("before broker confirmation: status %s, open trades %d", tr.Status(), len(ib.OpenTrades()))
	}
	if err := srv.Fill(e2eClientID, tr.Order.OrderID, 2, 100); err != nil {
		t.Fatal(err)
	}
	srv.Handle(protocol.MsgReqCurrentTime, func([]string) [][]byte {
		return [][]byte{
			fakegw.Msg(protocol.InMsgErrMsg, 2, tr.Order.OrderID, 10148, "Order cannot be cancelled, state: Filled.", ""),
			fakegw.Msg(protocol.InMsgCurrentTime, 1, 1795753114),
		}
	})
	roundTrip(t, ib)
	if tr.Status() != order.StatusFilled || tr.FilledQty() != 2 || len(ib.OpenTrades()) != 0 {
		t.Errorf("after fill: status %s, filled %g, open trades %d", tr.Status(), tr.FilledQty(), len(ib.OpenTrades()))
	}
	select {
	case <-cancelled:
		t.Error("cancelled event for an order that filled")
	default:
	}
}

func TestPositionsAcrossInterruptedReconnectDownload(t *testing.T) {
	srv := startFake(t)
	ib := New()
	connected := startReconnecting(t, srv, ib)
	position := func(acct string, qty float64) []byte {
		return fakegw.Msg(protocol.InMsgPosition, 3, acct, 265598, "AAPL", "STK", "", 0, "", "",
			"SMART", "USD", "AAPL", "NMS", qty, 90)
	}
	end := fakegw.Msg(protocol.InMsgPositionEnd, 1)
	srv.Handle(protocol.MsgReqPositions, func([]string) [][]byte {
		return [][]byte{position("DU1", 4), position("DU2", -2), end}
	})
	if pos, err := ib.ReqPositions(ctx5s(t)); err != nil || len(pos) != 2 {
		t.Fatalf("initial positions = %+v, err %v", pos, err)
	}

	srv.DropConnection()
	waitConnected(t, connected)
	srv.Handle(protocol.MsgReqPositions, func([]string) [][]byte {
		return [][]byte{position("DU2", -1.5)} // connection drops before positionEnd
	})
	seen := ib.PositionEvent.Once()
	ctx := ctx5s(t)
	pending := make(chan error, 1)
	go func() {
		_, err := ib.ReqPositions(ctx)
		pending <- err
	}()
	select {
	case <-seen:
	case <-ctx.Done():
		t.Fatal("no position row in partial download")
	}
	srv.DropConnection()
	if err := <-pending; !errors.Is(err, ErrDisconnected) {
		t.Fatalf("interrupted positions request: %v", err)
	}
	waitConnected(t, connected)
	if pos := ib.Positions(); len(pos) != 2 {
		t.Fatalf("incomplete download removed a cached position: %+v", pos)
	}

	// DU1 closed its AAPL position while disconnected. A fresh complete
	// snapshot only mentions DU2 even though both held the same conId.
	srv.Handle(protocol.MsgReqPositions, func([]string) [][]byte {
		return [][]byte{position("DU2", -1.5), end}
	})
	closed := make(chan string, 2)
	ib.PositionEvent.Subscribe(func(p *account.Position) {
		if p.Position == 0 {
			closed <- p.Account
		}
	})
	rep, err := ib.SyncOrders(ctx5s(t), SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pos := rep.Positions; len(pos) != 1 || pos[0].Account != "DU2" || pos[0].Position != -1.5 {
		t.Errorf("positions after complete sync: %+v", pos)
	}
	if pos := ib.Positions(); len(pos) != 1 || pos[0].Account != "DU2" || pos[0].Position != -1.5 {
		t.Errorf("cached positions after complete sync: %+v", pos)
	}
	select {
	case acct := <-closed:
		if acct != "DU1" {
			t.Errorf("removed position in account %s, want DU1", acct)
		}
	default:
		t.Error("position subscribers missed the offline close")
	}
}

func TestCancelledSnapshotIgnoresLateTicksOnSharedTicker(t *testing.T) {
	srv := startFake(t)
	ids := make(chan string, 2)
	srv.Handle(protocol.MsgReqMktData, func(req []string) [][]byte {
		ids <- req[2]
		return nil
	})
	ib := connectFake(t, srv)
	con := aaplE2E()
	stream, err := ib.ReqMktData(con, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	deadline := ctx5s(t)
	nextID := func() string {
		t.Helper()
		select {
		case id := <-ids:
			return id
		case <-deadline.Done():
			t.Fatal("gateway did not receive market data request")
			return ""
		}
	}
	streamID := nextID()
	ctx, cancel := context.WithCancel(deadline)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		tickers, err := ib.ReqTickers(ctx, con)
		if len(tickers) != 1 || tickers[0] != stream {
			t.Error("snapshot lost the contract's streaming ticker")
		}
		done <- err
	}()
	snapshotID := nextID()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot returned %v", err)
	}
	srv.Handle(protocol.MsgReqCurrentTime, func([]string) [][]byte {
		return [][]byte{
			fakegw.Msg(protocol.InMsgTickPrice, 6, streamID, 1, 101, 1, 0),
			fakegw.Msg(protocol.InMsgTickPrice, 6, snapshotID, 1, 99, 1, 0),
			fakegw.Msg(protocol.InMsgTickSnapshotEnd, 1, snapshotID),
			fakegw.Msg(protocol.InMsgCurrentTime, 1, 1795753114),
		}
	})
	roundTrip(t, ib)
	if bid := stream.Snapshot().Bid; bid != 101 {
		t.Fatalf("stream bid after late snapshot tick = %g, want 101", bid)
	}
	ib.CancelMktData(con)
	roundTrip(t, ib)
	if req := srv.LastRequest(protocol.MsgCancelMktData); len(req) < 3 || req[2] != streamID {
		t.Fatalf("cancel of surviving stream = %v, want ID %s", req, streamID)
	}
}
