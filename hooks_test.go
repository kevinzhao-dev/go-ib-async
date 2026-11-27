package ibgo

import (
	"testing"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/risk"
)

var _ BracketChecker = (*risk.Manager)(nil)

func TestHookGetters(t *testing.T) {
	ib := New()
	if ib.RiskManager() != nil || ib.Journal() != nil {
		t.Fatal("no hooks installed yet")
	}
	rm := risk.New(risk.Limits{}, ib)
	ib.SetRiskManager(rm)
	if ib.RiskManager() != PreTradeChecker(rm) {
		t.Fatal("RiskManager must return the installed checker")
	}
}

// countingChecker is a PreTradeChecker without CheckBracketLeg.
type countingChecker struct{ n int }

func (c *countingChecker) CheckOrder(*contract.Contract, *order.Order, bool) error { c.n++; return nil }

// The open-order limit must count the legs of the bracket being checked.
func TestBracketCountsEarlierLegs(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	ib.SetRiskManager(risk.New(risk.Limits{MaxOpenOrders: 2}, ib))
	b, err := ib.BracketOrder("BUY", 1, 150, 160, 140)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ib.PlaceBracketOrder(aaplE2E(), b); err == nil {
		t.Fatal("a bracket needs 3 open-order slots, the limit is 2")
	} else if v, ok := risk.IsViolation(err); !ok || v.Rule != risk.RuleOpenOrders {
		t.Fatalf("err = %v", err)
	}
	if srv.Received(3) != 0 {
		t.Fatal("no leg may be sent")
	}

	// Checkers without CheckBracketLeg still see every leg.
	cc := &countingChecker{}
	ib.SetRiskManager(cc)
	b, _ = ib.BracketOrder("BUY", 1, 150, 160, 140)
	if _, err := ib.PlaceBracketOrder(aaplE2E(), b); err != nil || cc.n != 3 {
		t.Fatalf("err = %v, checks = %d", err, cc.n)
	}
}

func TestTradeFillsCarryCommission(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	if ib.TradeFills(nil) != nil {
		t.Fatal("nil trade")
	}
	tr, err := ib.PlaceOrder(aaplE2E(), order.MarketOrder("BUY", 2))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "commission", func() bool {
		f := ib.TradeFills(tr)
		return len(f) == 1 && f[0].CommissionReport != nil && f[0].CommissionReport.Commission == 1
	})
	f := ib.TradeFills(tr)
	f[0].Time = f[0].Time.AddDate(1, 0, 0)
	if tr.FillsSnapshot()[0].Time.Equal(f[0].Time) {
		t.Fatal("TradeFills must return copies")
	}
}
