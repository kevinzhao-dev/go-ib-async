package account

import (
	"math"
	"testing"

	"github.com/kevinzhao-dev/go-ib-async/contract"
)

func TestAccountValueFields(t *testing.T) {
	av := AccountValue{
		Account:  "DU123456",
		Tag:      "NetLiquidation",
		Value:    "100000",
		Currency: "USD",
	}
	if av.Account != "DU123456" || av.Tag != "NetLiquidation" {
		t.Fatalf("unexpected: %+v", av)
	}
}

func TestPosition(t *testing.T) {
	c := contract.Stock("AAPL", "SMART", "USD")
	p := Position{
		Account:  "DU123456",
		Contract: c,
		Position: 100,
		AvgCost:  150.50,
	}
	if p.Position != 100 || p.AvgCost != 150.50 {
		t.Fatalf("unexpected: %+v", p)
	}
}

func TestPortfolioItem(t *testing.T) {
	c := contract.Stock("AAPL", "SMART", "USD")
	pi := PortfolioItem{
		Contract:      c,
		Position:      100,
		MarketPrice:   155.0,
		MarketValue:   15500.0,
		AverageCost:   150.0,
		UnrealizedPNL: 500.0,
		RealizedPNL:   0,
		Account:       "DU123456",
	}
	if pi.UnrealizedPNL != 500 {
		t.Fatalf("UnrealizedPNL = %g, want 500", pi.UnrealizedPNL)
	}
}

func TestPnL(t *testing.T) {
	pnl := PnL{
		Account:       "DU123456",
		DailyPnL:      math.NaN(),
		UnrealizedPnL: math.NaN(),
		RealizedPnL:   math.NaN(),
	}
	if !math.IsNaN(pnl.DailyPnL) {
		t.Fatal("DailyPnL should be NaN")
	}
}

func TestNewScannerSubscription(t *testing.T) {
	s := NewScannerSubscription()
	if s.NumberOfRows != -1 {
		t.Fatalf("NumberOfRows = %d, want -1", s.NumberOfRows)
	}
}

func TestConnectionStats(t *testing.T) {
	cs := ConnectionStats{
		NumBytesRecv: 1024,
		NumBytesSent: 512,
		NumMsgRecv:   10,
		NumMsgSent:   5,
	}
	if cs.NumMsgRecv != 10 {
		t.Fatalf("NumMsgRecv = %d, want 10", cs.NumMsgRecv)
	}
}
