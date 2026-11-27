package journal

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

func TestJournalStickyWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.jsonl")
	j, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if j.Path() != path || j.Err() != nil {
		t.Fatal("fresh journal")
	}
	tr := testTrade(1, 1, 0)
	j.f.Close() // simulate the disk/file going away underneath us

	if err := j.RecordSubmit(tr.Contract, tr.Order, false); err == nil {
		t.Fatal("write to a closed file must fail")
	}
	if j.Err() == nil {
		t.Fatal("write failure must be sticky")
	}
	err = j.RecordSubmit(tr.Contract, tr.Order, false)
	if err == nil || !strings.Contains(err.Error(), "previous write failed") {
		t.Fatalf("second write: %v", err)
	}
	j.Close()
	if err := j.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
}

func TestOpenErrors(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(blocker, "orders.jsonl"), Options{}); err == nil {
		t.Fatal("parent path is a file: Open must fail")
	}
	if _, err := Open(dir, Options{}); err == nil {
		t.Fatal("path is a directory: Open must fail")
	}
	if _, err := Load(filepath.Join(dir, "missing.jsonl")); err == nil {
		t.Fatal("Load of a missing file must fail")
	}
}

func TestOptionsClockAndBlankLines(t *testing.T) {
	fixed := time.Date(2026, 11, 27, 9, 30, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "orders.jsonl")
	j, err := Open(path, Options{NoSync: true, Now: func() time.Time { return fixed }})
	if err != nil {
		t.Fatal(err)
	}
	must(t, j.Append(Record{Type: TypeSubmit, ClientID: 1, OrderID: 1}))
	must(t, j.Close())
	// Blank lines (e.g. hand edits) are tolerated.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("\n   \n")
	f.Close()
	j, err = Open(path, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	must(t, j.Append(Record{Type: TypeCancelRequest, ClientID: 1, OrderID: 1}))
	j.Close()
	recs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || !recs[0].Time.Equal(fixed) || recs[1].Seq != 2 {
		t.Fatalf("records = %+v", recs)
	}
}

func TestRecordsWithoutTrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orders.jsonl")
	j, err := Open(path, Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	con := contract.Stock("MSFT", "SMART", "USD")
	con.Strike = math.Inf(1) // non-finite values must not break JSON encoding
	fill := &order.Fill{
		Contract:         con,
		Execution:        &order.Execution{ExecID: "x1", OrderID: 0, ClientID: 0, PermID: 777, Shares: 2, Price: 5},
		CommissionReport: &order.CommissionReport{ExecID: "x1", Commission: math.MaxFloat64, RealizedPNL: math.NaN()},
	}
	must(t, j.RecordFill(nil, fill)) // execution of an order this client doesn't know
	must(t, j.RecordCommission(nil, fill))
	must(t, j.RecordStatus(nil))
	must(t, j.RecordFill(nil, nil))
	must(t, j.RecordFill(nil, &order.Fill{}))
	must(t, j.RecordCommission(nil, &order.Fill{Execution: &order.Execution{}}))
	must(t, j.RecordError(nil, 1100, "connectivity lost"))
	must(t, j.RecordCancelRequest(&order.Trade{}))
	j.Close()

	recs, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 {
		t.Fatalf("records = %d, want 4 (nil inputs are skipped)", len(recs))
	}
	if recs[0].Contract == nil || recs[0].Contract.Strike != 0 || recs[0].KeyOf() != (Key{ClientID: 0, OrderID: 777}) {
		t.Fatalf("fill record = %+v", recs[0])
	}
	if recs[1].Commission.Commission != 0 || recs[1].Commission.RealizedPNL != 0 {
		t.Fatalf("sentinels must be stored as 0: %+v", recs[1].Commission)
	}
	entries := Replay(recs)
	e := entries[Key{0, 777}]
	if e == nil || e.Filled != 2 || e.Contract == nil || e.Contract.Symbol != "MSFT" {
		t.Fatalf("manual-order entry = %+v", e)
	}
	if len(entries) != 1 {
		t.Fatalf("records without an order key must be ignored, entries = %d", len(entries))
	}
	if snapshotContract(nil) != nil {
		t.Fatal("nil contract snapshot")
	}
}

func TestReplayMergesByPermIDAndOrdersEntries(t *testing.T) {
	t0 := time.Date(2026, 11, 27, 9, 30, 0, 0, time.UTC)
	recs := []Record{
		{Seq: 1, Time: t0, Type: TypeSubmit, ClientID: 2, OrderID: 5, Order: &OrderSnapshot{Action: "SELL", TotalQuantity: 4}},
		{Seq: 2, Time: t0.Add(time.Second), Type: TypeStatus, ClientID: 2, OrderID: 5, PermID: 4242, Status: &StatusSnapshot{Status: order.StatusSubmitted}},
		// A later record that only knows the permId (e.g. completed order with orderId 0).
		{Seq: 3, Time: t0.Add(2 * time.Second), Type: TypeFill, PermID: 4242, Fill: &FillSnapshot{ExecID: "f", Shares: 4, Price: 9}},
		{Seq: 4, Time: t0.Add(3 * time.Second), Type: TypeModify, ClientID: 2, OrderID: 5, Order: &OrderSnapshot{Action: "SELL", TotalQuantity: 4, OrderType: "LMT"}},
		{Seq: 5, Time: t0, Type: TypeSubmit, ClientID: 1, OrderID: 9},
		{Seq: 6, Time: t0, Type: TypeSubmit, ClientID: 2, OrderID: 1},
	}
	entries := Replay(recs)
	e := entries[Key{2, 5}]
	if len(entries) != 3 || e.Filled != 4 || e.PermID != 4242 || e.Order.OrderType != "LMT" || !e.Submitted.Equal(t0) || !e.Updated.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("entries = %+v, merged = %+v", entries, e)
	}
	open := OpenEntries(entries)
	if len(open) != 3 || open[0].Key != (Key{1, 9}) || open[1].Key != (Key{2, 1}) || open[2].Key != (Key{2, 5}) {
		t.Fatalf("open entries not sorted by (clientId, orderId): %+v", open)
	}

	// Reconcile matches by permId when the broker knows the order only by it.
	broker := &order.Trade{Contract: contract.Stock("X", "SMART", "USD"), Order: &order.Order{PermID: 4242}, OrderStatus: &order.OrderStatus{}}
	broker.AddFill(&order.Fill{Execution: &order.Execution{Shares: 4}}, t0)
	other := &order.Trade{Contract: broker.Contract, Order: &order.Order{ClientID: 3, OrderID: 1}, OrderStatus: &order.OrderStatus{}}
	d := Reconcile(entries, []*order.Trade{broker, other}, 2)
	if len(d) != 2 || d[0].Kind != MissingAtBroker || d[1].Kind != MissingAtBroker {
		t.Fatalf("discrepancies = %+v (only the two unmatched open entries; other clients ignored)", d)
	}
}

func TestReplayCommissionReportsByExecution(t *testing.T) {
	reports := []CommissionSnapshot{
		{ExecID: "first", Commission: 1, RealizedPNL: -5},
		{ExecID: "second", Commission: 2, RealizedPNL: 10},
		{ExecID: "first", Commission: 1, RealizedPNL: -5}, // repeated on a sync
		{ExecID: "first", Commission: 0.5, RealizedPNL: -4.5},
		{Commission: 0.25}, // records without an execution ID cannot be matched
		{Commission: 0.25},
	}
	var records []Record
	for _, cr := range reports {
		records = append(records, Record{Type: TypeCommission, ClientID: 1, OrderID: 42, Commission: &cr})
	}
	e := Replay(records)[Key{ClientID: 1, OrderID: 42}]
	if e == nil || e.Commission != 3 || e.RealizedPNL != 5.5 {
		t.Fatalf("replayed commission reports = %+v, want fees 3 and realized P&L 5.5", e)
	}
}
