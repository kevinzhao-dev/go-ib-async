package ibgo

// Regression tests for behaviour observed against a live IB Gateway
// (server version 178). Message fields are copied from captured traffic.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
	"github.com/kevinzhao-dev/go-ib-async/internal/state"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

const readOnlyMsg = "Error validating request.-'cp' : cause - The API interface is currently in Read-Only mode."

func readOnlyReply(code string) func([]string) [][]byte {
	return func([]string) [][]byte {
		return [][]byte{fakegw.Msg(4, 2, -1, 321, "Error validating request.-'"+code+"' : cause - The API interface is currently in Read-Only mode.", "")}
	}
}

func TestValidationError321FailsPendingRequest(t *testing.T) {
	srv := startFake(t)
	srv.Handle(20, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(4, 2, req[1], 321, "Error validating request.-'bH' : cause - Historical data bar size setting is invalid.", "")}
	})
	ib := connectFake(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err := ib.ReqHistoricalData(ctx, aaplE2E(), "", "1 D", "7 mins", "TRADES", true, 1)
	var ibe *IBError
	if !errors.As(err, &ibe) || ibe.Code != 321 || ibe.IsWarning {
		t.Fatalf("err = %v, want non-warning IBError 321", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("request was not failed immediately")
	}
}

func TestRequestErrorIsIBError(t *testing.T) {
	srv := startFake(t)
	srv.Handle(9, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(4, 2, req[2], 200, "No security definition has been found for the request", "")}
	})
	ib := connectFake(t, srv)
	var evt *IBError
	ib.ErrorEvent.Subscribe(func(e *IBError) { evt = e })
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := ib.ReqContractDetails(ctx, contract.Stock("ZZZZ", "SMART", "USD"))
	var ibe *IBError
	if !errors.As(err, &ibe) || ibe.Code != 200 {
		t.Fatalf("err = %v, want *IBError 200", err)
	}
	var re *RequestError
	if !errors.As(err, &re) {
		t.Fatal("RequestError alias no longer matches")
	}
	if ibe.Contract == nil || ibe.Contract.Symbol != "ZZZZ" {
		t.Fatalf("error contract = %+v", ibe.Contract)
	}
	if evt != ibe {
		t.Fatal("ErrorEvent and returned error should be the same value")
	}
}

func TestReadOnlyRejectionsFailFast(t *testing.T) {
	srv := startFake(t)
	srv.Handle(5, readOnlyReply("cp"))
	srv.Handle(99, readOnlyReply("bZ"))
	ib := connectFake(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := ib.ReqOpenOrders(ctx); !errors.Is(err, ErrReadOnlyAPI) {
		t.Fatalf("ReqOpenOrders err = %v, want ErrReadOnlyAPI", err)
	}
	if _, err := ib.ReqCompletedOrders(ctx, false); !errors.Is(err, ErrReadOnlyAPI) {
		t.Fatalf("ReqCompletedOrders err = %v, want ErrReadOnlyAPI", err)
	}
	// reqAllOpenOrders works in read-only mode and must not be failed by a
	// stray read-only error for another request.
	if _, err := ib.ReqAllOpenOrders(ctx); err != nil {
		t.Fatalf("ReqAllOpenOrders: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("requests waited for the timeout")
	}
}

func TestReadOnlyErrorDoesNotFailUnrelatedSingles(t *testing.T) {
	ib := newTestIB()
	key := int64(-42)
	ch := ib.state.StartReq(key, nil)
	ib.openOrdersReq.key.Store(key) // reqAllOpenOrders in flight (not rejectable)
	dispatch(ib, "4", "2", "-1", "321", readOnlyMsg, "")
	select {
	case <-ch:
		t.Fatal("reqAllOpenOrders must not be failed by a read-only error")
	default:
	}
	ib.openOrdersReq.roRejected.Store(true)
	dispatch(ib, "4", "2", "-1", "321", readOnlyMsg, "")
	res := <-ch
	if !errors.Is(res.Err, ErrReadOnlyAPI) {
		t.Fatalf("err = %v", res.Err)
	}
}

func TestSyncOrdersReadOnlyFallback(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	con := aaplE2E()
	tr, err := ib.PlaceOrder(con, order.LimitOrder("BUY", 1, 10))
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, tr, order.StatusSubmitted)

	// Switch the fake to read-only and make the local trade stale.
	srv.Handle(5, readOnlyReply("cp"))
	srv.Handle(99, readOnlyReply("bZ"))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rep, err := ib.SyncOrders(ctx, SyncOptions{IncludeCompleted: true})
	if err != nil {
		t.Fatalf("SyncOrders: %v", err)
	}
	if !rep.ReadOnly {
		t.Fatal("report should flag read-only mode")
	}
	if len(rep.OpenTrades) != 1 || rep.OpenTrades[0] != tr {
		t.Fatalf("open trades = %d, want the local trade via reqAllOpenOrders", len(rep.OpenTrades))
	}
}

func TestPendingSubmitOrderRejectedBy321(t *testing.T) {
	ib := newTestIB()
	tr := order.NewTrade(aaplE2E(), order.LimitOrder("BUY", 1, 10))
	tr.Order.OrderID = 55
	tr.Order.ClientID = ib.ClientID()
	tr.OrderStatus.Status = order.StatusPendingSubmit
	ib.state.Trades[state.OrderKey{ClientID: ib.ClientID(), OrderID: 55}] = tr
	dispatch(ib, "4", "2", "55", "321", readOnlyMsg, "")
	if tr.Status() != order.StatusCancelled {
		t.Fatalf("status = %s, want Cancelled", tr.Status())
	}
}

func TestEpochTimesAreUTC(t *testing.T) {
	if got := parseIBDatetime("1764217860"); got.Location() != time.UTC || got.Unix() != 1764217860 {
		t.Fatalf("got %v", got)
	}
	ib := newTestIB()
	rt := &market.RealTimeBarList{ReqID: 3}
	ib.state.ReqID2Subscriber[3] = rt
	dispatch(ib, "50", "3", "3", "1764217860", "1.09745", "1.09745", "1.09745", "1.09745", "-1", "-1", "-1")
	if len(rt.Bars) != 1 || rt.Bars[0].Time.Location() != time.UTC {
		t.Fatalf("bars = %+v", rt.Bars)
	}
}
