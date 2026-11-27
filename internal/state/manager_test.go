package state

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

func TestAppendResultAndCancelReq(t *testing.T) {
	m := NewManager()
	m.AppendResult(1, "ignored") // not started: no-op
	if m.HasResult(1) {
		t.Fatal("unknown request must not collect results")
	}

	con := contract.Stock("AAPL", "SMART", "USD")
	ch := m.StartReq(2, con)
	if !m.HasResult(2) || m.ReqID2Contract[2] != con {
		t.Fatal("StartReq must register results and contract")
	}
	m.AppendResult(2, "a")
	m.AppendResult(2, "b")
	m.EndReq(2, nil, nil)
	res := <-ch
	if items, ok := res.Value.([]interface{}); !ok || len(items) != 2 || items[1] != "b" {
		t.Fatalf("result = %#v", res.Value)
	}
	if m.HasResult(2) || m.ReqID2Contract[2] != nil {
		t.Fatal("EndReq must clean up")
	}

	// An explicit value wins over accumulated results; errors pass through.
	ch = m.StartReq(3, nil)
	m.AppendResult(3, "x")
	m.EndReq(3, "explicit", errors.New("boom"))
	if res := <-ch; res.Value != "explicit" || res.Err == nil {
		t.Fatalf("result = %+v", res)
	}

	// A non-list result is left alone by AppendResult.
	m.StartReq(4, nil)
	m.Mu.Lock()
	m.Results[4] = "scalar"
	m.Mu.Unlock()
	m.AppendResult(4, "y")
	if m.Results[4] != "scalar" {
		t.Fatal("AppendResult must not clobber a scalar result")
	}

	ch = m.StartReq(5, con)
	m.CancelReq(5)
	if _, ok := <-ch; ok {
		t.Fatal("cancelled request channel must be closed without a value")
	}
	if m.HasResult(5) || m.ReqID2Contract[5] != nil || m.Requests.Has(5) {
		t.Fatal("CancelReq must clean up")
	}
}

func TestSnapshotsAndResetSession(t *testing.T) {
	m := NewManager()
	con := contract.Stock("AAPL", "SMART", "USD")
	con.ConID = 1
	tr := &order.Trade{Contract: con, Order: order.NewOrder(), OrderStatus: &order.OrderStatus{Status: order.StatusSubmitted}}
	done := &order.Trade{Contract: con, Order: order.NewOrder(), OrderStatus: &order.OrderStatus{Status: order.StatusFilled}}
	m.Trades[OrderKey{1, 1}] = tr
	m.Trades[OrderKey{1, 2}] = done
	m.Fills["e1"] = &order.Fill{Execution: &order.Execution{ExecID: "e1", Shares: 5}}
	m.AccountValues["DU1:NetLiquidation:USD"] = &account.AccountValue{Account: "DU1", Tag: "NetLiquidation", Value: "1"}
	m.Portfolio["DU1"] = map[int64]*account.PortfolioItem{1: {Account: "DU1", Contract: con, Position: 3}}
	m.Positions["DU1"] = map[int64]*account.Position{1: {Account: "DU1", Contract: con, Position: 3}}
	tk := market.NewTicker(con)
	m.Tickers[1] = tk
	m.ReqID2Ticker[9] = tk
	m.MarkTickerPending(tk)
	m.StartReq(10, con)

	if len(m.GetTrades()) != 2 || len(m.GetOpenTrades()) != 1 || m.GetOpenTrades()[0] != tr {
		t.Fatal("trade snapshots")
	}
	fills := m.GetFills()
	fills[0].Execution = nil // copies of Fill values
	if len(m.GetFills()) != 1 || m.GetFills()[0].Execution == nil {
		t.Fatal("GetFills must return value copies")
	}
	if av := m.GetAccountValues(); len(av) != 1 || av[0].Value != "1" {
		t.Fatalf("account values = %+v", av)
	}
	if p := m.GetPortfolio("DU1"); len(p) != 1 || p[0].Position != 3 {
		t.Fatalf("portfolio = %+v", p)
	}
	if len(m.GetPortfolio("other")) != 0 || len(m.GetPositions()) != 1 {
		t.Fatal("portfolio/positions")
	}

	now := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	m.SetLastTime(now)
	if lt, ts := m.GetLastTime(); !lt.Equal(now) || ts != float64(now.UnixMilli())/1000 {
		t.Fatalf("last time = %v %v", lt, ts)
	}

	m.ResetSession(nil)
	if len(m.Trades) != 2 || len(m.Fills) != 1 || len(m.Positions) != 1 || len(m.AccountValues) != 1 {
		t.Fatal("ResetSession must keep orders, fills, positions and account values")
	}
	if len(m.Tickers) != 0 || len(m.ReqID2Ticker) != 0 || len(m.PendingTickers) != 0 || len(m.Results) != 0 || len(m.ReqID2Contract) != 0 {
		t.Fatal("ResetSession must drop market data and request state")
	}

	m.Reset()
	if len(m.Trades) != 0 || len(m.Fills) != 0 || len(m.Positions) != 0 || len(m.AccountValues) != 0 || len(m.Portfolio) != 0 {
		t.Fatal("Reset must clear everything")
	}
}

func TestPendingTickersClearTickLists(t *testing.T) {
	m := NewManager()
	tk := market.NewTicker(nil)
	tk.Ticks = append(tk.Ticks, market.TickData{Price: 1})
	m.MarkTickerPending(tk)
	m.MarkTickerPending(tk) // idempotent
	if p := m.GetPendingTickers(); len(p) != 1 || p[0] != tk {
		t.Fatalf("pending = %v", p)
	}
	m.ClearPendingTickers()
	if len(m.GetPendingTickers()) != 0 || tk.Ticks != nil {
		t.Fatal("clear must empty the set and per-batch tick lists")
	}
}

// A user holding a ticker's read lock while calling into the client (which
// takes Mu) must not deadlock with ClearPendingTickers on the reader.
func TestClearPendingTickersLockOrder(t *testing.T) {
	m := NewManager()
	tk := market.NewTicker(nil)
	m.MarkTickerPending(tk)

	tk.RLock()
	cleared := make(chan struct{})
	go func() { m.ClearPendingTickers(); close(cleared) }()
	time.Sleep(50 * time.Millisecond) // the clearer is now waiting for the ticker lock

	got := make(chan struct{})
	go func() { _ = m.GetPendingTickers(); close(got) }()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("Mu is held while waiting for a ticker lock")
	}
	tk.RUnlock()
	<-cleared
}

// Request starters and the reader goroutine touch the same maps; run with -race.
func TestManagerConcurrentUse(t *testing.T) {
	m := NewManager()
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(2)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				id := int64(g*1000 + i)
				m.StartReq(id, nil)
				m.AppendResult(id, i)
				m.EndReq(id, nil, nil)
			}
		}(g)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				m.MarkTickerPending(market.NewTicker(nil))
				_ = m.GetPendingTickers()
				m.ClearPendingTickers()
				m.SetLastTime(time.Now())
				_, _ = m.GetLastTime()
			}
		}()
	}
	wg.Wait()
}
