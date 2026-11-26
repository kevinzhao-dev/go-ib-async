package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

func testTrade(clientID, orderID, permID int64) *order.Trade {
	o := order.LimitOrder("BUY", 100, 150)
	o.ClientID = clientID
	o.OrderID = orderID
	o.PermID = permID
	return &order.Trade{
		Contract:    contract.Stock("AAPL", "SMART", "USD"),
		Order:       o,
		OrderStatus: &order.OrderStatus{OrderID: orderID, ClientID: clientID, Status: order.StatusPendingSubmit},
	}
}

func testFill(execID string, shares, price float64, t *order.Trade) *order.Fill {
	return &order.Fill{
		Contract: t.Contract,
		Execution: &order.Execution{
			ExecID: execID, Shares: shares, Price: price, Side: "BOT",
			OrderID: t.Order.OrderID, ClientID: t.Order.ClientID, PermID: t.Order.PermID,
		},
		CommissionReport: &order.CommissionReport{ExecID: execID, Commission: 1.0, Currency: "USD", RealizedPNL: 0},
	}
}

func TestJournalLifecycleReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "orders.jsonl")
	j, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	tr := testTrade(1, 42, 0)
	if err := j.RecordSubmit(tr.Contract, tr.Order, false); err != nil {
		t.Fatal(err)
	}
	tr.Order.PermID = 9001
	tr.UpdateOrderStatus(order.OrderStatus{OrderID: 42, ClientID: 1, PermID: 9001, Status: order.StatusSubmitted, Remaining: 100}, time.Now())
	must(t, j.RecordStatus(tr))
	f1 := testFill("e1", 40, 150, tr)
	f2 := testFill("e2", 60, 151, tr)
	must(t, j.RecordFill(tr, f1))
	must(t, j.RecordFill(tr, f1)) // duplicate execId must not double count
	must(t, j.RecordFill(tr, f2))
	must(t, j.RecordCommission(tr, f1))
	must(t, j.RecordCommission(tr, f2))
	tr.UpdateOrderStatus(order.OrderStatus{OrderID: 42, ClientID: 1, PermID: 9001, Status: order.StatusFilled, Filled: 100, AvgFillPrice: 150.6}, time.Now())
	must(t, j.RecordStatus(tr))
	must(t, j.Close())

	recs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 8 {
		t.Fatalf("records = %d, want 8", len(recs))
	}
	for i, r := range recs {
		if r.Seq != int64(i+1) {
			t.Fatalf("record %d seq = %d", i, r.Seq)
		}
	}
	if recs[0].Type != TypeSubmit || recs[0].Order == nil || *recs[0].Order.LmtPrice != 150 {
		t.Fatalf("bad submit record: %+v", recs[0])
	}

	entries := Replay(recs)
	e := entries[Key{ClientID: 1, OrderID: 42}]
	if e == nil {
		t.Fatalf("entry missing: %v", entries)
	}
	if e.Status != order.StatusFilled || !e.IsDone() {
		t.Fatalf("status = %s", e.Status)
	}
	if e.Filled != 100 || len(e.Fills) != 2 {
		t.Fatalf("filled = %g (%d fills), want 100 (2)", e.Filled, len(e.Fills))
	}
	if diff := e.AvgFillPrice - 150.6; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("avg = %g, want 150.6", e.AvgFillPrice)
	}
	if e.Commission != 2 || e.PermID != 9001 {
		t.Fatalf("commission = %g permId = %d", e.Commission, e.PermID)
	}
	if len(OpenEntries(entries)) != 0 {
		t.Fatal("no open entries expected")
	}
}

func TestJournalReopenContinuesSeqAndTruncatesPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.jsonl")
	j, err := Open(path, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	tr := testTrade(1, 7, 0)
	must(t, j.RecordSubmit(tr.Contract, tr.Order, false))
	must(t, j.RecordCancelRequest(tr))
	must(t, j.Close())

	// Simulate a crash in the middle of writing a record.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"seq":3,"type":"sta`)
	f.Close()

	if recs, err := Load(path); err != nil || len(recs) != 2 {
		t.Fatalf("Load with partial tail: %d records, err=%v", len(recs), err)
	}

	j, err = Open(path, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	must(t, j.RecordError(tr, 201, "rejected"))
	must(t, j.Close())

	recs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[2].Seq != 3 || recs[2].Type != TypeError {
		t.Fatalf("after reopen: %+v", recs)
	}
	e := Replay(recs)[Key{ClientID: 1, OrderID: 7}]
	if !e.CancelRequested || e.LastErrorCode != 201 || e.IsDone() {
		t.Fatalf("entry = %+v", e)
	}
	if open := OpenEntries(Replay(recs)); len(open) != 1 {
		t.Fatalf("open entries = %d, want 1", len(open))
	}
}

func TestJournalCorruptMiddleLineRefusesToOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.jsonl")
	content := `{"seq":1,"type":"submit","clientId":1,"orderId":1}` + "\n" +
		`garbage` + "\n" +
		`{"seq":3,"type":"status","clientId":1,"orderId":1}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("Load err = %v, want corrupt line 2", err)
	}
	if _, err := Open(path, Options{}); err == nil {
		t.Fatal("Open must refuse a journal with a corrupt record")
	}
}

func TestJournalClosedAndUnsetPrices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.jsonl")
	j, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	mkt := order.MarketOrder("SELL", 5) // LmtPrice/AuxPrice are UNSET (MaxFloat64)
	mkt.OrderID = 3
	mkt.ClientID = 1
	must(t, j.RecordSubmit(contract.Stock("MSFT", "SMART", "USD"), mkt, false))
	must(t, j.Close())
	if err := j.RecordSubmit(contract.Stock("MSFT", "SMART", "USD"), mkt, false); err != ErrClosed {
		t.Fatalf("write after close: err = %v, want ErrClosed", err)
	}
	recs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if recs[0].Order.LmtPrice != nil || recs[0].Order.AuxPrice != nil {
		t.Fatalf("unset prices must be null, got %+v", recs[0].Order)
	}
}

func TestReconcile(t *testing.T) {
	recs := []Record{
		{Seq: 1, Type: TypeSubmit, ClientID: 1, OrderID: 10, Order: &OrderSnapshot{Action: "BUY", TotalQuantity: 10}},
		{Seq: 2, Type: TypeSubmit, ClientID: 1, OrderID: 11, Order: &OrderSnapshot{Action: "BUY", TotalQuantity: 10}},
		{Seq: 3, Type: TypeFill, ClientID: 1, OrderID: 11, Fill: &FillSnapshot{ExecID: "x", Shares: 4, Price: 1}},
	}
	entries := Replay(recs)

	broker11 := testTrade(1, 11, 0)
	broker11.AddFill(&order.Fill{Contract: broker11.Contract, Execution: &order.Execution{ExecID: "x", Shares: 4}}, time.Now())
	broker11.AddFill(&order.Fill{Contract: broker11.Contract, Execution: &order.Execution{ExecID: "y", Shares: 6}}, time.Now())
	broker12 := testTrade(1, 12, 0)

	d := Reconcile(entries, []*order.Trade{broker11, broker12}, 1)
	kinds := map[string]Key{}
	for _, x := range d {
		kinds[x.Kind] = x.Key
	}
	if len(d) != 3 {
		t.Fatalf("discrepancies = %+v", d)
	}
	if kinds[MissingAtBroker] != (Key{1, 10}) || kinds[FillMismatch] != (Key{1, 11}) || kinds[UnknownToJournal] != (Key{1, 12}) {
		t.Fatalf("unexpected discrepancies: %+v", kinds)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
