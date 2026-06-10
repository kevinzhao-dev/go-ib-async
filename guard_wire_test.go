package ibgo

// Wire-format guardrail tests.
//
// Every fixture below is a "golden wire message": the exact field sequence a
// real TWS/Gateway sends, derived from the reference implementation
// (ib_async decoder.py, github.com/ib-api-reloaded/ib_async), NOT from this
// library's handler code. Provenance for each fixture is quoted in a comment.
//
// Rules for adding fixtures:
//  1. Derive the field list from ib_async's decoder (or ibapi's decoder.py),
//     never from our own handler — otherwise the test just mirrors our bugs.
//  2. Always end with assertFullyConsumed: a handler must consume exactly the
//     fields the wire provides. Overflow (reads past the end) or an unread
//     tail both indicate a field-offset bug, which silently corrupts every
//     subsequent field.
//  3. Build fixtures for ServerVersion 178 (this library's MaxClientVersion)
//     unless testing a version-gated branch.

import (
	"strconv"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/state"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/protocol"
)

// dispatchWire feeds one raw wire message (msgId first, as delivered by
// Connection.ReadMessage) through the production dispatch path and returns
// the reader so the caller can verify full consumption.
func dispatchWire(t *testing.T, ib *IB, fields []string) *protocol.FieldReader {
	t.Helper()
	msgID, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("bad msgId %q: %v", fields[0], err)
	}
	r := protocol.NewFieldReader(fields)
	r.Skip(1) // msgId, as in Client.readLoop
	ib.handleMessage(msgID, r)
	return r
}

func assertFullyConsumed(t *testing.T, r *protocol.FieldReader) {
	t.Helper()
	if r.Overflow > 0 {
		t.Fatalf("decoder read %d field(s) PAST the end of the message — decoder expects more fields than the wire provides", r.Overflow)
	}
	if r.Pos != len(r.Fields) {
		t.Fatalf("decoder consumed %d of %d fields — unread tail %q", r.Pos, len(r.Fields), r.Fields[r.Pos:])
	}
}

func newTestIB(serverVersion int) *IB {
	ib := New()
	ib.client.ServerVersion = serverVersion
	return ib
}

// seedTrade installs a trade keyed by (clientID, orderID) and permID.
func seedTrade(ib *IB, clientID, orderID, permID int64, status string) *order.Trade {
	con := contract.Stock("AAPL", "SMART", "USD")
	con.ConID = 265598
	ord := order.NewOrder()
	ord.OrderID = orderID
	ord.ClientID = clientID
	ord.PermID = permID
	ord.TotalQuantity = 100
	trade := &order.Trade{
		Contract:    con,
		Order:       ord,
		OrderStatus: &order.OrderStatus{OrderID: orderID, Status: status},
	}
	ib.state.Mu.Lock()
	ib.state.Trades[state.OrderKey{ClientID: clientID, OrderID: orderID}] = trade
	if permID != 0 {
		ib.state.PermID2Trade[permID] = trade
	}
	ib.state.Mu.Unlock()
	return trade
}

// seedTicker installs a ticker for a market data reqID.
func seedTicker(ib *IB, reqID int64) *market.Ticker {
	con := contract.Stock("AAPL", "SMART", "USD")
	ticker := market.NewTicker(con)
	ib.state.Mu.Lock()
	ib.state.ReqID2Ticker[reqID] = ticker
	ib.state.Mu.Unlock()
	return ticker
}

// --- execDetails (msgId 11) ---
//
// ib_async decoder.py execDetails — NO version field (servers >= 147; this
// library requires >= 157):
//
//	(_, reqId, ex.orderId,
//	 c.conId, c.symbol, c.secType, c.lastTradeDateOrContractMonth, c.strike,
//	 c.right, c.multiplier, c.exchange, c.currency, c.localSymbol, c.tradingClass,
//	 ex.execId, timeStr, ex.acctNumber, ex.exchange, ex.side, ex.shares, ex.price,
//	 ex.permId, ex.clientId, ex.liquidation, ex.cumQty, ex.avgPrice, ex.orderRef,
//	 ex.evRule, ex.evMultiplier, ex.modelCode, ex.lastLiquidity, *fields) = fields
//	if serverVersion >= 178: ex.pendingPriceRevision
func TestWireGoldenExecDetails(t *testing.T) {
	ib := newTestIB(178)
	trade := seedTrade(ib, 1, 42, 999, order.StatusSubmitted)

	fields := []string{
		"11",     // msgId
		"-1",     // reqId (-1 = unsolicited live fill)
		"42",     // orderId
		"265598", // contract.conId
		"AAPL", "STK", "", "0", "", "", // symbol, secType, lastTradeDate, strike, right, multiplier
		"ISLAND", "USD", "AAPL", "NMS", // exchange, currency, localSymbol, tradingClass
		"0000e0d5.681c.01.01",            // execId
		"20260610 09:30:01", "DU123456", // time, acctNumber
		"ISLAND", "BOT", // exchange, side
		"50", "150.25", // shares, price
		"999", "1", // permId, clientId
		"0", "50", "150.25", // liquidation, cumQty, avgPrice
		"", "", "0", "", "1", // orderRef, evRule, evMultiplier, modelCode, lastLiquidity
		"0", // pendingPriceRevision (serverVersion >= 178)
	}
	r := dispatchWire(t, ib, fields)
	assertFullyConsumed(t, r)

	if len(trade.Fills) != 1 {
		t.Fatalf("trade should have 1 fill, got %d", len(trade.Fills))
	}
	exec := trade.Fills[0].Execution
	if exec.OrderID != 42 {
		t.Errorf("OrderID = %d, want 42", exec.OrderID)
	}
	if exec.ExecID != "0000e0d5.681c.01.01" {
		t.Errorf("ExecID = %q", exec.ExecID)
	}
	if exec.Side != "BOT" {
		t.Errorf("Side = %q, want BOT", exec.Side)
	}
	if exec.Shares != 50 {
		t.Errorf("Shares = %g, want 50", exec.Shares)
	}
	if exec.Price != 150.25 {
		t.Errorf("Price = %g, want 150.25", exec.Price)
	}
	if exec.PermID != 999 {
		t.Errorf("PermID = %d, want 999", exec.PermID)
	}
	if exec.CumQty != 50 {
		t.Errorf("CumQty = %g, want 50", exec.CumQty)
	}
	if exec.AvgPrice != 150.25 {
		t.Errorf("AvgPrice = %g, want 150.25", exec.AvgPrice)
	}
	if exec.LastLiquidity != 1 {
		t.Errorf("LastLiquidity = %d, want 1", exec.LastLiquidity)
	}
	c := trade.Fills[0].Contract
	if c.ConID != 265598 || c.Symbol != "AAPL" || c.LocalSymbol != "AAPL" || c.TradingClass != "NMS" {
		t.Errorf("contract = conId %d symbol %q localSymbol %q tradingClass %q", c.ConID, c.Symbol, c.LocalSymbol, c.TradingClass)
	}
}

// --- position (msgId 61) ---
//
// ib_async decoder.py position — version field present, contract INCLUDES
// localSymbol and tradingClass:
//
//	(_, _, account,
//	 c.conId, c.symbol, c.secType, c.lastTradeDateOrContractMonth, c.strike,
//	 c.right, c.multiplier, c.exchange, c.currency, c.localSymbol, c.tradingClass,
//	 position, avgCost) = fields
func TestWireGoldenPosition(t *testing.T) {
	ib := newTestIB(178)

	fields := []string{
		"61", "3", // msgId, version
		"DU123456", // account
		"265598", "AAPL", "STK", "", "0", "", "", // conId, symbol, secType, lastTradeDate, strike, right, multiplier
		"NASDAQ", "USD", "AAPL", "NMS", // exchange, currency, localSymbol, tradingClass
		"100", "150.5", // position, avgCost
	}
	r := dispatchWire(t, ib, fields)
	assertFullyConsumed(t, r)

	positions := ib.Positions()
	if len(positions) != 1 {
		t.Fatalf("expected 1 position, got %d", len(positions))
	}
	p := positions[0]
	if p.Position != 100 {
		t.Errorf("Position = %g, want 100", p.Position)
	}
	if p.AvgCost != 150.5 {
		t.Errorf("AvgCost = %g, want 150.5", p.AvgCost)
	}
	if p.Contract.LocalSymbol != "AAPL" || p.Contract.TradingClass != "NMS" {
		t.Errorf("contract localSymbol %q tradingClass %q, want AAPL/NMS", p.Contract.LocalSymbol, p.Contract.TradingClass)
	}
}

// --- orderStatus (msgId 3) ---
//
// ib_async decoder.py: wrap("orderStatus",
//	[int, str, float, float, float, int, int, float, int, str, float], skip=1)
// skip=1 → msgId only, NO version field.
func TestWireGoldenOrderStatus(t *testing.T) {
	ib := newTestIB(178)
	trade := seedTrade(ib, 1, 42, 999, order.StatusPendingSubmit)

	fields := []string{
		"3",  // msgId
		"42", // orderId
		"Submitted",
		"0", "100", "0", // filled, remaining, avgFillPrice
		"999", "0", "0", // permId, parentId, lastFillPrice
		"1", "", "0", // clientId, whyHeld, mktCapPrice
	}
	r := dispatchWire(t, ib, fields)
	assertFullyConsumed(t, r)

	if trade.OrderStatus.Status != order.StatusSubmitted {
		t.Errorf("status = %q, want Submitted", trade.OrderStatus.Status)
	}
	if trade.OrderStatus.Remaining != 100 {
		t.Errorf("remaining = %g, want 100", trade.OrderStatus.Remaining)
	}
}

// --- commissionReport (msgId 59) ---
//
// ib_async decoder.py commissionReport:
//	(_, _, execId, commission, currency, realizedPNL, yield_, yieldRedemptionDate) = fields
func TestWireGoldenCommissionReport(t *testing.T) {
	ib := newTestIB(178)
	trade := seedTrade(ib, 1, 42, 999, order.StatusSubmitted)
	exec := &order.Execution{ExecID: "0000e0d5.681c.01.01", PermID: 999}
	fill := &order.Fill{Contract: trade.Contract, Execution: exec, CommissionReport: &order.CommissionReport{}}
	ib.state.Mu.Lock()
	ib.state.Fills[exec.ExecID] = fill
	ib.state.Mu.Unlock()

	fields := []string{
		"59", "1", // msgId, version
		"0000e0d5.681c.01.01",       // execId
		"1.25", "USD",               // commission, currency
		"1.7976931348623157E308",    // realizedPNL (UNSET_DOUBLE)
		"1.7976931348623157E308",    // yield (UNSET_DOUBLE)
		"0",                          // yieldRedemptionDate
	}
	r := dispatchWire(t, ib, fields)
	assertFullyConsumed(t, r)

	if fill.CommissionReport.Commission != 1.25 {
		t.Errorf("commission = %g, want 1.25", fill.CommissionReport.Commission)
	}
	if fill.CommissionReport.RealizedPNL != 0 {
		t.Errorf("unset realizedPNL should map to 0, got %g", fill.CommissionReport.RealizedPNL)
	}
}

// --- errMsg (msgId 4) ---
//
// ib_async decoder.py error: (_, _, reqId, errorCode, errorString, *fields)
// For serverVersion >= 166 the tail contains advancedOrderRejectJson.
func TestWireGoldenErrMsg(t *testing.T) {
	ib := newTestIB(178)

	got := make(chan *IBError, 1)
	ib.ErrorEvent.Subscribe(func(e *IBError) { got <- e })

	fields := []string{
		"4", "2", // msgId, version
		"7",                       // reqId
		"201",                     // errorCode
		"Order rejected - reason", // errorString
		"",                        // advancedOrderRejectJson (serverVersion >= 166)
	}
	r := dispatchWire(t, ib, fields)
	assertFullyConsumed(t, r)

	select {
	case e := <-got:
		if e.Code != 201 || e.ReqID != 7 {
			t.Errorf("error = code %d reqId %d, want 201/7", e.Code, e.ReqID)
		}
	default:
		t.Fatal("ErrorEvent not emitted")
	}
}

// --- tickPrice (msgId 1) ---
//
// ib_async decoder.py priceSizeTick: (_, _, reqId, tickType, price, size, _) = fields
// (msgId, version, ..., trailing attrib mask)
func TestWireGoldenTickPrice(t *testing.T) {
	ib := newTestIB(178)
	ticker := seedTicker(ib, 17)

	fields := []string{
		"1", "6", // msgId, version
		"17",             // reqId
		"1",              // tickType BID
		"150.10", "200", // price, size
		"1", // attrib mask
	}
	r := dispatchWire(t, ib, fields)
	assertFullyConsumed(t, r)

	if ticker.Bid != 150.10 || ticker.BidSize != 200 {
		t.Errorf("bid = %g/%g, want 150.10/200", ticker.Bid, ticker.BidSize)
	}
}

// --- tickSize (msgId 2) ---
//
// ib_async decoder.py: wrap("tickSize", [int, int, float]) skip=2 (msgId+version).
func TestWireGoldenTickSize(t *testing.T) {
	ib := newTestIB(178)
	ticker := seedTicker(ib, 17)

	fields := []string{"2", "6", "17", "0", "300"} // BID_SIZE
	r := dispatchWire(t, ib, fields)
	assertFullyConsumed(t, r)

	if ticker.BidSize != 300 {
		t.Errorf("bidSize = %g, want 300", ticker.BidSize)
	}
}

// --- tickGeneric (msgId 45) ---
//
// ib_async decoder.py: wrap("tickGeneric", [int, int, float]) skip=2.
func TestWireGoldenTickGeneric(t *testing.T) {
	ib := newTestIB(178)
	ticker := seedTicker(ib, 17)

	fields := []string{"45", "6", "17", "49", "1"} // HALTED = 1
	r := dispatchWire(t, ib, fields)
	assertFullyConsumed(t, r)

	if ticker.Halted != 1 {
		t.Errorf("halted = %g, want 1", ticker.Halted)
	}
}

// --- tickString (msgId 46) ---
//
// ib_async decoder.py: wrap("tickString", [int, int, str]) skip=2.
// tickType 45 = lastTimestamp (unix seconds).
func TestWireGoldenTickStringTimestamp(t *testing.T) {
	ib := newTestIB(178)
	ticker := seedTicker(ib, 17)

	fields := []string{"46", "6", "17", "45", "1718000000"}
	r := dispatchWire(t, ib, fields)
	assertFullyConsumed(t, r)

	want := time.Unix(1718000000, 0)
	if !ticker.LastTimestamp.Equal(want) {
		t.Errorf("lastTimestamp = %v, want %v", ticker.LastTimestamp, want)
	}
}

// --- updateAccountValue (msgId 6) ---
//
// ib_async decoder.py: wrap("updateAccountValue", [str, str, str, str]) skip=2.
func TestWireGoldenUpdateAccountValue(t *testing.T) {
	ib := newTestIB(178)

	fields := []string{"6", "2", "NetLiquidation", "100000.00", "USD", "DU123456"}
	r := dispatchWire(t, ib, fields)
	assertFullyConsumed(t, r)

	values := ib.AccountValues()
	if len(values) != 1 {
		t.Fatalf("expected 1 account value, got %d", len(values))
	}
	if values[0].Tag != "NetLiquidation" || values[0].Value != "100000.00" {
		t.Errorf("got %+v", values[0])
	}
}

// --- accountSummary (msgId 63) + accountSummaryEnd (msgId 64) ---
//
// ib_async decoder.py: wrap("accountSummary", [int, str, str, str, str]) skip=2,
// wrap("accountSummaryEnd", [int]) skip=2.
func TestWireGoldenAccountSummary(t *testing.T) {
	ib := newTestIB(178)
	ch := ib.state.StartReq(9001, nil)

	r := dispatchWire(t, ib, []string{"63", "1", "9001", "DU123456", "NetLiquidation", "100000.00", "USD"})
	assertFullyConsumed(t, r)
	r = dispatchWire(t, ib, []string{"64", "1", "9001"})
	assertFullyConsumed(t, r)

	select {
	case result := <-ch:
		if result.Err != nil {
			t.Fatalf("unexpected error: %v", result.Err)
		}
		items := result.Value.([]interface{})
		if len(items) != 1 {
			t.Fatalf("expected 1 row, got %d", len(items))
		}
	default:
		t.Fatal("accountSummaryEnd did not complete the request")
	}
}

// --- updatePortfolio (msgId 7) ---
//
// ib_async decoder.py updatePortfolio:
//	(_, _, c.conId, c.symbol, c.secType, c.lastTradeDateOrContractMonth,
//	 c.strike, c.right, c.multiplier, c.primaryExchange, c.currency,
//	 c.localSymbol, c.tradingClass, position, marketPrice, marketValue,
//	 averageCost, unrealizedPNL, realizedPNL, accountName) = fields
func TestWireGoldenUpdatePortfolio(t *testing.T) {
	ib := newTestIB(178)

	fields := []string{
		"7", "8", // msgId, version
		"265598", "AAPL", "STK", "", "0", "", "", // conId..multiplier
		"NASDAQ", "USD", "AAPL", "NMS", // primaryExchange, currency, localSymbol, tradingClass
		"100", "151.0", "15100.0", "150.5", "50.0", "0.0", // position..realizedPNL
		"DU123456", // accountName
	}
	r := dispatchWire(t, ib, fields)
	assertFullyConsumed(t, r)

	items := ib.state.GetPortfolio("DU123456")
	if len(items) != 1 {
		t.Fatalf("expected 1 portfolio item, got %d", len(items))
	}
	if items[0].Position != 100 || items[0].MarketPrice != 151.0 || items[0].AverageCost != 150.5 {
		t.Errorf("got %+v", items[0])
	}
}

// --- realtimeBar (msgId 50) ---
//
// ib_async decoder.py: wrap("realtimeBar",
//	[int, int, float, float, float, float, float, float, int]) skip=2.
func TestWireGoldenRealtimeBar(t *testing.T) {
	ib := newTestIB(178)
	rtbl := &market.RealTimeBarList{ReqID: 5001}
	ib.state.Mu.Lock()
	ib.state.ReqID2Subscriber[5001] = rtbl
	ib.state.Mu.Unlock()

	fields := []string{
		"50", "3", "5001",
		"1718000000",                          // time
		"1.0", "2.0", "0.5", "1.5",            // O H L C
		"1000", "1.2", "42",                   // volume, wap, count
	}
	r := dispatchWire(t, ib, fields)
	assertFullyConsumed(t, r)

	if len(rtbl.Bars) != 1 {
		t.Fatalf("expected 1 bar, got %d", len(rtbl.Bars))
	}
	b := rtbl.Bars[0]
	if b.Open != 1.0 || b.High != 2.0 || b.Low != 0.5 || b.Close != 1.5 || b.Volume != 1000 || b.Count != 42 {
		t.Errorf("bar = %+v", b)
	}
}

// --- historicalData (msgId 17) ---
//
// ib_async decoder.py historicalData — NO version field for servers >= 124:
//	reqId, startDateStr, endDateStr, numBars,
//	then per bar: date, open, high, low, close, volume, average, barCount
func TestWireGoldenHistoricalData(t *testing.T) {
	ib := newTestIB(178)
	ch := ib.state.StartReq(8001, nil)

	fields := []string{
		"17", "8001",
		"20260601 09:30:00", "20260610 16:00:00", // start, end
		"2", // numBars
		"20260609", "100", "101", "99", "100.5", "5000", "100.2", "120",
		"20260610", "100.5", "102", "100", "101.5", "6000", "101.1", "130",
	}
	r := dispatchWire(t, ib, fields)
	assertFullyConsumed(t, r)

	select {
	case result := <-ch:
		bars := result.Value.([]market.BarData)
		if len(bars) != 2 {
			t.Fatalf("expected 2 bars, got %d", len(bars))
		}
		if bars[0].Open != 100 || bars[0].Close != 100.5 || bars[0].BarCount != 120 {
			t.Errorf("bar[0] = %+v", bars[0])
		}
		if bars[1].Date != time.Date(2026, 6, 10, 0, 0, 0, 0, time.UTC) {
			t.Errorf("bar[1].Date = %v", bars[1].Date)
		}
	default:
		t.Fatal("historicalData did not complete the request")
	}
}
