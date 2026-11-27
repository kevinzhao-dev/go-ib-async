package market

import (
	"math"
	"sync"
	"testing"
)

func TestTickerSnapshotIsDeepCopy(t *testing.T) {
	tk := NewTicker(nil)
	tk.Lock()
	tk.Bid, tk.BidSize, tk.Ask, tk.AskSize, tk.Last = 10, 1, 10.2, 2, 10.1
	tk.Ticks = []TickData{{Price: 1}}
	tk.TickByTicks = []interface{}{TickByTickMidPoint{MidPoint: 1}}
	tk.DomBids = []DOMLevel{{Price: 9.9}}
	tk.DomAsks = []DOMLevel{{Price: 10.3}}
	tk.DomTicks = []MktDepthData{{Price: 9.9}}
	tk.FundamentalRatios = map[string]string{"PE": "20"}
	tk.Unlock()

	d := tk.Snapshot()
	tk.Lock()
	tk.Ticks[0].Price = 2
	tk.DomBids[0].Price = 1
	tk.DomAsks[0].Price = 1
	tk.DomTicks[0].Price = 1
	tk.TickByTicks[0] = nil
	tk.FundamentalRatios["PE"] = "x"
	tk.Bid = 0
	tk.Unlock()

	if d.Bid != 10 || d.Ticks[0].Price != 1 || d.DomBids[0].Price != 9.9 || d.DomAsks[0].Price != 10.3 ||
		d.DomTicks[0].Price != 9.9 || d.TickByTicks[0] == nil || d.FundamentalRatios["PE"] != "20" {
		t.Fatalf("snapshot shares state with the ticker: %+v", d)
	}
	if d.Midpoint() != 10.1 || d.MarketPrice() != 10.1 || !d.HasBidAsk() {
		t.Fatal("value helpers on the snapshot")
	}
	tk.RLock()
	b := tk.Bid
	tk.RUnlock()
	if b != 0 || !math.IsNaN(NewTicker(nil).Snapshot().Bid) {
		t.Fatal("ticker values")
	}
}

func TestTickerConcurrentAccess(t *testing.T) {
	tk := NewTicker(nil)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 1000 {
			tk.Lock()
			tk.Bid = float64(i)
			tk.Ticks = append(tk.Ticks, TickData{Price: float64(i)})
			tk.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for range 1000 {
			_ = tk.Snapshot()
		}
	}()
	wg.Wait()
}

func TestBarListSnapshots(t *testing.T) {
	bl := &BarDataList{}
	bl.Lock()
	bl.Bars = []BarData{{Close: 1}}
	bl.Unlock()
	s := bl.Snapshot()
	bl.Lock()
	bl.Bars[0].Close = 2
	bl.Unlock()
	bl.RLock()
	c := bl.Bars[0].Close
	bl.RUnlock()
	if s[0].Close != 1 || c != 2 {
		t.Fatal("bar list snapshot")
	}

	rl := &RealTimeBarList{}
	rl.Lock()
	rl.Bars = []RealTimeBar{{Close: 1}}
	rl.Unlock()
	rs := rl.Snapshot()
	rl.Lock()
	rl.Bars[0].Close = 2
	rl.Unlock()
	rl.RLock()
	rc := rl.Bars[0].Close
	rl.RUnlock()
	if rs[0].Close != 1 || rc != 2 {
		t.Fatal("real-time bar list snapshot")
	}
}
