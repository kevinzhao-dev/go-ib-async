package risk

import (
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

type fakeState struct {
	positions []account.Position
	open      []*order.Trade
}

func (f *fakeState) Positions() []account.Position { return f.positions }
func (f *fakeState) OpenTrades() []*order.Trade    { return f.open }

func aapl() *contract.Contract {
	c := contract.Stock("AAPL", "SMART", "USD")
	c.ConID = 265598
	return c
}

func openTrade(action string, qty float64, orderID int64, parentID int64) *order.Trade {
	o := order.LimitOrder(action, qty, 100)
	o.OrderID = orderID
	o.ClientID = 1
	o.ParentID = parentID
	return &order.Trade{Contract: aapl(), Order: o, OrderStatus: &order.OrderStatus{Status: order.StatusSubmitted}}
}

func wantRule(t *testing.T, err error, rule string) {
	t.Helper()
	if rule == "" {
		if err != nil {
			t.Fatalf("expected order to pass, got %v", err)
		}
		return
	}
	v, ok := IsViolation(err)
	if !ok {
		t.Fatalf("expected %s violation, got %v", rule, err)
	}
	if v.Rule != rule {
		t.Fatalf("rule = %s (%s), want %s", v.Rule, v.Message, rule)
	}
}

func TestBasicValidation(t *testing.T) {
	m := New(Limits{}, nil)
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("HOLD", 1, 1), false), RuleInvalidOrder)
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 0, 1), false), RuleInvalidOrder)
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", -5, 1), false), RuleInvalidOrder)
	wantRule(t, m.CheckOrder(nil, order.LimitOrder("BUY", 1, 1), false), RuleInvalidOrder)
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 1, 1), false), "")
}

func TestOrderQuantityAndNotional(t *testing.T) {
	price := 200.0
	m := New(Limits{MaxOrderQuantity: 100, MaxOrderNotional: 10_000}, nil,
		WithPriceSource(func(*contract.Contract) (float64, bool) { return price, true }))
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 101, 1), false), RuleOrderQuantity)
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 60, 200), false), RuleOrderNotional) // 12,000
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 40, 200), false), "")                // 8,000
	// Market order uses the reference price.
	wantRule(t, m.CheckOrder(aapl(), order.MarketOrder("SELL", 60), false), RuleOrderNotional)

	// Without a price source a market order cannot be sized: fail closed.
	m2 := New(Limits{MaxOrderNotional: 10_000}, nil)
	wantRule(t, m2.CheckOrder(aapl(), order.MarketOrder("BUY", 1), false), RuleOrderNotional)

	// Futures multiplier counts.
	fut := contract.Future("ES", "202612", "CME")
	fut.Multiplier = "50"
	wantRule(t, m2.CheckOrder(fut, order.LimitOrder("BUY", 1, 6000), false), RuleOrderNotional) // 300,000
}

func TestPositionLimitWorstCase(t *testing.T) {
	st := &fakeState{
		positions: []account.Position{{Account: "DU1", Contract: aapl(), Position: 600}},
		open:      []*order.Trade{openTrade("BUY", 300, 5, 0)},
	}
	m := New(Limits{MaxPositionQuantity: 1000}, st)

	// 600 + 300 open + 100 = 1000: ok.
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 100, 1), false), "")
	// 600 + 300 + 200 = 1100: rejected.
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 200, 1), false), RulePosition)
	// Modifying order 5 itself up to 400: 600 + 400 = 1000 ok (own open qty excluded).
	mod := order.LimitOrder("BUY", 400, 1)
	mod.OrderID, mod.ClientID = 5, 1
	wantRule(t, m.CheckOrder(aapl(), mod, true), "")
	// Selling to reduce is always allowed even when the long side is over.
	st.positions[0].Position = 1200
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("SELL", 500, 1), false), "")
	// ...but not when it would flip through zero and beyond the limit.
	st.positions[0].Position = 100
	st.open = nil
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("SELL", 1200, 1), false), RulePosition)
}

func TestBracketChildrenNotCountedAsExposure(t *testing.T) {
	st := &fakeState{}
	m := New(Limits{MaxPositionQuantity: 100}, st)
	b, err := order.NewBracket("BUY", 100, 50, 55, 45)
	if err != nil {
		t.Fatal(err)
	}
	b.Parent.OrderID, b.TakeProfit.OrderID, b.StopLoss.OrderID = 1, 2, 3
	b.Link()
	for _, leg := range b.Orders() {
		wantRule(t, m.CheckOrder(aapl(), leg, false), "")
	}
	// With the bracket legs open, a second parent would exceed the limit.
	st.open = []*order.Trade{openTrade("BUY", 100, 1, 0), openTrade("SELL", 100, 2, 1), openTrade("SELL", 100, 3, 1)}
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 1, 50), false), RulePosition)
}

func TestOpenOrdersRateDuplicate(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	st := &fakeState{open: []*order.Trade{openTrade("BUY", 1, 1, 0), openTrade("BUY", 1, 2, 0)}}
	m := New(Limits{MaxOpenOrders: 2}, st, WithClock(clock))
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 1, 1), false), RuleOpenOrders)

	m = New(Limits{MaxOrdersPerWindow: 2, OrderWindow: time.Second}, nil, WithClock(clock))
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 1, 1), false), "")
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 2, 1), false), "")
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 3, 1), false), RuleOrderRate)
	now = now.Add(1100 * time.Millisecond)
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 3, 1), false), "")

	m = New(Limits{DuplicateWindow: 2 * time.Second}, nil, WithClock(clock))
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 10, 99), false), "")
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 10, 99), false), RuleDuplicate)
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 10, 98), false), "")
	now = now.Add(3 * time.Second)
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 10, 99), false), "")
}

func TestSymbolAndPriceCollar(t *testing.T) {
	m := New(Limits{AllowedSecTypes: []string{"STK"}, AllowedSymbols: []string{"aapl", "MSFT"}, BlockedSymbols: []string{"MSFT"}, PriceCollarPct: 5}, nil,
		WithPriceSource(func(*contract.Contract) (float64, bool) { return 100, true }))
	wantRule(t, m.CheckOrder(contract.Stock("TSLA", "SMART", "USD"), order.LimitOrder("BUY", 1, 100), false), RuleSymbol)
	wantRule(t, m.CheckOrder(contract.Stock("MSFT", "SMART", "USD"), order.LimitOrder("BUY", 1, 100), false), RuleSymbol)
	wantRule(t, m.CheckOrder(contract.Forex("EURUSD"), order.LimitOrder("BUY", 1, 1), false), RuleSecType)
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 1, 106), false), RulePriceCollar)
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 1, 104), false), "")
	wantRule(t, m.CheckOrder(aapl(), order.MarketOrder("BUY", 1), false), "")
}

func TestHaltAndDailyLoss(t *testing.T) {
	st := &fakeState{positions: []account.Position{{Contract: aapl(), Position: 100}}}
	m := New(Limits{MaxDailyLoss: 500, AllowReducingWhenHalted: true}, st)

	fill := func(id string, pnl float64) *order.Fill {
		return &order.Fill{Execution: &order.Execution{ExecID: id}, CommissionReport: &order.CommissionReport{ExecID: id, RealizedPNL: pnl}}
	}
	m.OnCommissionReport(nil, fill("a", -300))
	m.OnCommissionReport(nil, fill("a", -300)) // duplicate report ignored
	if h, _ := m.Halted(); h {
		t.Fatal("should not halt at -300")
	}
	m.OnCommissionReport(nil, fill("b", -250))
	if h, why := m.Halted(); !h {
		t.Fatal("should halt at -550")
	} else if why == "" {
		t.Fatal("halt reason missing")
	}
	if got := m.RealizedToday(); got != -550 {
		t.Fatalf("realized today = %g", got)
	}

	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 1, 1), false), RuleHalted)
	// Reducing the long position is still allowed...
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("SELL", 100, 1), false), "")
	// ...but not flipping it short.
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("SELL", 101, 1), false), RuleHalted)

	m.Resume()
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 1, 1), false), "")
	m.Halt("manual")
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("BUY", 1, 1), false), RuleHalted)
}

func TestReducingWithOpenOrdersCannotFlip(t *testing.T) {
	st := &fakeState{
		positions: []account.Position{{Contract: aapl(), Position: 100}},
		open:      []*order.Trade{openTrade("SELL", 80, 9, 0)},
	}
	m := New(Limits{MaxPositionQuantity: 50}, st)
	// 100 long, 80 already offered: another SELL 20 still ends flat → ok.
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("SELL", 20, 1), false), "")
	// SELL 90 would end at -70 beyond the limit → rejected.
	wantRule(t, m.CheckOrder(aapl(), order.LimitOrder("SELL", 90, 1), false), RulePosition)
}
