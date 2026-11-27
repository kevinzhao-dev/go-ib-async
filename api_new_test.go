package ibgo

// Tests for the market data / account / contract APIs. Inbound messages are
// copied from a live IB Gateway session (server version 178).

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/protocol"
)

func split(msg string) []string { return strings.Split(msg, "|") }

func feedMsg(ib *IB, msg string) { dispatch(ib, split(msg)...) }

// TestInboundMessageIDs pins the inbound ids to the official ibapi numbering
// (79 and 81 were swapped with other ids before; confirmed on a live
// gateway).
func TestInboundMessageIDs(t *testing.T) {
	want := map[string][2]int{
		"SymbolSamples":     {protocol.InMsgSymbolSamples, 79},
		"MktDepthExchanges": {protocol.InMsgMktDepthExchanges, 80},
		"TickReqParams":     {protocol.InMsgTickReqParams, 81},
		"SmartComponents":   {protocol.InMsgSmartComponents, 82},
		"NewsArticle":       {protocol.InMsgNewsArticle, 83},
		"TickNews":          {protocol.InMsgTickNews, 84},
		"NewsProviders":     {protocol.InMsgNewsProviders, 85},
		"HeadTimestamp":     {protocol.InMsgHeadTimestamp, 88},
		"HistDataUpdate":    {protocol.InMsgHistoricalDataUpdate, 90},
		"MarketRule":        {protocol.InMsgMarketRule, 93},
		"PnL":               {protocol.InMsgPnL, 94},
		"PnLSingle":         {protocol.InMsgPnLSingle, 95},
		"HistoricalTicks":   {protocol.InMsgHistoricalTicks, 96},
		"TickByTick":        {protocol.InMsgTickByTick, 99},
		"TickOptionComp":    {protocol.InMsgTickOptionComp, 21},
	}
	for name, v := range want {
		if v[0] != v[1] {
			t.Errorf("%s = %d, want %d", name, v[0], v[1])
		}
	}
}

func newTickerReq(ib *IB, reqID int64, con *contract.Contract) *market.Ticker {
	tk := market.NewTicker(con)
	ib.registerTickerReq(reqID, con, tk)
	return tk
}

func TestTickReqParamsAndGreeks(t *testing.T) {
	ib := newTestIB()
	opt := &contract.Contract{ConID: 945700537, Symbol: "AAPL", SecType: "OPT"}
	tk := newTickerReq(ib, 15, opt)

	feedMsg(ib, "81|15|0.01|c70003|0")
	feedMsg(ib, "21|15|83|0|0.23814440180248647|0.5402742436003877|5.201084149220854|0.0|0.03390775812618169|0.1955261139009492|-0.3672632233709878|355.75")
	feedMsg(ib, "21|15|80|0|-1|-2|-1|0.0|-2|-2|-2|355.07")

	d := tk.Snapshot()
	if d.MinTick != 0.01 || d.BboExchange != "c70003" || d.SnapshotPermissions != 0 {
		t.Fatalf("tickReqParams: minTick=%v bbo=%q perms=%d", d.MinTick, d.BboExchange, d.SnapshotPermissions)
	}
	m := d.ModelGreeks
	if m == nil || *m.ImpliedVol != 0.23814440180248647 || *m.Delta != 0.5402742436003877 ||
		*m.OptPrice != 5.201084149220854 || *m.PvDividend != 0 || *m.Gamma != 0.03390775812618169 ||
		*m.Vega != 0.1955261139009492 || *m.Theta != -0.3672632233709878 || *m.UndPrice != 355.75 {
		t.Fatalf("model greeks = %+v", m)
	}
	b := d.BidGreeks
	if b == nil || b.ImpliedVol != nil || b.Delta != nil || b.OptPrice != nil || b.Gamma != nil ||
		b.Vega != nil || b.Theta != nil || b.PvDividend == nil || *b.UndPrice != 355.07 {
		t.Fatalf("bid greeks sentinels not mapped to nil: %+v", b)
	}
}

func TestDelayedTicksAndSnapshotRelease(t *testing.T) {
	ib := newTestIB()
	es := &contract.Contract{ConID: 815824257, Symbol: "MES", SecType: "FUT"}
	tk := newTickerReq(ib, 14, es)
	ib.state.StartReq(14, es)
	ib.client.OnDataArrived()
	for _, m := range []string{
		"58|1|14|3",
		"1|6|14|66|7316.25|19|0",
		"1|6|14|67|7316.50|14|0",
		"1|6|14|68|7316.50|2|0",
		"46|6|14|88|1795753114",
		"1|6|14|72|7321.25|0|0",
		"1|6|14|73|7187.00|0|0",
		"2|6|14|74|532848",
		"1|6|14|75|7195.75|0|0",
		"1|6|14|76|7241.00|0|0",
	} {
		feedMsg(ib, m)
	}
	d := tk.Snapshot()
	if d.MarketDataType != 3 || d.Bid != 7316.25 || d.BidSize != 19 || d.Ask != 7316.5 || d.Last != 7316.5 ||
		d.High != 7321.25 || d.Low != 7187 || d.Volume != 532848 || d.Close != 7195.75 || d.Open != 7241 {
		t.Fatalf("delayed ticker = %+v", d)
	}
	if d.DelayedLastTimestamp.Unix() != 1795753114 || d.DelayedLastTimestamp.Location() != time.UTC {
		t.Fatalf("delayed last timestamp = %v", d.DelayedLastTimestamp)
	}
	if len(d.Ticks) != 8 {
		t.Fatalf("ticks in batch = %d, want 8", len(d.Ticks))
	}
	feedMsg(ib, "57|1|14")
	// Completed only after the batch is processed (its ticks are stamped
	// onto the ticker in OnDataProcessed).
	if ib.tickerFor(14) == nil || !ib.state.Requests.Has(14) {
		t.Fatal("snapshot completed before the end of its batch")
	}
	ib.client.OnDataProcessed()
	if ib.tickerFor(14) != nil || ib.state.Requests.Has(14) {
		t.Fatal("snapshot end must release the request")
	}
	if len(tk.Snapshot().Ticks) != 0 {
		t.Fatal("per-batch ticks not cleared")
	}
}

func TestTickByTickAndDepth(t *testing.T) {
	ib := newTestIB()
	fx := &contract.Contract{ConID: 12087792, Symbol: "EUR", SecType: "CASH"}
	tk := newTickerReq(ib, 25, fx)
	ib.registerTickerReq(26, fx, tk)
	ib.registerTickerReq(27, fx, tk)

	feedMsg(ib, "99|25|3|1795753723|1.09735|1.09738|5000000|1500000|0")
	feedMsg(ib, "99|26|4|1795753720|1.097365")
	feedMsg(ib, "99|26|2|1795753721|1.0974|20000|3|IDEALPRO|cond")
	feedMsg(ib, "12|1|27|0|0|1|1.09735|5000000")
	feedMsg(ib, "12|1|27|1|0|1|1.09734|6500000")
	feedMsg(ib, "12|1|27|0|0|0|1.09738|1500000")
	feedMsg(ib, "13|1|27|0|MM1|1|1|1.09736|1000000|1") // update bid level 0 (L2)

	d := tk.Snapshot()
	if len(d.TickByTicks) != 3 {
		t.Fatalf("tick-by-ticks = %d", len(d.TickByTicks))
	}
	ba, ok := d.TickByTicks[0].(market.TickByTickBidAsk)
	if !ok || ba.BidPrice != 1.09735 || ba.AskPrice != 1.09738 || ba.BidSize != 5e6 || ba.AskSize != 1.5e6 || ba.Time.Unix() != 1795753723 {
		t.Fatalf("bid/ask tick = %+v", d.TickByTicks[0])
	}
	if mp, ok := d.TickByTicks[1].(market.TickByTickMidPoint); !ok || mp.MidPoint != 1.097365 {
		t.Fatalf("midpoint tick = %+v", d.TickByTicks[1])
	}
	last, ok := d.TickByTicks[2].(market.TickByTickAllLast)
	if !ok || last.Price != 1.0974 || last.Size != 20000 || !last.TickAttribLast.PastLimit || !last.TickAttribLast.Unreported || last.Exchange != "IDEALPRO" || last.SpecialConditions != "cond" {
		t.Fatalf("last tick = %+v", last)
	}
	if d.Bid != 1.09735 || d.Ask != 1.09738 || d.Last != 1.0974 {
		t.Fatalf("ticker bid/ask/last = %v/%v/%v", d.Bid, d.Ask, d.Last)
	}
	if len(d.DomBids) != 2 || d.DomBids[0].Price != 1.09736 || d.DomBids[0].MarketMaker != "MM1" || d.DomBids[1].Price != 1.09734 {
		t.Fatalf("bids = %+v", d.DomBids)
	}
	if len(d.DomAsks) != 1 || d.DomAsks[0].Price != 1.09738 {
		t.Fatalf("asks = %+v", d.DomAsks)
	}
	feedMsg(ib, "12|1|27|0|2|1|0|0") // delete top bid
	if d := tk.Snapshot(); len(d.DomBids) != 1 || d.DomTicks[len(d.DomTicks)-1].Price != 1.09736 {
		t.Fatalf("after delete bids = %+v", d.DomBids)
	}
	feedMsg(ib, "12|1|27|9|2|1|0|0")  // delete beyond the book: ignored
	feedMsg(ib, "12|1|27|-1|0|1|1|1") // negative position: ignored
}

func TestHistoricalTicksDecoding(t *testing.T) {
	ib := newTestIB()
	ch := ib.state.StartReq(18, nil)
	feedMsg(ib, "98|18|2|1795640339|2|355.86|19|NASDAQ| F I|1795640339|0|355.85|40|BATS| F  |0")
	feedMsg(ib, "98|18|1|1795640340|1|355.84|5|ARCA||1")
	res := <-ch
	items := resultItems(res.Value)
	if len(items) != 3 {
		t.Fatalf("items = %d", len(items))
	}
	l0 := items[0].(market.HistoricalTickLast)
	if l0.Price != 355.86 || l0.Size != 19 || l0.Exchange != "NASDAQ" || l0.SpecialConditions != " F I" || !l0.TickAttribLast.Unreported || l0.TickAttribLast.PastLimit {
		t.Fatalf("tick 0 = %+v", l0)
	}
	if !items[2].(market.HistoricalTickLast).TickAttribLast.PastLimit {
		t.Fatal("past limit bit")
	}

	ch = ib.state.StartReq(20, nil)
	feedMsg(ib, "97|20|1|1795640334|1|1.09763|1.09766|1000000|9500000|1")
	ba := resultItems((<-ch).Value)[0].(market.HistoricalTickBidAsk)
	if ba.PriceBid != 1.09763 || ba.PriceAsk != 1.09766 || ba.SizeBid != 1e6 || ba.SizeAsk != 9.5e6 || !ba.TickAttribBidAsk.AskPastHigh {
		t.Fatalf("bid/ask tick = %+v", ba)
	}

	ch = ib.state.StartReq(19, nil)
	feedMsg(ib, "96|19|2|1795640303|0|1.097625|0|1795640304|0|1.09762|0|1")
	mid := resultItems((<-ch).Value)
	if len(mid) != 2 || mid[1].(market.HistoricalTick).Price != 1.09762 || mid[0].(market.HistoricalTick).Time.Unix() != 1795640303 {
		t.Fatalf("midpoint ticks = %+v", mid)
	}
}

func TestPnLAndPortfolioHandlers(t *testing.T) {
	ib := newTestIB()
	ib.client.SetServerVersion(178)
	key := pnlKey{account: "U1", modelCode: ""}
	ib.pnl[key] = &account.PnL{Account: "U1"}
	ib.state.ReqID2Subscriber[21] = key
	skey := pnlKey{account: "U1", conID: 815824257}
	ib.pnlSingle[skey] = &account.PnLSingle{Account: "U1", ConID: 815824257}
	ib.state.ReqID2Subscriber[22] = skey

	var pnlEvt atomic.Pointer[account.PnL]
	ib.PnLEvent.Subscribe(func(p *account.PnL) { pnlEvt.Store(p) })
	feedMsg(ib, "94|21|412.7500000000012|412.7500000000012|0.0")
	feedMsg(ib, "95|22|3|150.1259765625012|150.1259765625012|1.7976931348623157E308|109731.8759765625")

	p := ib.PnL()
	if len(p) != 1 || p[0].DailyPnL != 412.7500000000012 || p[0].RealizedPnL != 0 || pnlEvt.Load() == nil {
		t.Fatalf("pnl = %+v", p)
	}
	s := ib.PnLSingles()
	if len(s) != 1 || s[0].Position != 3 || s[0].Value != 109731.8759765625 || !math.IsNaN(s[0].RealizedPnL) {
		t.Fatalf("pnl single = %+v", s)
	}

	var portEvt atomic.Int32
	ib.PortfolioEvent.Subscribe(func(*account.PortfolioItem) { portEvt.Add(1) })
	feedMsg(ib, "7|8|815824257|MES|FUT|20261218|0|0|5|CME|USD|MESZ6|MES|3|7312.1250977|109681.88|36540.25|60.13|0.0|U1")
	port := ib.Portfolio()
	if len(port) != 1 || port[0].Position != 3 || port[0].MarketValue != 109681.88 || port[0].Contract.LocalSymbol != "MESZ6" || portEvt.Load() != 1 {
		t.Fatalf("portfolio = %+v", port)
	}
}

func TestContractLookupHandlers(t *testing.T) {
	srv := startFake(t)
	srv.Handle(81, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(79, req[1], 2,
			6223250, "TSM", "STK", "NYSE", "USD", 5, "CFD", "OPT", "IOPT", "WAR", "BAG", "TAIWAN SEMICONDUCTOR-SP ADR", "",
			589552755, "TSME", "STK", "ARCA", "USD", 0, "THRIVENT SML-MDC EQ ETF-US I", "")}
	})
	srv.Handle(91, func(req []string) [][]byte { return [][]byte{fakegw.Msg(93, req[1], 1, 0, 0.01)} })
	srv.Handle(87, func(req []string) [][]byte { return [][]byte{fakegw.Msg(88, req[1], "19801212-14:30:00")} })
	srv.Handle(49, func([]string) [][]byte { return [][]byte{fakegw.Msg(49, 1, 1795753114)} })
	ib := connectFake(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	syms, err := ib.ReqMatchingSymbols(ctx, "TSM")
	if err != nil || len(syms) != 2 || syms[0].Contract.ConID != 6223250 || syms[0].Contract.PrimaryExchange != "NYSE" ||
		len(syms[0].DerivativeSecTypes) != 5 || syms[0].Contract.Description != "TAIWAN SEMICONDUCTOR-SP ADR" || syms[1].Contract.Symbol != "TSME" {
		t.Fatalf("symbols = %+v err=%v", syms, err)
	}
	incs, err := ib.ReqMarketRule(ctx, 4563)
	if err != nil || len(incs) != 1 || incs[0].Increment != 0.01 {
		t.Fatalf("market rule = %+v err=%v", incs, err)
	}
	head, err := ib.ReqHeadTimeStamp(ctx, aaplE2E(), "TRADES", true)
	if err != nil || !head.Equal(time.Date(1980, 12, 12, 14, 30, 0, 0, time.UTC)) {
		t.Fatalf("head = %v err=%v", head, err)
	}
	waitFor(t, "cancelHeadTimeStamp", func() bool { return srv.Received(90) == 1 })
	now, err := ib.ReqCurrentTime(ctx)
	if err != nil || now.Unix() != 1795753114 {
		t.Fatalf("current time = %v err=%v", now, err)
	}
}

func TestQualifyContracts(t *testing.T) {
	srv := startFake(t)
	srv.Handle(9, func(req []string) [][]byte {
		id := req[2]
		switch req[4] {
		case "AAPL":
			return [][]byte{contractDetailsMsg(id, "STK", ""), fakegw.Msg(52, 1, id)}
		case "DUP":
			return [][]byte{contractDetailsMsg(id, "STK", ""), contractDetailsMsg(id, "STK", ""), fakegw.Msg(52, 1, id)}
		default:
			return [][]byte{fakegw.Msg(4, 2, id, 200, "No security definition has been found for the request", "")}
		}
	})
	ib := connectFake(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	good := contract.Stock("AAPL", "SMART", "USD")
	dup := contract.Stock("DUP", "SMART", "USD")
	missing := contract.Stock("NOPE", "SMART", "USD")
	err := ib.QualifyContracts(ctx, good, dup, missing)
	if good.ConID == 0 || good.Exchange != "SMART" {
		t.Fatalf("not qualified: %+v", good)
	}
	if dup.ConID != 0 || missing.ConID != 0 {
		t.Fatal("failed contracts must stay untouched")
	}
	var qe *QualifyError
	if !errors.Is(err, ErrAmbiguousContract) || !errors.Is(err, ErrContractNotFound) || !errors.As(err, &qe) {
		t.Fatalf("err = %v", err)
	}
	if err := ib.QualifyContracts(ctx, contract.Stock("AAPL", "SMART", "USD")); err != nil {
		t.Fatalf("single qualify: %v", err)
	}
}

func TestReqTickersWaitsForAllSnapshots(t *testing.T) {
	srv := startFake(t)
	srv.Handle(1, func(req []string) [][]byte {
		id := req[2]
		if req[4] == "NOPE" {
			return [][]byte{fakegw.Msg(4, 2, id, 354, "Requested market data is not subscribed.", "")}
		}
		return [][]byte{
			fakegw.Msg(58, 1, id, 3),
			fakegw.Msg(4, 2, id, 10091, "Part of requested market data requires additional subscription for API.", ""),
			fakegw.Msg(1, 6, id, 75, 355.58, 0, 0),
			fakegw.Msg(57, 1, id),
		}
	})
	ib := connectFake(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	a := aaplE2E()
	n := &contract.Contract{ConID: 9, Symbol: "NOPE", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	tickers, err := ib.ReqTickers(ctx, a, n)
	if len(tickers) != 2 || tickers[0].Snapshot().Close != 355.58 || tickers[0].Snapshot().MarketDataType != 3 {
		t.Fatalf("tickers = %+v", tickers)
	}
	var ibe *IBError
	if !errors.As(err, &ibe) || ibe.Code != 354 {
		t.Fatalf("err = %v, want joined 354", err)
	}
	ib.state.Mu.RLock()
	left := len(ib.state.ReqID2Ticker)
	ib.state.Mu.RUnlock()
	if left != 0 {
		t.Fatalf("%d snapshot requests not released", left)
	}
}

func TestReqMarketDataTypeValidation(t *testing.T) {
	ib := newTestIB()
	if err := ib.ReqMarketDataType(7); err == nil {
		t.Fatal("invalid type accepted")
	}
	if err := ib.ReqMarketDataType(3); err == nil {
		t.Fatal("not connected: expected send error")
	}
	if ib.mdType.Load() != 0 {
		t.Fatal("failed request must not be remembered")
	}
}

func TestHistoricalDataStream(t *testing.T) {
	srv := startFake(t)
	srv.Handle(20, func(req []string) [][]byte {
		id := req[1]
		return [][]byte{
			fakegw.Msg(17, id, "20261126 22:58:41 US/Eastern", "20261126 23:28:41 US/Eastern", 2,
				"20261126 22:58:00 US/Eastern", 1.0976, 1.09765, 1.0976, 1.0976, -1, -1, -1,
				"20261126 22:59:00 US/Eastern", 1.0976, 1.09765, 1.0976, 1.09765, -1, -1, -1),
			fakegw.Msg(90, id, -1, "20261126 22:59:00 US/Eastern", 1.0976, 1.09765, 1.09765, 1.0976, -1, -1), // same as last: no event
			fakegw.Msg(90, id, -1, "20261126 22:59:00 US/Eastern", 1.0976, 1.0977, 1.0977, 1.0976, -1, -1),   // revise last
			fakegw.Msg(90, id, -1, "20261126 23:00:00 US/Eastern", 1.0977, 1.0978, 1.0978, 1.0977, -1, -1),   // new bar
			fakegw.Msg(90, id, -1, "20261126 22:58:00 US/Eastern", 1, 1, 1, 1, -1, -1),                       // stale: ignored
		}
	})
	ib := connectFake(t, srv)
	var revised, added atomic.Int32
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	fx := &contract.Contract{ConID: 12087792, Symbol: "EUR", SecType: "CASH", Exchange: "IDEALPRO", Currency: "USD"}
	bars, err := ib.ReqHistoricalDataStream(ctx, fx, "1800 S", "1 min", "MIDPOINT", false, 1)
	if err != nil {
		t.Fatal(err)
	}
	bars.UpdateEvent.Subscribe(func(b *market.BarDataList) {
		if b.NewBar {
			added.Add(1)
		} else {
			revised.Add(1)
		}
	})
	if req := srv.LastRequest(20); req[len(req)-3] != "1" {
		t.Fatalf("keepUpToDate flag not sent: %v", req)
	}
	waitFor(t, "3 bars", func() bool { return len(bars.Snapshot()) == 3 })
	b := bars.Snapshot()
	if b[1].Close != 1.0977 || b[2].Close != 1.0978 {
		t.Fatalf("bars = %+v", b)
	}
	ib.CancelHistoricalData(bars)
	waitFor(t, "cancel", func() bool { return srv.Received(25) == 1 })
}

func TestSubscriptionsRestoredAfterReconnect(t *testing.T) {
	srv := startFake(t)
	ib := New()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg := DefaultReconnectConfig()
	cfg.RetryDelay = 20 * time.Millisecond
	cfg.ProbeInterval = 0
	cfg.SyncOrders = false
	connected := make(chan struct{}, 4)
	go ib.ConnectWithReconnect(ctx, "127.0.0.1", srv.Port(), e2eClientID, cfg, func() { connected <- struct{}{} })
	<-connected

	if err := ib.ReqMarketDataType(MarketDataDelayed); err != nil {
		t.Fatal(err)
	}
	tk, err := ib.ReqMktData(aaplE2E(), "233", false, false)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := ib.ReqRealTimeBars(aaplE2E(), 5, "TRADES", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := ib.ReqPnL("", ""); err != nil {
		t.Fatal(err)
	}
	oldRt := rt.ReqID
	waitFor(t, "requests", func() bool { return srv.Received(1) == 1 && srv.Received(50) == 1 && srv.Received(92) == 1 })

	srv.DropConnection()
	<-connected
	waitFor(t, "restored requests", func() bool {
		return srv.Received(1) == 2 && srv.Received(50) == 2 && srv.Received(92) == 2 && srv.Received(59) == 2
	})
	req := srv.LastRequest(1)
	newID := req[2]
	if req[16] != "233" {
		t.Fatalf("generic ticks not restored: %v", req)
	}
	// Ticks for the new request id update the same Ticker object.
	srv.Handle(49, func([]string) [][]byte {
		return [][]byte{fakegw.Msg(1, 6, newID, 4, 123.45, 1, 0), fakegw.Msg(49, 1, 1795753114)}
	})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	if _, err := ib.ReqCurrentTime(ctx2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "tick on restored subscription", func() bool { return tk.Snapshot().Last == 123.45 })
	if rt.ReqID == oldRt {
		t.Fatal("bar list request id not updated")
	}

	ib.CancelMktData(aaplE2E())
	ib.CancelRealTimeBars(rt)
	ib.CancelPnL("", "")
	waitFor(t, "cancels", func() bool { return srv.Received(2) == 1 && srv.Received(51) == 1 && srv.Received(93) == 1 })
	ib.subsMu.Lock()
	n := len(ib.subs)
	ib.subsMu.Unlock()
	if n != 0 {
		t.Fatalf("%d subscriptions left", n)
	}
}

func TestAccountUpdatesAndPnLSingleSubscriptions(t *testing.T) {
	srv := startFake(t)
	srv.Handle(6, func(req []string) [][]byte {
		if req[2] != "1" {
			return nil
		}
		return [][]byte{
			fakegw.Msg(6, 2, "NetLiquidation", "100000", "USD", req[3]),
			fakegw.Msg(7, 8, 815824257, "MES", "FUT", "20261218", 0, "0", "5", "CME", "USD", "MESZ6", "MES", 3, 7312.13, 109681.88, 36540.25, 60.13, 0.0, req[3]),
			fakegw.Msg(54, 1, req[3]),
		}
	})
	ib := connectFake(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ib.ReqAccountUpdates(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if req := srv.LastRequest(6); req[3] != "DU123456" {
		t.Fatalf("account not defaulted: %v", req)
	}
	if len(ib.Portfolio()) != 1 || len(ib.AccountValues()) != 1 {
		t.Fatalf("portfolio=%d values=%d", len(ib.Portfolio()), len(ib.AccountValues()))
	}
	if err := ib.CancelAccountUpdates(""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "unsubscribe", func() bool { r := srv.LastRequest(6); return len(r) > 2 && r[2] == "0" })

	if err := ib.ReqPnLSingle("", "", 815824257); err != nil {
		t.Fatal(err)
	}
	if err := ib.ReqPnLSingle("", "", 815824257); err != nil { // idempotent
		t.Fatal(err)
	}
	if len(ib.PnLSingles()) != 1 {
		t.Fatal("pnl single not registered")
	}
	ib.CancelPnLSingle("", "", 815824257)
	waitFor(t, "cancel pnl single", func() bool { return srv.Received(94) == 1 && srv.Received(95) == 1 })
	if len(ib.PnLSingles()) != 0 {
		t.Fatal("pnl single not removed")
	}
}

func TestDefaultAccountNeedsSingleAccount(t *testing.T) {
	ib := newTestIB()
	if _, err := ib.defaultAccount(""); err == nil {
		t.Fatal("no managed accounts: expected error")
	}
	if a, err := ib.defaultAccount("U1"); err != nil || a != "U1" {
		t.Fatal("explicit account")
	}
	if err := ib.ReqPnL("", ""); err == nil {
		t.Fatal("ReqPnL without account must fail")
	}
	if err := ib.ReqPnLSingle("", "", 1); err == nil {
		t.Fatal("ReqPnLSingle without account must fail")
	}
	if err := ib.ReqAccountUpdates(context.Background(), ""); err == nil {
		t.Fatal("ReqAccountUpdates without account must fail")
	}
	if err := ib.CancelAccountUpdates(""); err == nil {
		t.Fatal("CancelAccountUpdates without account must fail")
	}
}

func TestTickByTickAndDepthSubscriptions(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	fx := &contract.Contract{ConID: 12087792, Symbol: "EUR", SecType: "CASH", Exchange: "IDEALPRO", Currency: "USD"}
	if _, err := ib.ReqTickByTickData(fx, "Bogus", 0, false); err == nil {
		t.Fatal("invalid tick type accepted")
	}
	t1, err := ib.ReqTickByTickData(fx, TickByTickBidAsk, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	t2, err := ib.ReqMktDepth(fx, 5, false)
	if err != nil {
		t.Fatal(err)
	}
	t3, err := ib.ReqMktData(fx, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if t1 != t2 || t2 != t3 {
		t.Fatal("subscriptions of one contract must share the ticker")
	}
	waitFor(t, "requests", func() bool { return srv.Received(97) == 1 && srv.Received(10) == 1 && srv.Received(1) == 1 })
	ib.CancelTickByTickData(fx, TickByTickMidPoint) // not subscribed: no-op
	ib.CancelTickByTickData(fx, TickByTickBidAsk)
	ib.CancelMktDepth(fx, true) // smart depth not subscribed: no-op
	ib.CancelMktDepth(fx, false)
	waitFor(t, "cancels", func() bool { return srv.Received(98) == 1 && srv.Received(11) == 1 })
	ib.state.Mu.RLock()
	_, stillThere := ib.state.Tickers[fx.Key()]
	ib.state.Mu.RUnlock()
	if !stillThere {
		t.Fatal("ticker dropped while ReqMktData still uses it")
	}
	ib.CancelMktData(fx)
	ib.state.Mu.RLock()
	_, stillThere = ib.state.Tickers[fx.Key()]
	ib.state.Mu.RUnlock()
	if stillThere {
		t.Fatal("ticker kept after the last subscription")
	}
}

func TestReqHistoricalTicksOverWire(t *testing.T) {
	srv := startFake(t)
	srv.Handle(96, func(req []string) [][]byte {
		id := req[1]
		switch req[len(req)-4] {
		case "BID_ASK":
			return [][]byte{fakegw.Msg(97, id, 1, 1795640334, 0, 1.09763, 1.09766, 1000000, 9500000, 1)}
		case "MIDPOINT":
			return [][]byte{fakegw.Msg(96, id, 1, 1795640303, 0, 1.097625, 0, 1)}
		default:
			return [][]byte{
				fakegw.Msg(98, id, 1, 1795640339, 2, 355.86, 19, "NASDAQ", " F I", 0),
				fakegw.Msg(98, id, 1, 1795640340, 0, 355.85, 40, "BATS", "", 1),
			}
		}
	})
	srv.Handle(9, func(req []string) [][]byte { return [][]byte{fakegw.Msg(4, 2, req[2], 162, "no permissions", "")} })
	ib := connectFake(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ht, err := ib.ReqHistoricalTicks(ctx, aaplE2E(), "", "20261125 15:59:00 US/Eastern", 100, "TRADES", true, false)
	if err != nil || len(ht.Last) != 2 || ht.Last[1].Exchange != "BATS" {
		t.Fatalf("last ticks = %+v err=%v", ht, err)
	}
	if _, err := ib.ReqHistoricalTicks(ctx, aaplE2E(), "", "", 10, "BID_ASK", true, false); err == nil {
		t.Fatal("missing start/end accepted")
	}
	ht, err = ib.ReqHistoricalTicks(ctx, aaplE2E(), "20261125 09:30:00 US/Eastern", "", 10, "BID_ASK", true, false)
	if err != nil || len(ht.BidAsk) != 1 {
		t.Fatalf("bid/ask = %+v err=%v", ht, err)
	}
	ht, err = ib.ReqHistoricalTicks(ctx, aaplE2E(), "", "20261125-20:59:00", 10, "MIDPOINT", true, false)
	if err != nil || len(ht.MidPoint) != 1 {
		t.Fatalf("midpoint = %+v err=%v", ht, err)
	}
	// QualifyContracts passes non-200 errors through.
	err = ib.QualifyContracts(ctx, contract.Stock("X", "SMART", "USD"))
	var qe *QualifyError
	if !errors.As(err, &qe) || !strings.Contains(qe.Error(), "no permissions") {
		t.Fatalf("err = %v", err)
	}
}

func TestTickEFP(t *testing.T) {
	ib := newTestIB()
	tk := newTickerReq(ib, 3, &contract.Contract{ConID: 1, Symbol: "SPX", SecType: "IND"})
	feedMsg(ib, "47|6|3|38|12.5|12.50|4500.25|30|20261218|0.75|1.25")
	feedMsg(ib, "47|6|3|99|1|1|1|1||1|1") // unknown tick type: ignored
	d := tk.Snapshot()
	if d.BidEfp == nil || d.BidEfp.BasisPoints != 12.5 || d.BidEfp.ImpliedFuture != 4500.25 || d.BidEfp.HoldDays != 30 ||
		d.BidEfp.FutureLastTradeDate != "20261218" || d.BidEfp.DividendsToLastTradeDate != 1.25 {
		t.Fatalf("efp = %+v", d.BidEfp)
	}
}

func TestDisconnectForgetsPnLAndFailedSnapshots(t *testing.T) {
	srv := startFake(t)
	srv.Handle(1, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(4, 2, req[2], 354, "Requested market data is not subscribed.", "")}
	})
	ib := connectFake(t, srv)
	if _, err := ib.ReqMktData(aaplE2E(), "", true, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "snapshot failure released", func() bool {
		ib.state.Mu.RLock()
		defer ib.state.Mu.RUnlock()
		return len(ib.state.ReqID2Ticker) == 0 && srv.Received(1) == 1
	})
	if err := ib.ReqPnL("", ""); err != nil {
		t.Fatal(err)
	}
	ib.Disconnect()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	<-srv.Connected()
	if err := ib.Connect(ctx, "127.0.0.1", srv.Port(), e2eClientID); err != nil {
		t.Fatal(err)
	}
	<-srv.Connected()
	if err := ib.ReqPnL("", ""); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "PnL re-requested after Disconnect", func() bool { return srv.Received(92) == 2 })
}
