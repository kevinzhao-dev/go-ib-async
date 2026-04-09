package order

import (
	"math"
	"testing"

	"github.com/kevinzhao-dev/go-ib-async/contract"
)

func TestNewOrderDefaults(t *testing.T) {
	o := NewOrder()
	if !o.Transmit {
		t.Fatal("Transmit should default to true")
	}
	if o.OpenClose != "O" {
		t.Fatalf("OpenClose = %q, want O", o.OpenClose)
	}
	if o.ExemptCode != -1 {
		t.Fatalf("ExemptCode = %d, want -1", o.ExemptCode)
	}
	if o.LmtPrice != math.MaxFloat64 {
		t.Fatalf("LmtPrice = %g, want UNSET_DOUBLE", o.LmtPrice)
	}
	if o.MinQty != math.MaxInt32 {
		t.Fatalf("MinQty = %d, want UNSET_INTEGER", o.MinQty)
	}
}

func TestLimitOrder(t *testing.T) {
	o := LimitOrder("BUY", 100, 150.50)
	if o.OrderType != "LMT" {
		t.Fatalf("OrderType = %q, want LMT", o.OrderType)
	}
	if o.Action != "BUY" || o.TotalQuantity != 100 || o.LmtPrice != 150.50 {
		t.Fatalf("unexpected: action=%s qty=%g lmt=%g", o.Action, o.TotalQuantity, o.LmtPrice)
	}
	// Other defaults should still be set
	if !o.Transmit {
		t.Fatal("Transmit should default to true")
	}
}

func TestMarketOrder(t *testing.T) {
	o := MarketOrder("SELL", 50)
	if o.OrderType != "MKT" || o.Action != "SELL" || o.TotalQuantity != 50 {
		t.Fatalf("unexpected: %+v", o)
	}
}

func TestStopOrder(t *testing.T) {
	o := StopOrder("SELL", 100, 145.00)
	if o.OrderType != "STP" || o.AuxPrice != 145.00 {
		t.Fatalf("unexpected: type=%s aux=%g", o.OrderType, o.AuxPrice)
	}
}

func TestStopLimitOrder(t *testing.T) {
	o := StopLimitOrder("BUY", 100, 150, 148)
	if o.OrderType != "STP LMT" || o.LmtPrice != 150 || o.AuxPrice != 148 {
		t.Fatalf("unexpected: type=%s lmt=%g aux=%g", o.OrderType, o.LmtPrice, o.AuxPrice)
	}
}

func TestOrderStatusStates(t *testing.T) {
	if !DoneStates[StatusFilled] {
		t.Fatal("Filled should be in DoneStates")
	}
	if !DoneStates[StatusCancelled] {
		t.Fatal("Cancelled should be in DoneStates")
	}
	if !ActiveStates[StatusSubmitted] {
		t.Fatal("Submitted should be in ActiveStates")
	}
	if !WaitingStates[StatusPendingSubmit] {
		t.Fatal("PendingSubmit should be in WaitingStates")
	}
	if !WorkingStates[StatusSubmitted] {
		t.Fatal("Submitted should be in WorkingStates")
	}
	// Negative checks
	if DoneStates[StatusSubmitted] {
		t.Fatal("Submitted should NOT be in DoneStates")
	}
	if ActiveStates[StatusFilled] {
		t.Fatal("Filled should NOT be in ActiveStates")
	}
}

func TestOrderStatusTotal(t *testing.T) {
	s := &OrderStatus{Filled: 50, Remaining: 50}
	if s.Total() != 100 {
		t.Fatalf("Total() = %g, want 100", s.Total())
	}
}

func TestNewOrderState(t *testing.T) {
	s := NewOrderState()
	if s.Commission != math.MaxFloat64 {
		t.Fatal("Commission should be UNSET_DOUBLE")
	}
}

func TestSoftDollarTierIsSet(t *testing.T) {
	s := SoftDollarTier{}
	if s.IsSet() {
		t.Fatal("empty SoftDollarTier should not be set")
	}
	s.Name = "test"
	if !s.IsSet() {
		t.Fatal("SoftDollarTier with Name should be set")
	}
}

func TestCreateCondition(t *testing.T) {
	tests := []struct {
		condType int
		wantType int
	}{
		{1, 1},
		{3, 3},
		{4, 4},
		{5, 5},
		{6, 6},
		{7, 7},
	}
	for _, tt := range tests {
		c := CreateCondition(tt.condType)
		if c == nil {
			t.Fatalf("CreateCondition(%d) returned nil", tt.condType)
		}
		if c.CondType() != tt.wantType {
			t.Fatalf("CondType() = %d, want %d", c.CondType(), tt.wantType)
		}
		if c.GetConjunction() != "a" {
			t.Fatalf("Conjunction = %q, want 'a'", c.GetConjunction())
		}
	}
	if CreateCondition(99) != nil {
		t.Fatal("CreateCondition(99) should return nil")
	}
}

func TestTradeLifecycle(t *testing.T) {
	c := contract.Stock("AAPL", "SMART", "USD")
	o := LimitOrder("BUY", 100, 150)
	tr := NewTrade(c, o)

	if tr.IsDone() {
		t.Fatal("new trade should not be done")
	}

	tr.OrderStatus.Status = StatusPendingSubmit
	if !tr.IsWaiting() {
		t.Fatal("should be waiting")
	}
	if !tr.IsActive() {
		t.Fatal("should be active")
	}

	tr.OrderStatus.Status = StatusSubmitted
	if !tr.IsWorking() {
		t.Fatal("should be working")
	}

	tr.OrderStatus.Status = StatusFilled
	if !tr.IsDone() {
		t.Fatal("should be done")
	}
	if tr.IsActive() {
		t.Fatal("should not be active when done")
	}
}

func TestTradeFilledQty(t *testing.T) {
	c := contract.Stock("AAPL", "SMART", "USD")
	o := LimitOrder("BUY", 100, 150)
	tr := NewTrade(c, o)

	tr.Fills = append(tr.Fills, &Fill{
		Contract:  c,
		Execution: &Execution{Shares: 50},
	})
	tr.Fills = append(tr.Fills, &Fill{
		Contract:  c,
		Execution: &Execution{Shares: 30},
	})

	if tr.FilledQty() != 80 {
		t.Fatalf("FilledQty() = %g, want 80", tr.FilledQty())
	}
	if tr.RemainingQty() != 20 {
		t.Fatalf("RemainingQty() = %g, want 20", tr.RemainingQty())
	}
}

func TestTradeFilledQtyBag(t *testing.T) {
	bag := contract.Bag()
	o := LimitOrder("BUY", 10, 5)
	tr := NewTrade(bag, o)

	// BAG fill should count
	tr.Fills = append(tr.Fills, &Fill{
		Contract:  bag,
		Execution: &Execution{Shares: 5},
	})
	// Leg fill should NOT count
	legContract := contract.Stock("AAPL", "SMART", "USD")
	tr.Fills = append(tr.Fills, &Fill{
		Contract:  legContract,
		Execution: &Execution{Shares: 100},
	})

	if tr.FilledQty() != 5 {
		t.Fatalf("BAG FilledQty() = %g, want 5 (leg fills excluded)", tr.FilledQty())
	}
}
