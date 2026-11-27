package ibgo

// Wire-format tests for orders: placeOrder encoding (field by field, per
// ib_async client.placeOrder), condition encode/decode, and decoding a fully
// loaded openOrder message (per ib_async decoder.openOrder).

import (
	"math"
	"reflect"
	"strconv"
	"testing"

	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
	"github.com/kevinzhao-dev/go-ib-async/internal/state"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/protocol"
)

// capturePlaceOrder places ord on a fake that only records the message.
func capturePlaceOrder(t *testing.T, version int, con *contract.Contract, ord *order.Order) (int64, []string) {
	t.Helper()
	srv, err := fakegw.StartVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	srv.Handle(3, func([]string) [][]byte { return nil })
	ib := connectFake(t, srv)
	tr, err := ib.PlaceOrder(con, ord)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "placeOrder received", func() bool { return srv.Received(3) == 1 })
	return tr.Order.OrderID, srv.LastRequest(3)
}

// plainLimitFields is the reference encoding (server 178) of
// order.LimitOrder("BUY", 100, 150.5) on aaplE2E().
func plainLimitFields(id string) []string {
	return []string{
		"3", id,
		"265598", "AAPL", "STK", "", "0", "", "", "SMART", "", "USD", "AAPL", "NMS", // contract
		"", "", // secIdType, secId
		"BUY", "100", "LMT", "150.5", "", "", "", "", "O", "0", "", "1", // action .. transmit
		"0", "0", "0", "0", "0", "0", "0", // parentId, blockOrder, sweepToFill, displaySize, triggerMethod, outsideRth, hidden
		"", "0", "", "", "", "", "", // sharesAllocation, discretionaryAmt, goodAfterTime, goodTillDate, faGroup, faMethod, faPercentage
		"", "0", "", "-1", "0", "", "", "0", "", "", // modelCode .. percentOffset
		"0", "0", "", "0", "", "", "", "", "", "0", // eTradeOnly .. overridePercentageConstraints
		"", "", "", "", // volatility, volatilityType, deltaNeutralOrderType, deltaNeutralAuxPrice
		"0", "", "", "", "", "", "", // continuousUpdate .. scalePriceIncrement
		"", "", "", "", // scaleTable, activeStartTime, activeStopTime, hedgeType
		"0", "", "", "0", // optOutSmartRouting, clearingAccount, clearingIntent, notHeld
		"0",                        // no delta-neutral contract
		"",                         // algoStrategy
		"", "0", "", "0", "0", "0", // algoId, whatIf, orderMiscOptions, solicited, randomizeSize, randomizePrice
		"0",                             // conditions
		"", "", "", "", "", "", "0", "", // adjustedOrderType .. extOperator
		"", "", "", // softDollarTier name/val, cashQty
		"", "", "", "", // mifid2 fields
		"0", "0", "0", "0", // dontUseAutoPriceForHedge .. usePriceMgmtAlgo
		"", "", "0", "", "", // duration(158), postToAts(160), autoCancelParent(162), advancedErrorOverride(166), manualOrderTime(169)
	}
}

func TestPlaceOrderEncodingPlainLimit(t *testing.T) {
	id, got := capturePlaceOrder(t, 178, aaplE2E(), order.LimitOrder("BUY", 100, 150.5))
	if want := plainLimitFields(strconv.FormatInt(id, 10)); !reflect.DeepEqual(got, want) {
		t.Fatalf("placeOrder fields mismatch\n got (%d): %q\nwant (%d): %q", len(got), got, len(want), want)
	}
}

func TestPlaceOrderEncodingVersionBranches(t *testing.T) {
	// faPercentage is field index 39; faProfile follows it below v177.
	const afterFaPercentage = 40

	id, got := capturePlaceOrder(t, 176, aaplE2E(), order.LimitOrder("BUY", 100, 150.5))
	want := plainLimitFields(strconv.FormatInt(id, 10))
	want = append(want[:afterFaPercentage:afterFaPercentage], append([]string{""}, want[afterFaPercentage:]...)...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("v176 (with faProfile)\n got: %q\nwant: %q", got, want)
	}

	id, got = capturePlaceOrder(t, 157, aaplE2E(), order.LimitOrder("BUY", 100, 150.5))
	want = plainLimitFields(strconv.FormatInt(id, 10))
	want = append(want[:afterFaPercentage:afterFaPercentage], append([]string{""}, want[afterFaPercentage:]...)...)
	want = want[:len(want)-5] // no duration/postToAts/autoCancelParent/advancedErrorOverride/manualOrderTime
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("v157\n got: %q\nwant: %q", got, want)
	}
}

// walker asserts consecutive segments of a message.
type walker struct {
	t   *testing.T
	f   []string
	pos int
}

func (w *walker) eq(what string, vals ...string) {
	w.t.Helper()
	if w.pos+len(vals) > len(w.f) {
		w.t.Fatalf("%s: message too short at %d", what, w.pos)
	}
	if got := w.f[w.pos : w.pos+len(vals)]; !reflect.DeepEqual(got, vals) {
		w.t.Fatalf("%s at field %d:\n got %q\nwant %q", what, w.pos, got, vals)
	}
	w.pos += len(vals)
}

func (w *walker) skip(n int) { w.pos += n }

func (w *walker) end() {
	w.t.Helper()
	if w.pos != len(w.f) {
		w.t.Fatalf("message has %d trailing fields: %q", len(w.f)-w.pos, w.f[w.pos:])
	}
}

func TestPlaceOrderEncodingComplexOrder(t *testing.T) {
	bag := &contract.Contract{
		Symbol: "SPY", SecType: "BAG", Exchange: "IBKRATS", Currency: "USD",
		ComboLegs: []contract.ComboLeg{
			{ConID: 756733, Ratio: 1, Action: "BUY", Exchange: "SMART", ExemptCode: -1},
			{ConID: 320227571, Ratio: 2, Action: "SELL", Exchange: "SMART", OpenClose: 1, ShortSaleSlot: 2, DesignatedLocation: "X", ExemptCode: -1},
		},
		DeltaNeutralContract: &contract.DeltaNeutralContract{ConID: 9999, Delta: 0.6, Price: 101.5},
	}
	o := order.NewOrder()
	o.Action, o.TotalQuantity, o.OrderType = "BUY", 2, "PEG MID"
	o.TIF, o.OcaGroup, o.Account, o.OrderRef = "DAY", "oca1", "DU1", "ref-1"
	o.Volatility = 0.3 // must be stripped for non-VOL orders
	o.OrderComboLegs = []order.OrderComboLeg{{Price: 1.5}}
	o.SmartComboRoutingParams = []contract.TagValue{{Tag: "NonGuaranteed", Value: "1"}}
	o.DeltaNeutralOrderType, o.DeltaNeutralAuxPrice = "LMT", 0.5
	o.DeltaNeutralConID, o.DeltaNeutralSettlingFirm, o.DeltaNeutralClearingAccount = 9999, "SF", "CA"
	o.DeltaNeutralClearingIntent, o.DeltaNeutralOpenClose, o.DeltaNeutralShortSale = "IB", "O", true
	o.DeltaNeutralShortSaleSlot, o.DeltaNeutralDesignatedLocation = 2, "DL"
	o.ScaleInitLevelSize, o.ScaleSubsLevelSize, o.ScalePriceIncrement = 100, 50, 0.05
	o.ScalePriceAdjustValue, o.ScalePriceAdjustInterval, o.ScaleProfitOffset = 0.01, 60, 0.1
	o.ScaleAutoReset, o.ScaleInitPosition, o.ScaleInitFillQty, o.ScaleRandomPercent = true, 0, 0, false
	o.HedgeType, o.HedgeParam = "D", "0.5"
	o.AlgoStrategy = "Adaptive"
	o.AlgoParams = []contract.TagValue{{Tag: "adaptivePriority", Value: "Normal"}}
	price := order.CreateCondition(1).(*order.PriceCondition)
	price.IsMore, price.Price, price.ConID, price.Exchange, price.TriggerMethod = true, 450.5, 756733, "SMART", 2
	tm := order.CreateCondition(3).(*order.TimeCondition)
	tm.SetConjunction("o")
	tm.Time = "20231019 15:00:00"
	o.Conditions = []order.OrderCondition{price, tm}
	o.ConditionsIgnoreRTH = true
	o.MinTradeQty = 5
	o.MidOffsetAtWhole, o.MidOffsetAtHalf = 0.01, 0.005

	id, f := capturePlaceOrder(t, 178, bag, o)
	if o.Volatility != UnsetDouble {
		t.Fatal("volatility must be stripped for non-VOL orders")
	}
	w := &walker{t: t, f: f}
	w.eq("header", "3", strconv.FormatInt(id, 10))
	w.eq("contract", "0", "SPY", "BAG", "", "0", "", "", "IBKRATS", "", "USD", "", "")
	w.eq("secId", "", "")
	w.eq("order head", "BUY", "2", "PEG MID", "", "", "DAY", "oca1", "DU1", "O", "0", "ref-1", "1", "0", "0", "0", "0", "0", "0", "0")
	w.eq("combo legs", "2", "756733", "1", "BUY", "SMART", "0", "0", "", "-1", "320227571", "2", "SELL", "SMART", "1", "2", "X", "-1")
	w.eq("order combo legs", "1", "1.5")
	w.eq("smart combo params", "1", "NonGuaranteed", "1")
	w.skip(7)  // sharesAllocation .. faPercentage
	w.skip(20) // modelCode .. overridePercentageConstraints
	w.eq("volatility stripped", "", "")
	w.eq("delta neutral", "LMT", "0.5", "9999", "SF", "CA", "IB", "O", "1", "2", "DL")
	w.eq("scale", "0", "", "", "", "100", "50", "0.05", "0.01", "60", "0.1", "1", "0", "0", "0")
	w.eq("scale table/active times/hedge", "", "", "", "D", "0.5")
	w.eq("routing", "0", "", "", "0")
	w.eq("delta neutral contract", "1", "9999", "0.6", "101.5")
	w.eq("algo", "Adaptive", "1", "adaptivePriority", "Normal")
	w.eq("misc", "", "0", "", "0", "0", "0")
	w.eq("conditions", "2",
		"1", "a", "1", "450.5", "756733", "SMART", "2",
		"3", "o", "0", "20231019 15:00:00",
		"1", "0")
	w.skip(8 + 3 + 4 + 4) // adjusted .. extOperator, soft dollar + cashQty, mifid2, flags
	w.eq("version tail", "", "", "0", "", "")
	w.eq("IBKRATS minTradeQty", "5")
	w.eq("PEG MID offsets", "0.01", "0.005")
	w.end()
}

func TestPlaceOrderEncodingPegBestAndPegBench(t *testing.T) {
	o := order.NewOrder()
	o.Action, o.TotalQuantity, o.OrderType = "SELL", 1, "PEG BEST"
	o.MinCompeteSize, o.CompeteAgainstBestOffset = 100, math.Inf(1)
	o.MidOffsetAtWhole, o.MidOffsetAtHalf = 0.02, 0.01
	_, f := capturePlaceOrder(t, 178, aaplE2E(), o)
	if tail := f[len(f)-4:]; !reflect.DeepEqual(tail, []string{"100", "Infinite", "0.02", "0.01"}) {
		t.Fatalf("PEG BEST tail = %q", tail)
	}

	b := order.NewOrder()
	b.Action, b.TotalQuantity, b.OrderType = "BUY", 1, "PEG BENCH"
	b.ReferenceContractID, b.IsPeggedChangeAmountDecrease = 1111, true
	b.PeggedChangeAmount, b.ReferenceChangeAmount, b.ReferenceExchangeID = 0.5, 0.2, "ISLAND"
	_, f = capturePlaceOrder(t, 178, aaplE2E(), b)
	base := plainLimitFields("x")
	// The PEG BENCH block sits right before the conditions count, which is
	// followed by 24 fixed fields (adjusted .. manualOrderTime).
	idx := len(base) - 25
	if got := f[idx : idx+5]; !reflect.DeepEqual(got, []string{"1111", "1", "0.5", "0.2", "ISLAND"}) {
		t.Fatalf("PEG BENCH block = %q (fields %q)", got, f)
	}
}

func TestConditionEncodeDecodeRoundTrip(t *testing.T) {
	ib := newTestIB()
	mk := func(typ int, fill func(order.OrderCondition)) order.OrderCondition {
		c := order.CreateCondition(typ)
		fill(c)
		return c
	}
	conds := []order.OrderCondition{
		mk(1, func(c order.OrderCondition) {
			p := c.(*order.PriceCondition)
			p.IsMore, p.Price, p.ConID, p.Exchange, p.TriggerMethod = true, 450.5, 756733, "SMART", 2
		}),
		mk(3, func(c order.OrderCondition) {
			tc := c.(*order.TimeCondition)
			tc.SetConjunction("o")
			tc.Time = "20231019 15:00:00"
		}),
		mk(4, func(c order.OrderCondition) { m := c.(*order.MarginCondition); m.IsMore, m.Percent = true, 30 }),
		mk(5, func(c order.OrderCondition) {
			e := c.(*order.ExecutionCondition)
			e.SecType, e.Exchange, e.Symbol = "STK", "SMART", "SPY"
		}),
		mk(6, func(c order.OrderCondition) {
			v := c.(*order.VolumeCondition)
			v.IsMore, v.Volume, v.ConID, v.Exchange = true, 100000, 756733, "SMART"
		}),
		mk(7, func(c order.OrderCondition) {
			p := c.(*order.PercentChangeCondition)
			p.SetConjunction("o")
			p.ChangePercent, p.ConID, p.Exchange = 2.5, 756733, "SMART"
		}),
	}
	for _, c := range conds {
		vals := encodeCondition(c)
		fields := make([]string, len(vals))
		for i, v := range vals {
			fields[i] = protocol.EncodeField(v)
		}
		r := protocol.NewFieldReader(fields)
		decoded := order.CreateCondition(int(r.ReadInt()))
		ib.readConditionFields(decoded, r)
		if !reflect.DeepEqual(decoded, c) || r.HasMore() {
			t.Fatalf("condition %d round trip:\n got %+v\nwant %+v", c.CondType(), decoded, c)
		}
	}
	if encodeCondition(nil) != nil {
		t.Fatal("unknown condition must encode to nil")
	}
}

// loadedOpenOrder is an openOrder message (server 178) that exercises every
// optional block of the decoder.
func loadedOpenOrder() []string {
	return []string{
		"5", "42",
		"28812380", "SPY", "BAG", "", "0", "", "", "SMART", "USD", "", "",
		"BUY", "3", "PEG BENCH", "1.25", "0", "GTC", "oca1", "DU1", "O", "0", "ref-1",
		"7", "123456", "1", "0", "0", "", "", "", "", "",
		"", "", "", "", "", "0", "", "-1", "0", "", "", "", "", "", "0", "0", "0", "0", "", "3", "0", "0", "",
		"0", "0", "", "", "LMT", "0",
		"9999", "SF", "CA", "IB", "O", "1", "2", "DL",
		"0", "", "", "", "", "", "SPY+QQQ",
		"2", "756733", "1", "BUY", "SMART", "0", "0", "", "-1", "320227571", "2", "SELL", "SMART", "1", "2", "X", "-1",
		"2", "1.5", "",
		"1", "NonGuaranteed", "1",
		"100", "50", "0.05",
		"0.01", "60", "0.1", "1", "0", "0", "0",
		"D", "0.5",
		"0", "", "", "0", "1", "9999", "0.6", "101.5",
		"Adaptive", "1", "adaptivePriority", "Normal",
		"0", "0", "Submitted", "", "", "", "", "", "", "", "", "", "", "", "", "", "",
		"0", "0",
		"1111", "1", "0.5", "0.2", "ISLAND",
		"6",
		"1", "a", "1", "450.5", "756733", "SMART", "2",
		"3", "o", "0", "20231019 15:00:00",
		"4", "a", "1", "30",
		"5", "a", "STK", "SMART", "SPY",
		"6", "a", "1", "100000", "756733", "SMART",
		"7", "o", "0", "2.5", "756733", "SMART",
		"1", "0",
		"STP", "440", "", "", "439", "", "", "0",
		"", "", "",
		"", "0", "0", "0", "0",
		"", "", "0",
		"", "", "", "", "",
	}
}

func TestOpenOrderDecodesAllBlocks(t *testing.T) {
	ib := newTestIB()
	var opened *order.Trade // dispatch runs synchronously on this goroutine
	ib.OpenOrderEvent.Subscribe(func(tr *order.Trade) { opened = tr })
	dispatch(ib, loadedOpenOrder()...)

	tr := ib.state.Trades[state.OrderKey{ClientID: 7, OrderID: 42}]
	if tr == nil || opened != tr || ib.state.PermID2Trade[123456] != tr {
		t.Fatal("openOrder must register the trade and fire OpenOrderEvent")
	}
	o, c := tr.Order, tr.Contract
	if o.Action != "BUY" || o.TotalQuantity != 3 || o.LmtPrice != 1.25 || o.TIF != "GTC" || o.OcaType != 3 || !o.OutsideRTH || o.OrderRef != "ref-1" {
		t.Fatalf("order head = %+v", o)
	}
	if o.DeltaNeutralConID != 9999 || o.DeltaNeutralDesignatedLocation != "DL" || !o.DeltaNeutralShortSale || o.DeltaNeutralShortSaleSlot != 2 {
		t.Fatalf("delta neutral = %+v", o)
	}
	if c.ComboLegsDescrip != "SPY+QQQ" || len(c.ComboLegs) != 2 || c.ComboLegs[1].ConID != 320227571 || c.ComboLegs[1].Ratio != 2 ||
		c.ComboLegs[1].DesignatedLocation != "X" || c.ComboLegs[1].ExemptCode != -1 {
		t.Fatalf("combo legs = %+v", c.ComboLegs)
	}
	if len(o.OrderComboLegs) != 2 || o.OrderComboLegs[0].Price != 1.5 || len(o.SmartComboRoutingParams) != 1 {
		t.Fatalf("order combo legs = %+v / %+v", o.OrderComboLegs, o.SmartComboRoutingParams)
	}
	if o.ScalePriceIncrement != 0.05 || o.ScalePriceAdjustInterval != 60 || !o.ScaleAutoReset || o.HedgeType != "D" || o.HedgeParam != "0.5" {
		t.Fatalf("scale/hedge = %+v", o)
	}
	if dnc := c.DeltaNeutralContract; dnc == nil || dnc.ConID != 9999 || dnc.Delta != 0.6 || dnc.Price != 101.5 {
		t.Fatalf("delta neutral contract = %+v", dnc)
	}
	if o.AlgoStrategy != "Adaptive" || len(o.AlgoParams) != 1 || o.AlgoParams[0].Value != "Normal" {
		t.Fatalf("algo = %+v", o.AlgoParams)
	}
	if o.ReferenceContractID != 1111 || !o.IsPeggedChangeAmountDecrease || o.ReferenceExchangeID != "ISLAND" {
		t.Fatalf("peg bench = %+v", o)
	}
	if len(o.Conditions) != 6 || !o.ConditionsIgnoreRTH || o.ConditionsCancelOrder {
		t.Fatalf("conditions = %d ignoreRth %v", len(o.Conditions), o.ConditionsIgnoreRTH)
	}
	if p := o.Conditions[0].(*order.PriceCondition); p.Price != 450.5 || p.TriggerMethod != 2 || !p.IsMore {
		t.Fatalf("price condition = %+v", p)
	}
	if v := o.Conditions[4].(*order.VolumeCondition); v.Volume != 100000 || v.Exchange != "SMART" {
		t.Fatalf("volume condition = %+v", v)
	}
	if pc := o.Conditions[5].(*order.PercentChangeCondition); pc.ChangePercent != 2.5 || pc.GetConjunction() != "o" {
		t.Fatalf("percent condition = %+v", pc)
	}
	if o.AdjustedOrderType != "STP" || o.TriggerPrice != 440 || o.AdjustedStopPrice != 439 {
		t.Fatalf("adjusted = %s %g %g", o.AdjustedOrderType, o.TriggerPrice, o.AdjustedStopPrice)
	}
	if tr.Status() != order.StatusSubmitted {
		t.Fatalf("status = %s", tr.Status())
	}
	if next := ib.client.GetReqID(); next < 43 {
		t.Fatalf("next order id %d must exceed seen order ids", next)
	}

	// A second openOrder for the same order updates the trade in place.
	msg := loadedOpenOrder()
	msg[14] = "5"    // totalQuantity
	msg[16] = "1.30" // lmtPrice
	dispatch(ib, msg...)
	if ib.state.Trades[state.OrderKey{ClientID: 7, OrderID: 42}] != tr || tr.Order.TotalQuantity != 5 || tr.Order.LmtPrice != 1.30 {
		t.Fatal("openOrder update must modify the existing trade")
	}
}

func TestOpenOrderForManualOrderKeyedByPermID(t *testing.T) {
	ib := newTestIB()
	msg := loadedOpenOrder()
	msg[1] = "0"  // manual TWS order: no API order id
	msg[24] = "0" // clientId 0
	dispatch(ib, msg...)
	if ib.state.Trades[state.OrderKey{ClientID: 0, OrderID: 123456}] == nil {
		t.Fatal("manual orders must be keyed by permId")
	}
}
