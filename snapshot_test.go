package ibgo

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
)

func TestReqTickerSnapshot(t *testing.T) {
	srv := startFake(t)
	srv.Handle(1, func(req []string) [][]byte {
		id := req[2]
		return [][]byte{
			fakegw.Msg(58, 1, id, 3), // marketDataType: delayed
			fakegw.Msg(1, 6, id, 66, 150.0, 100.0, 0),
			fakegw.Msg(1, 6, id, 67, 150.1, 200.0, 0),
			fakegw.Msg(1, 6, id, 68, 150.05, 5.0, 0),
			fakegw.Msg(57, 1, id),
		}
	})
	ib := connectFake(t, srv)

	tk, err := ib.ReqTickerSnapshot(ctx5s(t), aaplE2E())
	if err != nil {
		t.Fatal(err)
	}
	if tk.Bid != 150 || tk.Ask != 150.1 || tk.Last != 150.05 || tk.AskSize != 200 || tk.MarketDataType != 3 {
		t.Fatalf("ticker bid %g ask %g last %g askSize %g type %d", tk.Bid, tk.Ask, tk.Last, tk.AskSize, tk.MarketDataType)
	}
	if tk.Time.IsZero() {
		t.Fatal("ticker time must be stamped before the snapshot completes")
	}
	ib.state.Mu.RLock()
	n, nc := len(ib.state.ReqID2Ticker), len(ib.state.ReqID2Contract)
	ib.state.Mu.RUnlock()
	if n != 0 || nc != 0 {
		t.Fatalf("snapshot ticker must be detached: %d tickers, %d contracts mapped", n, nc)
	}
	if req := srv.LastRequest(1); len(req) < 3 {
		t.Fatalf("reqMktData = %q", req)
	}
}

// The SPX index snapshot as a live Gateway sent it on a closed market: the
// close with an empty price, bid and ask as -1. The close must stay unset,
// not read as 0.
func TestReqTickerSnapshotEmptyClose(t *testing.T) {
	srv := startFake(t)
	srv.Handle(1, func(req []string) [][]byte {
		id := req[2]
		return [][]byte{
			fakegw.Msg(58, 1, id, 1),
			fakegw.Msg(81, id, "0.01", "", 0), // tickReqParams
			fakegw.Msg(1, 6, id, 9, "", 0, 0),
			fakegw.Msg(1, 6, id, 1, "-1.00", 0, 0),
			fakegw.Msg(1, 6, id, 2, "-1.00", 0, 0),
			fakegw.Msg(57, 1, id),
		}
	})
	ib := connectFake(t, srv)

	spx := &contract.Contract{ConID: 416904, Symbol: "SPX", SecType: "IND", Exchange: "CBOE", Currency: "USD"}
	tk, err := ib.ReqTickerSnapshot(ctx5s(t), spx)
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(tk.Close) || tk.Bid != -1 || tk.Ask != -1 {
		t.Fatalf("close %g bid %g ask %g, want NaN/-1/-1", tk.Close, tk.Bid, tk.Ask)
	}
}

func TestReqTickerSnapshotError(t *testing.T) {
	srv := startFake(t)
	srv.Handle(1, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(4, 2, req[2], 354, "Requested market data is not subscribed", "")}
	})
	ib := connectFake(t, srv)
	_, err := ib.ReqTickerSnapshot(ctx5s(t), aaplE2E())
	var re *RequestError
	if !errors.As(err, &re) || re.Code != 354 {
		t.Fatalf("err = %v, want RequestError 354", err)
	}
	ib.state.Mu.RLock()
	n := len(ib.state.ReqID2Ticker)
	ib.state.Mu.RUnlock()
	if n != 0 {
		t.Fatalf("%d tickers still mapped after error", n)
	}
}

func TestReqTickerSnapshotTimeoutCancels(t *testing.T) {
	srv := startFake(t)
	srv.Handle(1, func([]string) [][]byte { return nil }) // never completes
	ib := connectFake(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := ib.ReqTickerSnapshot(ctx, aaplE2E()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	waitFor(t, "cancelMktData", func() bool { return srv.Received(2) == 1 })
}

func TestReqTickerSnapshotInvalid(t *testing.T) {
	ib := New()
	if _, err := ib.ReqTickerSnapshot(context.Background(), nil); err == nil {
		t.Fatal("nil contract must fail")
	}
	if _, err := ib.ReqTickerSnapshot(context.Background(), contract.Stock("AAPL", "SMART", "USD")); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("err = %v, want ErrNotConnected", err)
	}
}

func TestReqMarketDataType(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	for _, bad := range []int{0, 5} {
		if err := ib.ReqMarketDataType(bad); err == nil {
			t.Fatalf("type %d must be rejected", bad)
		}
	}
	if err := ib.ReqMarketDataType(3); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "reqMarketDataType", func() bool { return srv.Received(59) == 1 })
	if req := srv.LastRequest(59); len(req) != 3 || req[2] != "3" {
		t.Fatalf("reqMarketDataType = %q", req)
	}
}

// CancelMktData must cancel the stream, never a pending snapshot of the same
// *Contract.
func TestCancelMktDataSkipsPendingSnapshot(t *testing.T) {
	srv := startFake(t)
	var mu sync.Mutex
	var reqIDs []string
	srv.Handle(1, func(req []string) [][]byte { // neither request completes
		mu.Lock()
		reqIDs = append(reqIDs, req[2])
		mu.Unlock()
		return nil
	})
	ib := connectFake(t, srv)
	con := aaplE2E()
	if _, err := ib.ReqMktData(con, "", false, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	snap := make(chan error, 1)
	go func() {
		_, err := ib.ReqTickerSnapshot(ctx, con)
		snap <- err
	}()
	waitFor(t, "both requests", func() bool { return srv.Received(1) == 2 })

	ib.CancelMktData(con)
	waitFor(t, "cancelMktData", func() bool { return srv.Received(2) == 1 })
	mu.Lock()
	stream := reqIDs[0]
	mu.Unlock()
	if req := srv.LastRequest(2); req[len(req)-1] != stream {
		t.Fatalf("cancelMktData %q, want the stream's request %s", req, stream)
	}
	select {
	case err := <-snap:
		t.Fatalf("snapshot ended by the stream's cancel: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := <-snap; !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot err = %v", err)
	}
}
