package ibgo

// Unit tests for inbound message decoding and the request/response public
// API. Message layouts follow ib_async's decoder.py at server version 178.

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
	"github.com/kevinzhao-dev/go-ib-async/internal/state"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/protocol"
)

// dispatch feeds one message through the real dispatcher (msgId first).
func dispatch(ib *IB, fields ...string) {
	r := protocol.NewFieldReader(fields)
	r.Skip(1)
	id, _ := strconv.Atoi(fields[0])
	ib.handleMessage(id, r)
}

func newTestIB() *IB {
	ib := New()
	ib.client.SetServerVersion(178)
	return ib
}

func TestTickMessagesUpdateTicker(t *testing.T) {
	ib := newTestIB()
	tk := market.NewTicker(aaplE2E())
	ib.state.ReqID2Ticker[5] = tk

	var updates, pendingBatches atomic.Int32
	tk.UpdateEvent.Subscribe(func(*market.Ticker) { updates.Add(1) })
	ib.PendingTickersEvent.Subscribe(func(ts []*market.Ticker) {
		if len(ts) == 1 && ts[0] == tk {
			pendingBatches.Add(1)
		}
	})

	ib.client.OnDataArrived()
	dispatch(ib, "1", "6", "5", "1", "150.25", "100", "0") // bid
	dispatch(ib, "1", "6", "5", "1", "150.30", "120", "0") // bid again
	dispatch(ib, "1", "6", "5", "2", "150.35", "90", "0")  // ask
	dispatch(ib, "1", "6", "5", "4", "150.32", "10", "0")  // last
	dispatch(ib, "1", "6", "5", "6", "151.00", "0", "0")   // high (price map)
	dispatch(ib, "1", "6", "5", "66", "149.90", "5", "0")  // delayed bid
	dispatch(ib, "2", "6", "5", "0", "130")                // bid size
	dispatch(ib, "2", "6", "5", "3", "95")                 // ask size
	dispatch(ib, "2", "6", "5", "5", "12")                 // last size
	dispatch(ib, "2", "6", "5", "8", "123456")             // volume (size map)
	dispatch(ib, "45", "6", "5", "49", "1")                // halted (generic)
	dispatch(ib, "46", "6", "5", "32", "QZ")               // bid exchange (string map)
	dispatch(ib, "58", "1", "5", "3")                      // delayed market data
	dispatch(ib, "1", "6", "999", "1", "1", "1", "0")      // unknown ticker: ignored
	dispatch(ib, "2", "6", "999", "0", "1")
	dispatch(ib, "45", "6", "999", "49", "1")
	dispatch(ib, "46", "6", "999", "32", "x")

	if tk.Bid != 149.90 || tk.PrevBid != 150.30 || tk.BidSize != 130 || tk.PrevBidSize != 5 {
		t.Fatalf("bid %g prev %g size %g prevSize %g", tk.Bid, tk.PrevBid, tk.BidSize, tk.PrevBidSize)
	}
	if tk.Ask != 150.35 || tk.AskSize != 95 || tk.PrevAskSize != 90 {
		t.Fatalf("ask %g size %g prev %g", tk.Ask, tk.AskSize, tk.PrevAskSize)
	}
	if tk.Last != 150.32 || tk.LastSize != 12 || tk.PrevLastSize != 10 {
		t.Fatalf("last %g size %g prev %g", tk.Last, tk.LastSize, tk.PrevLastSize)
	}
	if tk.High != 151 || tk.Volume != 123456 || tk.Halted != 1 || tk.BidExchange != "QZ" || tk.MarketDataType != 3 {
		t.Fatalf("high %g vol %g halted %g bidEx %q mdt %d", tk.High, tk.Volume, tk.Halted, tk.BidExchange, tk.MarketDataType)
	}
	if len(ib.state.GetPendingTickers()) != 1 {
		t.Fatal("ticker must be pending until the batch is processed")
	}

	ib.client.OnDataProcessed()
	if updates.Load() != 1 || pendingBatches.Load() != 1 {
		t.Fatalf("updates=%d pending batches=%d, want 1/1", updates.Load(), pendingBatches.Load())
	}
	if len(ib.state.GetPendingTickers()) != 0 || tk.Time.IsZero() {
		t.Fatal("pending tickers must be cleared and stamped")
	}
}

// Every tick map entry must name a real Ticker field that the setter handles.
func TestTickMapsMapToTickerFields(t *testing.T) {
	check := func(name string, m map[int]string, set func(*market.Ticker, string), want func(reflect.Value) bool) {
		for tickType, field := range m {
			tk := market.NewTicker(nil)
			set(tk, field)
			v := reflect.ValueOf(tk).Elem().FieldByName(field)
			if !v.IsValid() {
				t.Errorf("%s[%d] = %q: no such Ticker field", name, tickType, field)
				continue
			}
			if !want(v) {
				t.Errorf("%s[%d] = %q: setter does not handle the field", name, tickType, field)
			}
		}
	}
	isFloat42 := func(v reflect.Value) bool { return v.Kind() == reflect.Float64 && v.Float() == 42 }
	setFloat := func(tk *market.Ticker, f string) { setTickerFloat(tk, f, 42) }
	check("PriceTickMap", state.PriceTickMap, setFloat, isFloat42)
	check("SizeTickMap", state.SizeTickMap, setFloat, isFloat42)
	check("GenericTickMap", state.GenericTickMap, setFloat, isFloat42)
	check("StringTickMap", state.StringTickMap, func(tk *market.Ticker, f string) { setTickerString(tk, f, "x") },
		func(v reflect.Value) bool { return v.Kind() == reflect.String && v.String() == "x" })

	// The special-cased bid/ask/last names must work through the setter too.
	tk := market.NewTicker(nil)
	for _, f := range []string{"Bid", "Ask", "Last", "BidSize", "AskSize", "LastSize", "VWAP"} {
		setTickerFloat(tk, f, 7)
		if reflect.ValueOf(tk).Elem().FieldByName(f).Float() != 7 {
			t.Errorf("setTickerFloat(%q) not handled", f)
		}
	}
	setTickerFloat(tk, "NoSuchField", 1) // must be a no-op
	setTickerString(tk, "NoSuchField", "x")
}

func contractDetailsMsg(reqID, secType, lastTimes string) []byte {
	return fakegw.Msg(10, reqID, "AAPL", secType, lastTimes, 0.0, "", "SMART", "USD", "AAPL", "NMS", "NMS", 265598, 0.01,
		"", "ACTIVETIM,LMT,MKT", "SMART,NASDAQ", 1, 0, `APPLE INC`, "NASDAQ", "202312", "Technology", "Computers", "Computers",
		"US/Eastern", "20231019:0930-20231019:1600", "20231019:0930-20231019:1600", "", 0,
		1, "ISIN", "US0378331005",
		1, "", "", "26,26", "", "COMMON", 0.0001, 0.0001, 100)
}

func TestReqContractDetails(t *testing.T) {
	srv := startFake(t)
	srv.Handle(9, func(req []string) [][]byte {
		id := req[2]
		return [][]byte{
			contractDetailsMsg(id, "STK", ""),
			contractDetailsMsg(id, "OPT", "20240119 16:00 America/New_York"),
			contractDetailsMsg(id, "FUT", "20240315-09:30-US/Central"),
			fakegw.Msg(52, 1, id),
		}
	})
	ib := connectFake(t, srv)

	details, err := ib.ReqContractDetails(ctx5s(t), contract.Stock("AAPL", "SMART", "USD"))
	if err != nil {
		t.Fatal(err)
	}
	if len(details) != 3 {
		t.Fatalf("details = %d, want 3", len(details))
	}
	d := details[0]
	c := d.Contract
	if c.ConID != 265598 || c.Symbol != "AAPL" || c.PrimaryExchange != "NASDAQ" || c.TradingClass != "NMS" || c.LocalSymbol != "AAPL" {
		t.Fatalf("contract = %+v", c)
	}
	if d.MinTick != 0.01 || d.LongName != "APPLE INC" || d.MarketName != "NMS" || d.OrderTypes != "ACTIVETIM,LMT,MKT" ||
		d.Industry != "Technology" || d.MarketRuleIDs != "26,26" || d.StockType != "COMMON" || d.SuggestedSizeIncrement != 100 ||
		d.MinSize != 0.0001 || d.PriceMagnifier != 1 || d.ContractMonth != "202312" {
		t.Fatalf("details = %+v", d)
	}
	if len(d.SecIDList) != 1 || d.SecIDList[0].Tag != "ISIN" || d.SecIDList[0].Value != "US0378331005" {
		t.Fatalf("secIdList = %+v", d.SecIDList)
	}
	if o := details[1]; o.Contract.LastTradeDateOrContractMonth != "20240119" || o.LastTradeTime != "16:00" || o.TimeZoneID != "America/New_York" {
		t.Fatalf("space-separated lastTimes: %q %q %q", o.Contract.LastTradeDateOrContractMonth, o.LastTradeTime, o.TimeZoneID)
	}
	if f := details[2]; f.Contract.LastTradeDateOrContractMonth != "20240315" || f.LastTradeTime != "09:30" || f.TimeZoneID != "US/Central" {
		t.Fatalf("dash-separated lastTimes: %q %q %q", f.Contract.LastTradeDateOrContractMonth, f.LastTradeTime, f.TimeZoneID)
	}
	if req := srv.LastRequest(9); len(req) < 3 || req[4] != "AAPL" {
		t.Fatalf("request = %q", req)
	}
}

func TestReqContractDetailsError(t *testing.T) {
	srv := startFake(t)
	srv.Handle(9, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(4, 2, req[2], 200, "No security definition has been found for the request", "")}
	})
	ib := connectFake(t, srv)
	var evt atomic.Pointer[IBError]
	ib.ErrorEvent.Subscribe(func(e *IBError) { evt.Store(e) })

	con := contract.Stock("NOPE", "SMART", "USD")
	_, err := ib.ReqContractDetails(ctx5s(t), con)
	var re *RequestError
	if !errors.As(err, &re) || re.Code != 200 || !strings.Contains(re.Error(), "200") {
		t.Fatalf("err = %v, want RequestError 200", err)
	}
	if e := evt.Load(); e == nil || e.Contract != con || !strings.Contains(e.Error(), "No security definition") {
		t.Fatalf("error event = %+v", e)
	}
}

func TestReqHistoricalData(t *testing.T) {
	srv := startFake(t)
	srv.Handle(20, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(17, req[1], "20231019 09:30:00", "20231019 16:00:00", 2,
			"20231019 09:30:00 US/Eastern", 174.5, 175.0, 174.0, 174.8, 1200.5, 174.6, 300,
			"20231019 10:30:00 US/Eastern", 174.8, 176.0, 174.7, 175.9, 900.0, 175.4, 250)}
	})
	ib := connectFake(t, srv)

	bars, err := ib.ReqHistoricalData(ctx5s(t), aaplE2E(), "", "1 D", "1 hour", "TRADES", true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 2 {
		t.Fatalf("bars = %d", len(bars))
	}
	b := bars[0]
	ny, _ := time.LoadLocation("America/New_York")
	if !b.Date.Equal(time.Date(2023, 10, 19, 9, 30, 0, 0, ny)) {
		t.Fatalf("bar date = %v (intraday dates with zone must parse)", b.Date)
	}
	if b.Open != 174.5 || b.High != 175 || b.Low != 174 || b.Close != 174.8 || b.Volume != 1200.5 || b.Average != 174.6 || b.BarCount != 300 {
		t.Fatalf("bar = %+v", b)
	}
}

func TestReqHistoricalDataCancelledOnTimeout(t *testing.T) {
	srv := startFake(t)
	srv.Handle(20, func([]string) [][]byte { return nil }) // never answers
	ib := connectFake(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := ib.ReqHistoricalData(ctx, aaplE2E(), "", "1 D", "1 hour", "TRADES", true, 1)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	waitFor(t, "cancelHistoricalData", func() bool { return srv.Received(25) == 1 })
	if ib.state.Requests.Len() != 0 {
		t.Fatal("timed-out request must be removed")
	}
}

func TestHistoricalDataUpdateAppendsBar(t *testing.T) {
	ib := newTestIB()
	bdl := &market.BarDataList{ReqID: 3}
	ib.state.ReqID2Subscriber[3] = bdl
	var got atomic.Int32
	bdl.UpdateEvent.Subscribe(func(*market.BarDataList) { got.Add(1) })

	// historicalDataUpdate: reqId, barCount, date, open, close, high, low, average, volume
	dispatch(ib, "90", "3", "12", "1697722200", "10", "11", "12", "9", "10.5", "1000")
	dispatch(ib, "90", "77", "1", "1697722200", "1", "1", "1", "1", "1", "1") // unknown subscriber
	if len(bdl.Bars) != 1 || got.Load() != 1 {
		t.Fatalf("bars=%d events=%d", len(bdl.Bars), got.Load())
	}
	b := bdl.Bars[0]
	if b.BarCount != 12 || b.Open != 10 || b.Close != 11 || b.High != 12 || b.Low != 9 || b.Average != 10.5 || b.Volume != 1000 ||
		!b.Date.Equal(time.Unix(1697722200, 0)) {
		t.Fatalf("bar = %+v", b)
	}
}

func TestReqAccountSummaryCancelsSubscription(t *testing.T) {
	srv := startFake(t)
	srv.Handle(62, func(req []string) [][]byte {
		id := req[2]
		return [][]byte{
			fakegw.Msg(63, 1, id, "DU1", "NetLiquidation", "100000.50", "USD"),
			fakegw.Msg(63, 1, id, "DU1", "BuyingPower", "400000", "USD"),
			fakegw.Msg(64, 1, id),
		}
	})
	ib := connectFake(t, srv)
	vals, err := ib.ReqAccountSummary(ctx5s(t), "All", "NetLiquidation,BuyingPower")
	if err != nil {
		t.Fatal(err)
	}
	if len(vals) != 2 || vals[0].Tag != "NetLiquidation" || vals[0].Value != "100000.50" || vals[0].Account != "DU1" || vals[1].Currency != "USD" {
		t.Fatalf("values = %+v", vals)
	}
	waitFor(t, "cancelAccountSummary", func() bool { return srv.Received(63) == 1 })
	if req := srv.LastRequest(63); req[2] != srv.LastRequest(62)[2] {
		t.Fatalf("cancel for wrong request: %q vs %q", req, srv.LastRequest(62))
	}
}

func TestReqSecDefOptParams(t *testing.T) {
	srv := startFake(t)
	srv.Handle(78, func(req []string) [][]byte {
		id := req[1]
		return [][]byte{
			fakegw.Msg(75, id, "SMART", 265598, "AAPL", "100", 2, "20231020", "20231027", 3, 170.0, 175.0, 180.0),
			fakegw.Msg(75, id, "CBOE", 265598, "AAPL", "100", 1, "20231020", 1, 175.0),
			fakegw.Msg(76, id),
		}
	})
	ib := connectFake(t, srv)
	chains, err := ib.ReqSecDefOptParams(ctx5s(t), "AAPL", "", "STK", 265598)
	if err != nil {
		t.Fatal(err)
	}
	if len(chains) != 2 {
		t.Fatalf("chains = %d", len(chains))
	}
	c := chains[0]
	if c.Exchange != "SMART" || c.UnderlyingConID != 265598 || c.TradingClass != "AAPL" || c.Multiplier != "100" ||
		!reflect.DeepEqual(c.Expirations, []string{"20231020", "20231027"}) || !reflect.DeepEqual(c.Strikes, []float64{170, 175, 180}) {
		t.Fatalf("chain = %+v", c)
	}
}

func TestReqRealTimeBarsAndCancel(t *testing.T) {
	srv := startFake(t)
	srv.Handle(50, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(50, 3, req[2], 1697722200, 174.0, 175.0, 173.5, 174.5, 1000.0, 174.2, 12)}
	})
	ib := connectFake(t, srv)
	rtbl, err := ib.ReqRealTimeBars(aaplE2E(), 5, "TRADES", true)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "real-time bar", func() bool {
		ib.state.Mu.RLock()
		defer ib.state.Mu.RUnlock()
		return len(rtbl.Bars) == 1
	})
	ib.state.Mu.RLock()
	b := rtbl.Bars[0]
	ib.state.Mu.RUnlock()
	if !b.Time.Equal(time.Unix(1697722200, 0)) || b.Open != 174 || b.High != 175 || b.Low != 173.5 || b.Close != 174.5 || b.WAP != 174.2 || b.Count != 12 {
		t.Fatalf("bar = %+v", b)
	}
	ib.CancelRealTimeBars(rtbl)
	waitFor(t, "cancelRealTimeBars", func() bool { return srv.Received(51) == 1 })
	ib.state.Mu.RLock()
	_, still := ib.state.ReqID2Subscriber[rtbl.ReqID]
	ib.state.Mu.RUnlock()
	if still {
		t.Fatal("subscription must be removed")
	}
}

func TestReqMktDataSnapshot(t *testing.T) {
	srv := startFake(t)
	srv.Handle(1, func(req []string) [][]byte {
		id := req[2]
		return [][]byte{
			fakegw.Msg(1, 6, id, 1, 150.0, 100.0, 0),
			fakegw.Msg(1, 6, id, 2, 150.1, 200.0, 0),
			fakegw.Msg(57, 1, id),
		}
	})
	ib := connectFake(t, srv)
	tk, err := ib.ReqMktData(aaplE2E(), "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "snapshot end", func() bool { return ib.state.Requests.Len() == 0 })
	if tk.Bid != 150 || tk.Ask != 150.1 || tk.BidSize != 100 || tk.AskSize != 200 {
		t.Fatalf("ticker bid %g/%g ask %g/%g", tk.Bid, tk.BidSize, tk.Ask, tk.AskSize)
	}
	ib.CancelMktData(aaplE2E()) // matched by contract key (conId)
	waitFor(t, "cancelMktData", func() bool { return srv.Received(2) == 1 })
}

func TestAccountValuesAndPortfolio(t *testing.T) {
	ib := newTestIB()
	var events atomic.Int32
	ib.AccountValueEvent.Subscribe(func(*account.AccountValue) { events.Add(1) })

	dispatch(ib, "6", "2", "NetLiquidation", "100000", "USD", "DU1")
	dispatch(ib, "6", "2", "NetLiquidation", "100500", "USD", "DU1") // replaces
	dispatch(ib, "6", "2", "CashBalance", "5000", "USD", "DU1")
	dispatch(ib, "54", "1", "DU1") // accountDownloadEnd: no-op
	vals := ib.AccountValues()
	if len(vals) != 2 || events.Load() != 3 {
		t.Fatalf("values=%+v events=%d", vals, events.Load())
	}
	for _, v := range vals {
		if v.Tag == "NetLiquidation" && v.Value != "100500" {
			t.Fatalf("NetLiquidation = %s", v.Value)
		}
	}

	port := func(pos string) {
		dispatch(ib, "7", "8", "265598", "AAPL", "STK", "", "0", "", "", "NASDAQ", "USD", "AAPL", "NMS",
			pos, "175.5", "17550", "150.25", "2525", "10", "DU1")
	}
	port("100")
	items := ib.state.GetPortfolio("DU1")
	if len(items) != 1 {
		t.Fatalf("portfolio = %+v", items)
	}
	p := items[0]
	if p.Position != 100 || p.MarketPrice != 175.5 || p.MarketValue != 17550 || p.AverageCost != 150.25 || p.UnrealizedPNL != 2525 ||
		p.RealizedPNL != 10 || p.Contract.PrimaryExchange != "NASDAQ" || p.Contract.TradingClass != "NMS" {
		t.Fatalf("portfolio item = %+v", p)
	}
	port("0")
	if len(ib.state.GetPortfolio("DU1")) != 0 || len(ib.state.GetPortfolio("nobody")) != 0 {
		t.Fatal("flat portfolio item must be removed")
	}
}

func TestOrderBoundAndCurrentTime(t *testing.T) {
	ib := newTestIB()
	o := order.LimitOrder("BUY", 1, 1)
	o.OrderID, o.ClientID = 12, 3
	tr := &order.Trade{Contract: aaplE2E(), Order: o, OrderStatus: &order.OrderStatus{Status: order.StatusSubmitted}}
	ib.state.Trades[state.OrderKey{ClientID: 3, OrderID: 12}] = tr

	dispatch(ib, "100", "555666", "3", "12")
	dispatch(ib, "100", "0", "3", "12") // ignored
	if tr.PermID() != 555666 || ib.state.PermID2Trade[555666] != tr {
		t.Fatal("orderBound must link the permId")
	}
	dispatch(ib, "49", "1", "1697722200")
	if ib.serverTime.Load() != 1697722200 {
		t.Fatalf("server time = %d", ib.serverTime.Load())
	}
	dispatch(ib, "9999", "1") // unknown message types are ignored
}

func TestStateAccessorsAndFills(t *testing.T) {
	srv := startFake(t)
	ib := connectFake(t, srv)
	tr, err := ib.PlaceOrder(aaplE2E(), order.LimitOrder("BUY", 3, 10))
	if err != nil {
		t.Fatal(err)
	}
	waitStatus(t, tr, order.StatusSubmitted)
	if err := srv.Fill(e2eClientID, tr.Order.OrderID, 3, 10); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, tr, order.StatusFilled)
	waitFor(t, "fills", func() bool { return len(ib.Fills()) == 1 })
	if f := ib.Fills()[0]; f.Execution.Shares != 3 || f.Contract.Symbol != "AAPL" {
		t.Fatalf("fill = %+v", f)
	}
	if len(ib.Trades()) != 1 || len(ib.OpenTrades()) != 0 {
		t.Fatal("trade bookkeeping")
	}
}

func TestErrorTypes(t *testing.T) {
	e := &IBError{ReqID: 4, Code: 201, Message: "rejected"}
	if !strings.Contains(e.Error(), "201") || !strings.Contains(e.Error(), "rejected") {
		t.Fatal(e.Error())
	}
	re := &RequestError{ReqID: 4, Code: 162, Message: "pacing"}
	if !strings.Contains(re.Error(), "162") || !strings.Contains(re.Error(), "pacing") {
		t.Fatal(re.Error())
	}
	for _, sentinel := range []error{ErrNotConnected, ErrDisconnected, ErrTimeout, ErrRequestCancelled, ErrOrderDone, ErrUseWhatIfOrder} {
		if sentinel.Error() == "" {
			t.Fatal("sentinel without message")
		}
	}
	if UnsetDouble != math.MaxFloat64 || UnsetInteger != math.MaxInt32 {
		t.Fatal("sentinels changed")
	}
}
