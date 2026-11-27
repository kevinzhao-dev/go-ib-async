package risk

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

func TestViolationAndLimitsAccessors(t *testing.T) {
	v := &Violation{Rule: RulePosition, Message: "too big"}
	if v.Error() != "risk: position: too big" {
		t.Fatal(v.Error())
	}
	wrapped := fmt.Errorf("ibgo: bracket leg rejected: %w", v)
	if got, ok := IsViolation(wrapped); !ok || got != v {
		t.Fatal("IsViolation must unwrap")
	}
	if _, ok := IsViolation(fmt.Errorf("other")); ok {
		t.Fatal("plain error is not a violation")
	}

	m := New(Limits{MaxOrderQuantity: 10}, nil)
	if l := m.Limits(); l.MaxOrderQuantity != 10 || l.OrderWindow != time.Second {
		t.Fatalf("limits = %+v (default window 1s)", l)
	}
	m.SetLimits(Limits{MaxOrderQuantity: 1})
	if l := m.Limits(); l.MaxOrderQuantity != 1 || l.OrderWindow != time.Second {
		t.Fatalf("SetLimits = %+v", l)
	}
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 2, 1), false), RuleOrderQuantity)
}

func TestUnqualifiedContractsMatchByFields(t *testing.T) {
	opt := func(strike float64) *contract.Contract {
		return contract.Option("AAPL", "20240119", strike, "C", "SMART") // conId 0
	}
	st := &fakeState{positions: []account.Position{
		{Account: "DU1", Contract: opt(150), Position: 8},
		{Account: "DU2", Contract: opt(150), Position: 5},
		{Account: "DU1", Contract: opt(155), Position: 50},
	}}
	m := New(Limits{MaxPositionQuantity: 10}, st)

	// DU1 + DU2 together hold 13 of the 150 call: over the limit for an account-less order.
	wantRule(t, m.CheckOrder(opt(150), order.LimitOrder("BUY", 1, 1), false), RulePosition)
	// Restricted to DU1 (8) it fits; the 155 strike is a different contract.
	o := order.LimitOrder("BUY", 2, 1)
	o.Account = "DU1"
	wantRule(t, m.CheckOrder(opt(150), o, false), "")
}

func TestAccountAllowListAndCashQty(t *testing.T) {
	m := New(Limits{AllowedAccounts: []string{"DU1"}, MaxOrderNotional: 1000}, nil)
	o := order.LimitOrder("BUY", 1, 10)
	o.Account = "U999"
	wantRule(t, m.CheckOrder(aapl(), o, false), RuleAccount)

	// Cash-quantity orders (qty 0) are valid; their notional is the cash amount.
	cash := order.MarketOrder("BUY", 0)
	cash.Account = "DU1"
	cash.CashQty = 5000
	cash.LmtPrice = 10 // price available so the notional check can run
	wantRule(t, m.CheckOrder(aapl(), cash, false), RuleOrderNotional)
	cash.CashQty = 500
	wantRule(t, m.CheckOrder(aapl(), cash, false), "")
}

func TestStopPriceAndBadMultiplier(t *testing.T) {
	m := New(Limits{MaxOrderNotional: 1000}, nil)
	// Stop orders are sized at the stop price.
	wantRule(t, m.CheckOrder(aapl(), order.StopOrder("SELL", 20, 60), false), RuleOrderNotional)
	wantRule(t, m.CheckOrder(aapl(), order.StopOrder("SELL", 10, 60), false), "")
	// An unparseable multiplier counts as 1.
	c := aapl()
	c.Multiplier = "n/a"
	wantRule(t, m.CheckOrder(c, order.LimitOrder("BUY", 10, 60), false), "")
}

func TestDailyLossResetsOnNewDay(t *testing.T) {
	day := time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)
	m := New(Limits{MaxDailyLoss: 100}, nil, WithClock(func() time.Time { return day }))
	fill := func(id string, pnl float64) *order.Fill {
		return &order.Fill{Execution: &order.Execution{ExecID: id}, CommissionReport: &order.CommissionReport{RealizedPNL: pnl}}
	}
	m.OnCommissionReport(nil, fill("a", -60))
	m.OnCommissionReport(nil, nil) // ignored
	m.OnCommissionReport(nil, &order.Fill{})
	if m.RealizedToday() != -60 {
		t.Fatalf("realized = %g", m.RealizedToday())
	}
	day = day.Add(24 * time.Hour)
	if m.RealizedToday() != 0 {
		t.Fatal("a new day starts at zero")
	}
	m.OnCommissionReport(nil, fill("b", -60))
	if h, _ := m.Halted(); h {
		t.Fatal("yesterday's loss must not count today")
	}
	m.OnCommissionReport(nil, fill("c", 1.7976931348623157e308)) // UNSET realized P&L is ignored
	if m.RealizedToday() != -60 {
		t.Fatalf("realized = %g", m.RealizedToday())
	}
}

func TestHaltReasonMentionsRule(t *testing.T) {
	m := New(Limits{}, nil)
	m.Halt("manual kill switch")
	err := m.CheckOrder(aapl(), order.LimitOrder("BUY", 1, 1), false)
	if err == nil || !strings.Contains(err.Error(), "manual kill switch") {
		t.Fatalf("err = %v", err)
	}
}

func TestLimitsRequiringBrokerState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limits Limits
		rule   string
	}{
		{"position", Limits{MaxPositionQuantity: 5}, RulePosition},
		{"open orders", Limits{MaxOpenOrders: 5}, RuleOpenOrders},
		{"order size only", Limits{MaxOrderQuantity: 5}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := order.LimitOrder("BUY", 1, 100)
			wantRule(t, New(tc.limits, &fakeState{}).CheckOrder(aapl(), o, false), "")
			wantRule(t, New(tc.limits, nil).CheckOrder(aapl(), o, false), tc.rule)
		})
	}
}

func TestCashOrderWithUnknownShareQuantity(t *testing.T) {
	st := &fakeState{positions: []account.Position{{Account: "DU1", Contract: aapl(), Position: 1}}}
	for _, tc := range []struct {
		name   string
		limits Limits
		halt   bool
		rule   string
	}{
		{"order size cap", Limits{MaxOrderQuantity: 2}, false, RuleOrderQuantity},
		{"position cap", Limits{MaxPositionQuantity: 2}, false, RulePosition},
		{"halted with reducing exemption", Limits{AllowReducingWhenHalted: true}, true, RuleHalted},
		{"unrestricted cash order", Limits{}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(tc.limits, st)
			if tc.halt {
				m.Halt("test")
			}
			cash := order.MarketOrder("SELL", 0)
			cash.Account = "DU1"
			cash.CashQty = 10_000
			wantRule(t, m.CheckOrder(aapl(), cash, false), tc.rule)
		})
	}
}
