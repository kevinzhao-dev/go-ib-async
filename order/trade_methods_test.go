package order

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
)

func newTestTrade(status string) *Trade {
	o := LimitOrder("BUY", 100, 10)
	o.OrderID, o.ClientID = 1, 2
	return &Trade{Contract: contract.Stock("AAPL", "SMART", "USD"), Order: o, OrderStatus: &OrderStatus{OrderID: 1, Status: status}}
}

func TestUpdateOrderStatusSemantics(t *testing.T) {
	tr := newTestTrade(StatusPendingSubmit)
	now := time.Now()

	old, emit := tr.UpdateOrderStatus(OrderStatus{Status: StatusSubmitted, Remaining: 100}, now)
	if old != StatusPendingSubmit || !emit || tr.Status() != StatusSubmitted || tr.StatusSnapshot().OrderID != 1 {
		t.Fatalf("first update: old=%s emit=%v snapshot=%+v", old, emit, tr.StatusSnapshot())
	}
	if _, emit := tr.UpdateOrderStatus(OrderStatus{OrderID: 1, Status: StatusSubmitted, Remaining: 100}, now); emit {
		t.Fatal("identical status must not emit")
	}

	// A pending modification is acknowledged by an unchanged "Submitted".
	tr.AppendLog(TradeLogEntry{Time: now, Status: StatusSubmitted, Message: "Modify"})
	if _, emit := tr.UpdateOrderStatus(OrderStatus{OrderID: 1, Status: StatusSubmitted, Remaining: 100}, now); !emit {
		t.Fatal("modify acknowledgement must emit")
	}
	logs := tr.LogSnapshot()
	if logs[len(logs)-1].Message != "Modified" {
		t.Fatalf("last log = %+v", logs[len(logs)-1])
	}

	// Copy-on-write: a previously obtained pointer keeps its old content.
	before := tr.OrderStatus
	tr.UpdateOrderStatus(OrderStatus{OrderID: 1, Status: StatusFilled, Filled: 100}, now)
	if before.Status != StatusSubmitted || tr.Status() != StatusFilled {
		t.Fatal("status update must not mutate the previous status object")
	}
}

func TestSetStatusApplyFinalAndAccessors(t *testing.T) {
	tr := newTestTrade(StatusSubmitted)
	if old := tr.SetStatus(StatusCancelled, time.Now(), "Error 201", 201); old != StatusSubmitted {
		t.Fatalf("old = %s", old)
	}
	l := tr.LogSnapshot()
	if l[len(l)-1].ErrorCode != 201 || !tr.IsDone() || tr.IsActive() {
		t.Fatalf("log %+v done %v", l, tr.IsDone())
	}

	tr2 := newTestTrade(StatusSubmitted)
	tr2.ApplyFinalStatus(StatusFilled, 120, time.Now(), "sync") // overfill clamps remaining at 0
	if st := tr2.StatusSnapshot(); st.Filled != 120 || st.Remaining != 0 || st.Status != StatusFilled {
		t.Fatalf("final = %+v", st)
	}
	tr3 := newTestTrade(StatusSubmitted)
	tr3.ApplyFinalStatus(StatusCancelled, -1, time.Now(), "sync") // unknown fill keeps fields
	if st := tr3.StatusSnapshot(); st.Filled != 0 || st.Status != StatusCancelled {
		t.Fatalf("final = %+v", st)
	}

	nilStatus := &Trade{Order: NewOrder()}
	if nilStatus.Status() != "" || nilStatus.StatusSnapshot() != (OrderStatus{}) {
		t.Fatal("trade without status must report empty status")
	}
	nilStatus.SetStatus(StatusInactive, time.Now(), "", 0)
	nilStatus2 := &Trade{Order: NewOrder()}
	nilStatus2.ApplyFinalStatus(StatusFilled, 1, time.Now(), "")
	if nilStatus.Status() != StatusInactive || nilStatus2.Status() != StatusFilled {
		t.Fatal("setting status on a trade without one")
	}

	tr.SetPermID(99)
	tr.SetAdvancedError(`{"x":1}`)
	if tr.PermID() != 99 || tr.AdvancedError == "" {
		t.Fatal("perm id / advanced error")
	}
	tr.UpdateFromOpenOrder(&Order{PermID: 100, TotalQuantity: 50, LmtPrice: 11, AuxPrice: 1, OrderType: "STP LMT", OrderRef: "r"})
	if o := tr.Order; o.PermID != 100 || o.TotalQuantity != 50 || o.LmtPrice != 11 || o.AuxPrice != 1 || o.OrderType != "STP LMT" || o.OrderRef != "r" || o.Action != "BUY" {
		t.Fatalf("open order update = %+v", o)
	}
}

func TestStateClassification(t *testing.T) {
	cases := []struct {
		status                         string
		waiting, working, active, done bool
	}{
		{StatusPendingSubmit, true, false, true, false},
		{StatusApiPending, true, false, true, false},
		{StatusPreSubmitted, true, false, true, false},
		{StatusSubmitted, false, true, true, false},
		{StatusValidationError, false, true, true, false},
		{StatusApiUpdate, false, true, true, false},
		{StatusPendingCancel, false, false, false, false},
		{StatusFilled, false, false, false, true},
		{StatusCancelled, false, false, false, true},
		{StatusApiCancelled, false, false, false, true},
		{StatusInactive, false, false, false, true},
	}
	for _, c := range cases {
		tr := newTestTrade(c.status)
		if tr.IsWaiting() != c.waiting || tr.IsWorking() != c.working || tr.IsActive() != c.active || tr.IsDone() != c.done {
			t.Errorf("%s: waiting=%v working=%v active=%v done=%v", c.status, tr.IsWaiting(), tr.IsWorking(), tr.IsActive(), tr.IsDone())
		}
	}
}

func TestAddFillAndSnapshotsAreCopies(t *testing.T) {
	tr := newTestTrade(StatusSubmitted)
	tr.AddFill(&Fill{Contract: tr.Contract, Execution: &Execution{ExecID: "a", Shares: 30, Price: 10.5}}, time.Now())
	tr.AddFill(&Fill{Contract: tr.Contract, Execution: &Execution{ExecID: "b", Shares: 20, Price: 10}}, time.Now())
	if tr.FilledQty() != 50 || tr.RemainingQty() != 50 {
		t.Fatalf("filled %g remaining %g", tr.FilledQty(), tr.RemainingQty())
	}
	if l := tr.LogSnapshot(); !strings.HasPrefix(l[0].Message, "Fill 30@10.5") {
		t.Fatalf("fill log = %q", l[0].Message)
	}
	fills := tr.FillsSnapshot()
	fills[0] = nil
	if tr.FillsSnapshot()[0] == nil {
		t.Fatal("FillsSnapshot must return a copy")
	}
	logs := tr.LogSnapshot()
	logs[0].Message = "x"
	if tr.LogSnapshot()[0].Message == "x" {
		t.Fatal("LogSnapshot must return a copy")
	}
}

func TestTradeConcurrentAccess(t *testing.T) {
	tr := newTestTrade(StatusSubmitted)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				tr.UpdateOrderStatus(OrderStatus{Status: StatusSubmitted, Filled: float64(j)}, time.Now())
				tr.AddFill(&Fill{Contract: tr.Contract, Execution: &Execution{Shares: 0}}, time.Now())
			}
		}(i)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = tr.Status()
				_ = tr.FilledQty()
				_ = tr.IsDone()
				_ = tr.StatusSnapshot()
				_ = tr.LogSnapshot()
			}
		}()
	}
	wg.Wait() // run with -race
}

func TestNewBracketValidation(t *testing.T) {
	bad := []struct {
		action           string
		qty, lmt, tp, sl float64
		wantErrContains  string
	}{
		{"HOLD", 1, 10, 11, 9, "BUY or SELL"},
		{"BUY", 1, 10, 9, 11, "takeProfit > limit > stopLoss"},
		{"BUY", 1, 10, 11, 10, "takeProfit > limit > stopLoss"},
		{"SELL", 1, 10, 11, 9, "takeProfit < limit < stopLoss"},
		{"BUY", 0, 10, 11, 9, "quantity"},
		{"BUY", 1, 10, 11, -1, "stopLoss must be positive"},
		{"SELL", 1, 10, -1, 11, "takeProfit must be positive"},
	}
	for _, c := range bad {
		if _, err := NewBracket(c.action, c.qty, c.lmt, c.tp, c.sl); err == nil || !strings.Contains(err.Error(), c.wantErrContains) {
			t.Errorf("NewBracket(%s %g %g/%g/%g) err = %v, want %q", c.action, c.qty, c.lmt, c.tp, c.sl, err, c.wantErrContains)
		}
	}

	b, err := NewBracket("SELL", 5, 100, 90, 110)
	if err != nil {
		t.Fatal(err)
	}
	b.Parent.OrderID, b.TakeProfit.OrderID, b.StopLoss.OrderID = 10, 11, 12
	b.TakeProfit.Transmit = true // Link must restore the transmit pattern
	b.Link()
	legs := b.Orders()
	if len(legs) != 3 || legs[0] != b.Parent || legs[2] != b.StopLoss {
		t.Fatal("Orders order")
	}
	if b.Parent.Action != "SELL" || b.TakeProfit.Action != "BUY" || b.StopLoss.Action != "BUY" {
		t.Fatal("children must be on the opposite side")
	}
	if b.TakeProfit.OrderType != "LMT" || b.TakeProfit.LmtPrice != 90 || b.StopLoss.OrderType != "STP" || b.StopLoss.AuxPrice != 110 {
		t.Fatalf("legs = %+v / %+v", b.TakeProfit, b.StopLoss)
	}
	if b.Parent.Transmit || b.TakeProfit.Transmit || !b.StopLoss.Transmit || b.TakeProfit.ParentID != 10 || b.StopLoss.ParentID != 10 {
		t.Fatal("bracket not linked")
	}
}

func TestOneCancelsAllAndParseValue(t *testing.T) {
	a, b := LimitOrder("BUY", 1, 1), LimitOrder("BUY", 1, 2)
	out := OneCancelsAll([]*Order{a, b}, "grp", 1)
	if len(out) != 2 || a.OcaGroup != "grp" || b.OcaType != 1 {
		t.Fatal("OneCancelsAll")
	}
	for in, want := range map[string]float64{"1500.25": 1500.25, "-3": -3} {
		if v, ok := ParseValue(in); !ok || v != want {
			t.Errorf("ParseValue(%q) = %g, %v", in, v, ok)
		}
	}
	for _, in := range []string{"", "abc", "1.7976931348623157E308"} {
		if _, ok := ParseValue(in); ok {
			t.Errorf("ParseValue(%q) must be unset", in)
		}
	}
	if _, ok := ParseValue("1e400"); ok {
		t.Error("overflow must not parse")
	}
}

func TestConditionAccessors(t *testing.T) {
	for _, typ := range []int{1, 3, 4, 5, 6, 7} {
		c := CreateCondition(typ)
		if c == nil || c.CondType() != typ || c.GetConjunction() != "a" {
			t.Fatalf("CreateCondition(%d) = %+v", typ, c)
		}
		c.SetConjunction("o")
		if c.GetConjunction() != "o" {
			t.Fatalf("SetConjunction on type %d", typ)
		}
	}
	if CreateCondition(2) != nil {
		t.Fatal("unknown condition type")
	}
}
