package ibgo

import (
	"math"
	"testing"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/protocol"
)

func setupTickerIB() (*IB, *market.Ticker) {
	ib := New()
	ib.client.ServerVersion = 178

	c := contract.Stock("AAPL", "SMART", "USD")
	c.ConID = 265598
	ticker := market.NewTicker(c)

	ib.state.ReqID2Ticker[1] = ticker
	return ib, ticker
}

func TestTickStringTimestamp(t *testing.T) {
	ib, ticker := setupTickerIB()

	// tickType 45 = lastTimestamp
	fields := []string{"46", "1", "1", "45", "1704067200"}
	r := protocol.NewFieldReader(fields)
	r.Skip(1)
	ib.handleTickString(r)

	if ticker.LastTimestamp.IsZero() {
		t.Fatal("LastTimestamp should be set")
	}
	if ticker.LastTimestamp.Unix() != 1704067200 {
		t.Fatalf("LastTimestamp = %v, want 1704067200", ticker.LastTimestamp.Unix())
	}
}

func TestTickStringTimestampZero(t *testing.T) {
	ib, ticker := setupTickerIB()

	// tickType 45 with value "0" should NOT set timestamp
	fields := []string{"46", "1", "1", "45", "0"}
	r := protocol.NewFieldReader(fields)
	r.Skip(1)
	ib.handleTickString(r)

	if !ticker.LastTimestamp.IsZero() {
		t.Fatal("LastTimestamp should remain zero for value '0'")
	}
}

func TestTickStringRTVolume(t *testing.T) {
	ib, ticker := setupTickerIB()

	// tickType 48 = RT Volume
	// Format: price;size;ms;volume;vwap;single
	fields := []string{"46", "1", "1", "48", "150.50;100;1704067200000;50000;150.25;true"}
	r := protocol.NewFieldReader(fields)
	r.Skip(1)
	ib.handleTickString(r)

	if ticker.Last != 150.50 {
		t.Fatalf("Last = %g, want 150.50", ticker.Last)
	}
	if ticker.LastSize != 100 {
		t.Fatalf("LastSize = %g, want 100", ticker.LastSize)
	}
	if ticker.RTVolume != 50000 {
		t.Fatalf("RTVolume = %g, want 50000", ticker.RTVolume)
	}
	if ticker.VWAP != 150.25 {
		t.Fatalf("VWAP = %g, want 150.25", ticker.VWAP)
	}
	if ticker.RTTime.IsZero() {
		t.Fatal("RTTime should be set")
	}
	if len(ticker.Ticks) != 1 {
		t.Fatalf("should have 1 tick, got %d", len(ticker.Ticks))
	}
}

func TestTickStringDividends(t *testing.T) {
	ib, ticker := setupTickerIB()

	// tickType 59 = dividends
	fields := []string{"46", "1", "1", "59", "0.83,0.92,20260219,0.23"}
	r := protocol.NewFieldReader(fields)
	r.Skip(1)
	ib.handleTickString(r)

	if ticker.Dividends == nil {
		t.Fatal("Dividends should be set")
	}
	if *ticker.Dividends.Past12Months != 0.83 {
		t.Fatalf("Past12Months = %g, want 0.83", *ticker.Dividends.Past12Months)
	}
	if *ticker.Dividends.Next12Months != 0.92 {
		t.Fatalf("Next12Months = %g, want 0.92", *ticker.Dividends.Next12Months)
	}
	if ticker.Dividends.NextDate != "20260219" {
		t.Fatalf("NextDate = %q, want 20260219", ticker.Dividends.NextDate)
	}
	if *ticker.Dividends.NextAmount != 0.23 {
		t.Fatalf("NextAmount = %g, want 0.23", *ticker.Dividends.NextAmount)
	}
}

func TestTickStringFundamentalRatios(t *testing.T) {
	ib, ticker := setupTickerIB()

	fields := []string{"46", "1", "1", "47", "PE=25.5;EPS=6.12;YIELD=0.55"}
	r := protocol.NewFieldReader(fields)
	r.Skip(1)
	ib.handleTickString(r)

	if ticker.FundamentalRatios == nil {
		t.Fatal("FundamentalRatios should be set")
	}
	if ticker.FundamentalRatios["PE"] != "25.5" {
		t.Fatalf("PE = %q, want 25.5", ticker.FundamentalRatios["PE"])
	}
	if ticker.FundamentalRatios["EPS"] != "6.12" {
		t.Fatalf("EPS = %q, want 6.12", ticker.FundamentalRatios["EPS"])
	}
}

func TestTickStringSimpleString(t *testing.T) {
	ib, ticker := setupTickerIB()

	// tickType 84 = lastExchange
	fields := []string{"46", "1", "1", "84", "NASDAQ"}
	r := protocol.NewFieldReader(fields)
	r.Skip(1)
	ib.handleTickString(r)

	if ticker.LastExchange != "NASDAQ" {
		t.Fatalf("LastExchange = %q, want NASDAQ", ticker.LastExchange)
	}
}

func TestPendingTickersEvent(t *testing.T) {
	ib, _ := setupTickerIB()

	var received []*market.Ticker
	ib.PendingTickersEvent.Subscribe(func(tickers []*market.Ticker) {
		received = tickers
	})

	// Simulate a tick arriving - the OnDataProcessed hook should fire PendingTickersEvent
	// We need to mark ticker as pending and call the hook manually
	ticker := ib.state.ReqID2Ticker[1]
	ib.state.MarkTickerPending(ticker)

	// Simulate packet lifecycle: DataArrived sets time, DataProcessed emits events
	ib.client.OnDataArrived()
	ib.state.MarkTickerPending(ticker) // re-mark after clear
	ib.client.OnDataProcessed()

	if len(received) != 1 {
		t.Fatalf("expected 1 pending ticker, got %d", len(received))
	}
	if received[0] != ticker {
		t.Fatal("wrong ticker in PendingTickersEvent")
	}

	// Verify time was set
	if received[0].Time.IsZero() {
		t.Fatal("ticker.Time should be set by OnDataProcessed")
	}

	// After processing, pending should be cleared
	ib.client.OnDataArrived() // simulates next packet
	if len(ib.state.PendingTickers) != 0 {
		t.Fatalf("PendingTickers should be cleared, got %d", len(ib.state.PendingTickers))
	}
}

func TestUpdateEventEmitted(t *testing.T) {
	ib := New()

	updateCount := 0
	ib.UpdateEvent.Subscribe(func(struct{}) {
		updateCount++
	})

	ib.client.OnDataProcessed()
	ib.client.OnDataProcessed()

	if updateCount != 2 {
		t.Fatalf("UpdateEvent should fire on each DataProcessed, got %d", updateCount)
	}
}

// Ensure NaN ticker fields don't cause issues in HasBidAsk
func TestTickerNaNSafety(t *testing.T) {
	tk := market.NewTicker(nil)
	if tk.HasBidAsk() {
		t.Fatal("NaN ticker should not have bid/ask")
	}
	mid := tk.Midpoint()
	if !math.IsNaN(mid) {
		t.Fatal("NaN ticker midpoint should be NaN")
	}
}
