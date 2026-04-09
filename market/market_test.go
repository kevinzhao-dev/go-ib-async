package market

import (
	"math"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
)

func TestNewTicker(t *testing.T) {
	c := contract.Stock("AAPL", "SMART", "USD")
	tk := NewTicker(c)

	if tk.Contract != c {
		t.Fatal("Contract not set")
	}
	if tk.MarketDataType != 1 {
		t.Fatalf("MarketDataType = %d, want 1", tk.MarketDataType)
	}
	if !math.IsNaN(tk.Bid) {
		t.Fatalf("Bid = %g, want NaN", tk.Bid)
	}
	if !math.IsNaN(tk.Ask) {
		t.Fatalf("Ask = %g, want NaN", tk.Ask)
	}
	if !math.IsNaN(tk.Last) {
		t.Fatalf("Last = %g, want NaN", tk.Last)
	}
	if !math.IsNaN(tk.Volume) {
		t.Fatalf("Volume = %g, want NaN", tk.Volume)
	}
}

func TestIsUnset(t *testing.T) {
	if !IsUnset(math.NaN()) {
		t.Fatal("NaN should be unset")
	}
	if IsUnset(100.0) {
		t.Fatal("100.0 should not be unset")
	}
	if IsUnset(0.0) {
		t.Fatal("0.0 should not be unset")
	}
}

func TestHasBidAsk(t *testing.T) {
	tk := NewTicker(nil)
	if tk.HasBidAsk() {
		t.Fatal("NaN ticker should not have bid/ask")
	}

	tk.Bid = 150.0
	tk.BidSize = 100
	tk.Ask = 151.0
	tk.AskSize = 200
	if !tk.HasBidAsk() {
		t.Fatal("should have valid bid/ask")
	}
}

func TestHasBidAskNegative(t *testing.T) {
	tk := NewTicker(nil)
	tk.Bid = -1
	tk.BidSize = 100
	tk.Ask = 151.0
	tk.AskSize = 200
	if tk.HasBidAsk() {
		t.Fatal("bid=-1 should not be valid")
	}
}

func TestHasBidAskZeroSize(t *testing.T) {
	tk := NewTicker(nil)
	tk.Bid = 150.0
	tk.BidSize = 0
	tk.Ask = 151.0
	tk.AskSize = 200
	if tk.HasBidAsk() {
		t.Fatal("bidSize=0 should not be valid")
	}
}

func TestMidpoint(t *testing.T) {
	tk := NewTicker(nil)
	tk.Bid = 150.0
	tk.BidSize = 100
	tk.Ask = 152.0
	tk.AskSize = 200

	mid := tk.Midpoint()
	if mid != 151.0 {
		t.Fatalf("Midpoint() = %g, want 151.0", mid)
	}
}

func TestMidpointNoData(t *testing.T) {
	tk := NewTicker(nil)
	mid := tk.Midpoint()
	if !math.IsNaN(mid) {
		t.Fatalf("Midpoint() = %g, want NaN", mid)
	}
}

func TestMarketPrice(t *testing.T) {
	tests := []struct {
		name string
		bid, ask, last float64
		bidSize, askSize float64
		want float64
	}{
		{"last within spread", 150, 152, 151, 100, 100, 151},
		{"last outside spread", 150, 152, 155, 100, 100, 151},
		{"no bid/ask", math.NaN(), math.NaN(), 149, 0, 0, 149},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tk := NewTicker(nil)
			tk.Bid = tt.bid
			tk.BidSize = tt.bidSize
			tk.Ask = tt.ask
			tk.AskSize = tt.askSize
			tk.Last = tt.last

			got := tk.MarketPrice()
			if math.IsNaN(tt.want) {
				if !math.IsNaN(got) {
					t.Fatalf("MarketPrice() = %g, want NaN", got)
				}
			} else if got != tt.want {
				t.Fatalf("MarketPrice() = %g, want %g", got, tt.want)
			}
		})
	}
}

func TestOptionComputationAdd(t *testing.T) {
	a := &OptionComputation{
		Delta:  Float64Ptr(0.5),
		Gamma:  Float64Ptr(0.01),
		Theta:  Float64Ptr(-0.05),
		Vega:   Float64Ptr(0.1),
		UndPrice: Float64Ptr(150),
	}
	b := &OptionComputation{
		Delta: Float64Ptr(0.3),
		Gamma: Float64Ptr(0.02),
		Theta: Float64Ptr(-0.03),
		Vega:  Float64Ptr(0.2),
	}

	result := a.Add(b)
	if *result.Delta != 0.8 {
		t.Fatalf("Delta = %g, want 0.8", *result.Delta)
	}
	if *result.Gamma != 0.03 {
		t.Fatalf("Gamma = %g, want 0.03", *result.Gamma)
	}
	if *result.UndPrice != 150 {
		t.Fatalf("UndPrice = %g, want 150 (from first operand)", *result.UndPrice)
	}
}

func TestOptionComputationSub(t *testing.T) {
	a := &OptionComputation{Delta: Float64Ptr(0.8)}
	b := &OptionComputation{Delta: Float64Ptr(0.3)}
	result := a.Sub(b)
	if math.Abs(*result.Delta-0.5) > 1e-10 {
		t.Fatalf("Delta = %g, want 0.5", *result.Delta)
	}
}

func TestOptionComputationMul(t *testing.T) {
	a := &OptionComputation{Delta: Float64Ptr(0.5), Vega: Float64Ptr(0.1)}
	result := a.Mul(10)
	if *result.Delta != 5.0 {
		t.Fatalf("Delta = %g, want 5.0", *result.Delta)
	}
	if *result.Vega != 1.0 {
		t.Fatalf("Vega = %g, want 1.0", *result.Vega)
	}
}

func TestOptionComputationNilValues(t *testing.T) {
	a := &OptionComputation{Delta: nil}
	b := &OptionComputation{Delta: Float64Ptr(0.5)}
	result := a.Add(b)
	// nil treated as 0
	if *result.Delta != 0.5 {
		t.Fatalf("Delta = %g, want 0.5 (nil + 0.5)", *result.Delta)
	}
}

func TestNewBar(t *testing.T) {
	now := time.Now()
	b := NewBar(now)
	if !math.IsNaN(b.Open) || !math.IsNaN(b.High) || !math.IsNaN(b.Low) || !math.IsNaN(b.Close) {
		t.Fatal("NewBar should have NaN OHLC")
	}
	if !b.Time.Equal(now) {
		t.Fatal("NewBar time not set")
	}
}
