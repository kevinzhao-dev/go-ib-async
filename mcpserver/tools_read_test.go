package mcpserver

import (
	"strings"
	"testing"
	"time"

	ibgo "github.com/kevinzhao-dev/go-ib-async"
	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

func TestReadOnlyServerRegistersNoTradingTools(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	for _, name := range []string{"place_order", "cancel_order", "cancel_all_orders", "halt_trading"} {
		if h.srv.toolByName[name] != nil {
			t.Fatalf("%s must not be registered without trading", name)
		}
	}
	for _, tl := range h.srv.tools {
		if !tl.Annotations.ReadOnlyHint {
			t.Fatalf("%s must be read-only on a read-only server", tl.Name)
		}
	}
	st := h.ok("get_status", nil)
	if st["connected"] != true || get(t, st, "accounts", 0, "id") != "DU123456" || get(t, st, "accounts", 0, "paper") != true ||
		get(t, st, "trading", "enabled") != false || st["market_data_type"] != "live" || st["client_id"] != float64(testClientID) {
		t.Fatalf("status = %v", st)
	}
	if !strings.Contains(h.srv.instructions(), "Trading is disabled") {
		t.Fatal("instructions must say trading is disabled")
	}
}

func TestStatusDisconnected(t *testing.T) {
	h := newHarness(t, harnessOpts{offline: true})
	st := h.ok("get_status", nil)
	if st["connected"] != false || len(st["accounts"].([]any)) != 0 {
		t.Fatalf("status = %v", st)
	}
	for _, c := range []struct {
		tool string
		args any
	}{
		{"search_contracts", map[string]any{"contract": map[string]any{"symbol": "AAPL"}}},
		{"get_quote", map[string]any{"contract": map[string]any{"symbol": "AAPL"}}},
		{"get_positions", nil},
		{"get_account_summary", nil},
		{"get_executions", nil},
		{"preview_order", map[string]any{"contract": map[string]any{"symbol": "AAPL"}}},
	} {
		h.fail(c.tool, c.args, "not connected")
	}
}

func TestSearchContracts(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	res := h.ok("search_contracts", map[string]any{"contract": map[string]any{"symbol": "aapl"}, "detail": true})
	c := get(t, res, "contracts", 0)
	if res["count"] != float64(1) || get(t, c, "con_id") != float64(aaplConID) || get(t, c, "long_name") != "AAPL INC" ||
		get(t, c, "primary_exchange") != "NASDAQ" || get(t, c, "min_tick") != 0.01 || get(t, c, "order_types") != "ACTIVETIM,LMT,MKT" {
		t.Fatalf("result = %v", res)
	}
	// Defaults: STK on SMART in USD, symbol upper-cased.
	if req := h.gw.LastRequest(9); req[4] != "AAPL" || req[5] != "STK" || req[10] != "SMART" || req[12] != "USD" {
		t.Fatalf("request = %q", req)
	}

	res = h.ok("search_contracts", map[string]any{"contract": map[string]any{"symbol": "AMBIG"}, "limit": 1})
	if res["count"] != float64(2) || res["truncated"] != true || len(res["contracts"].([]any)) != 1 {
		t.Fatalf("limit: %v", res)
	}
	if c := get(t, res, "contracts", 0); get(t, c, "order_types") != nil {
		t.Fatalf("details only with detail=true: %v", c)
	}
	h.fail("search_contracts", map[string]any{"contract": map[string]any{"symbol": "NOPE"}}, "No security definition")
	h.fail("search_contracts", map[string]any{"contract": map[string]any{"exchange": "SMART"}}, "needs con_id, symbol or local_symbol")
	h.fail("search_contracts", nil, `missing required argument "contract"`)
	h.fail("search_contracts", map[string]any{"contract": map[string]any{"symbol": "AAPL", "sec_type": "XYZ"}}, `"contract.sec_type" must be one of`)
	h.fail("search_contracts", map[string]any{"contract": map[string]any{"symbol": "AAPL"}, "bogus": 1}, "unknown field")
	h.fail("search_contracts", `{"contract": 5}`, "invalid arguments")

	// Futures come from TWS in no particular order (live: ESZ6, ESZ7, ESM7,
	// ESH8, ...). Results and ambiguity candidates are sorted by expiry, so
	// a limit keeps the front months; stocks keep TWS's order.
	h.gw.Handle(9, func(req []string) [][]byte {
		id := req[2]
		if req[4] == "AMBIG" {
			return [][]byte{detailsMsg(id, "AMBIG", "STK", 1002, "ARCA"), detailsMsg(id, "AMBIG", "STK", 1001, "NYSE"), fakegw.Msg(52, 1, id)}
		}
		return [][]byte{
			detailsMsgExpiry(id, "ES", "FUT", 3, "CME", "20271217"),
			detailsMsgExpiry(id, "ES", "FUT", 1, "CME", "20261218"),
			detailsMsgExpiry(id, "ES", "FUT", 2, "CME", "20270319"),
			fakegw.Msg(52, 1, id),
		}
	})
	es := map[string]any{"symbol": "ES", "sec_type": "FUT", "exchange": "CME"}
	res = h.ok("search_contracts", map[string]any{"contract": es, "limit": 2})
	if cs := res["contracts"].([]any); len(cs) != 2 || get(t, cs[0], "expiry") != "20261218" || get(t, cs[1], "expiry") != "20270319" {
		t.Fatalf("futures by expiry = %v", res)
	}
	msg := h.fail("get_quote", map[string]any{"contract": es}, "3 contracts match")
	if a, b := strings.Index(msg, "con_id 1: FUT ES ES 20261218"), strings.Index(msg, "con_id 2: FUT ES ES 20270319"); a < 0 || b < a ||
		strings.Count(msg, "con_id 1") != 1 {
		t.Fatalf("candidates must be by expiry, con_id once each: %s", msg)
	}
	res = h.ok("search_contracts", map[string]any{"contract": map[string]any{"symbol": "AMBIG"}})
	if get(t, res, "contracts", 0, "con_id") != 1002.0 {
		t.Fatalf("stocks keep TWS's order: %v", res)
	}
}

func TestContractDefaults(t *testing.T) {
	c, err := contractSpec{Symbol: "eurusd", SecType: "CASH"}.toContract()
	if err != nil || c.Symbol != "EUR" || c.Currency != "USD" || c.Exchange != "IDEALPRO" {
		t.Fatalf("cash = %+v, %v", c, err)
	}
	c, _ = contractSpec{Symbol: "AAPL", SecType: "OPT", Expiry: "20231020", Strike: 175, Right: "C"}.toContract()
	if c.Exchange != "SMART" || c.Currency != "" || c.Strike != 175 {
		t.Fatalf("opt = %+v", c)
	}
	c, _ = contractSpec{ConID: 5, Exchange: "nyse"}.toContract()
	if c.ConID != 5 || c.SecType != "" || c.Exchange != "NYSE" {
		t.Fatalf("by con_id = %+v", c)
	}
	if d := describeSpec(&contract.Contract{SecType: "OPT", Symbol: "AAPL", Strike: 175, ConID: 9}); d != "OPT AAPL strike 175 con_id 9" {
		t.Fatalf("describe = %q", d)
	}
}

func TestQuoteResolvesAndCaches(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	q := h.ok("get_quote", map[string]any{"contract": map[string]any{"symbol": "AAPL"}})
	if q["bid"] != 150.0 || q["ask"] != 150.1 || q["last"] != 150.05 || q["close"] != 149.0 || q["bid_size"] != 100.0 ||
		q["market_data_type"] != "live" || q["time"] == nil || get(t, q, "contract", "con_id") != float64(aaplConID) {
		t.Fatalf("quote = %v", q)
	}
	if px, ok := h.srv.ReferencePrice(&contract.Contract{ConID: aaplConID}); !ok || px != 150.05 {
		t.Fatalf("reference price = %g, %v (want the midpoint)", px, ok)
	}
	// By con_id: served from the details cache, no second lookup.
	before := h.gw.Received(9)
	h.ok("get_quote", map[string]any{"contract": map[string]any{"con_id": aaplConID}})
	if h.gw.Received(9) != before {
		t.Fatal("con_id lookups must use the details cache")
	}

	q = h.ok("get_quote", map[string]any{"contract": map[string]any{"symbol": "NODATA"}})
	if q["bid"] != nil || !strings.Contains(q["note"].(string), "no prices") {
		t.Fatalf("empty quote = %v", q)
	}
	if _, ok := h.srv.ReferencePrice(&contract.Contract{ConID: noDataConID}); ok {
		t.Fatal("no reference price without prices")
	}
	if _, ok := h.srv.ReferencePrice(nil); ok {
		t.Fatal("nil contract")
	}

	h.fail("get_quote", map[string]any{"contract": map[string]any{"symbol": "AMBIG"}}, "2 contracts match")
	msg := h.fail("get_quote", map[string]any{"contract": map[string]any{"symbol": "AMBIG"}}, "con_id 1002")
	if !strings.Contains(msg, "con_id 1001") {
		t.Fatalf("candidates missing: %s", msg)
	}
	h.fail("get_quote", map[string]any{"contract": map[string]any{"symbol": "EMPTY"}}, "no contract matches")
	h.fail("get_quote", map[string]any{"contract": map[string]any{"symbol": "NOID"}}, "no con_id")
	h.fail("get_quote", map[string]any{"contract": map[string]any{"symbol": "NOPE"}}, "contract lookup failed")
}

func TestQuoteMarketDataError(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.gw.Handle(1, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(4, 2, req[2], 354, "Requested market data is not subscribed", "")}
	})
	h.fail("get_quote", map[string]any{"contract": map[string]any{"symbol": "AAPL"}}, "354")
}

func TestQuoteView(t *testing.T) {
	tk := market.NewTicker(&contract.Contract{ConID: 1, Symbol: "X", SecType: "STK"})
	tk.MarketDataType = 7
	tk.Bid, tk.Ask, tk.Halted = -1, ibgo.UnsetDouble, 1
	v := viewQuote(tk)
	if v.Bid != nil || v.Ask != nil || v.MarketDataType != "unknown" || !v.Halted || v.Note == "" {
		t.Fatalf("view = %+v", v)
	}
	if _, ok := referenceOf(tk); ok {
		t.Fatal("no reference without prices")
	}
	tk.Close = 10
	if px, ok := referenceOf(tk); !ok || px != 10 {
		t.Fatalf("close fallback = %g", px)
	}

	// Live, before the open: bid/ask absent but bid_size/ask_size 0.
	tk.BidSize, tk.AskSize, tk.Last, tk.LastSize = 0, 0, 359.46, 48
	if v := viewQuote(tk); v.BidSize != nil || v.AskSize != nil || v.LastSize == nil || *v.LastSize != 48 {
		t.Fatalf("sizes need their price: %+v", v)
	}
	tk.Bid, tk.BidSize = 359, 0
	if v := viewQuote(tk); v.BidSize == nil || *v.BidSize != 0 {
		t.Fatalf("a quoted bid keeps its size, even 0: %+v", v)
	}
}

func TestHistoricalBars(t *testing.T) {
	h := newHarness(t, harnessOpts{config: func(c *Config) { c.MaxBars = 2 }})
	res := h.ok("get_historical_bars", map[string]any{"contract": map[string]any{"symbol": "AAPL"}, "duration": "1 d", "bar_size": "1 HOUR"})
	if res["count"] != float64(3) || res["truncated"] != true || res["bar_size"] != "1 hour" {
		t.Fatalf("bars = %v", res)
	}
	last := get(t, res, "bars", 1)
	if get(t, last, "o") != 175.9 || get(t, last, "c") != 176.1 || get(t, last, "v") != nil || get(t, last, "n") != float64(10) {
		t.Fatalf("last bar = %v (most recent bars must be kept, -1 volume omitted)", last)
	}
	req := h.gw.LastRequest(20)
	if !contains(req, "1 D") || !contains(req, "1 hour") || !contains(req, "TRADES") {
		t.Fatalf("request = %q", req)
	}

	h.ok("get_historical_bars", map[string]any{"contract": map[string]any{"symbol": "EURUSD", "sec_type": "CASH"}})
	if req := h.gw.LastRequest(20); !contains(req, "MIDPOINT") || !contains(req, "5 D") || !contains(req, "1 day") {
		t.Fatalf("CASH defaults: %q", req)
	}
	h.fail("get_historical_bars", map[string]any{"contract": map[string]any{"symbol": "AAPL"}, "duration": "5 days"}, "duration must look like")
	h.fail("get_historical_bars", map[string]any{"contract": map[string]any{"symbol": "AAPL"}, "bar_size": "7 mins"}, "must be one of")
	h.fail("get_historical_bars", map[string]any{"contract": map[string]any{"symbol": "NOPE"}}, "contract lookup failed")

	// Daily bars are dates (live they read 2026-11-19T00:00:00Z, intraday
	// bars carry the exchange offset), and IB's -1 bar count (MIDPOINT, CASH)
	// is omitted like its -1 volume.
	h.gw.Handle(20, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(17, req[1], "20231018", "20231020", 2,
			"20231019", 1.05, 1.06, 1.04, 1.055, -1.0, -1.0, -1,
			"20231020", 1.055, 1.07, 1.05, 1.06, -1.0, -1.0, -1)}
	})
	for _, size := range []string{"1 day", "1 week"} {
		res = h.ok("get_historical_bars", map[string]any{"contract": map[string]any{"symbol": "EURUSD", "sec_type": "CASH"}, "bar_size": size})
		if b := get(t, res, "bars", 0); get(t, b, "t") != "2023-10-19" || get(t, b, "n") != nil || get(t, b, "v") != nil {
			t.Fatalf("%s bar = %v", size, b)
		}
	}
	res = h.ok("get_historical_bars", map[string]any{"contract": map[string]any{"symbol": "EURUSD", "sec_type": "CASH"}, "bar_size": "1 hour"})
	if b := get(t, res, "bars", 0); !strings.HasPrefix(get(t, b, "t").(string), "2023-10-19T00:00:00") {
		t.Fatalf("intraday bars keep the time: %v", b)
	}

	h.gw.Handle(20, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(4, 2, req[1], 162, "Historical Market Data Service error message:HMDS query returned no data", "")}
	})
	h.fail("get_historical_bars", map[string]any{"contract": map[string]any{"symbol": "AAPL"}}, "HMDS")
}

func TestOptionChain(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	res := h.ok("get_option_chain", map[string]any{"underlying": map[string]any{"symbol": "AAPL"}, "max_expirations": 2, "strike_min": 172, "strike_max": 182})
	chains := res["chains"].([]any)
	if len(chains) != 1 {
		t.Fatalf("chains = %v (default exchange SMART)", res)
	}
	c := chains[0]
	exps, strikes := get(t, c, "expirations").([]any), get(t, c, "strikes").([]any)
	if len(exps) != 2 || exps[0] != "20231020" || exps[1] != "20231027" || get(t, c, "expirations_truncated") != true ||
		len(strikes) != 2 || strikes[0] != 175.0 || strikes[1] != 180.0 || get(t, c, "multiplier") != "100" {
		t.Fatalf("chain = %v", c)
	}
	if req := h.gw.LastRequest(78); req[2] != "AAPL" || req[3] != "" || req[4] != "STK" || req[5] != "265598" {
		t.Fatalf("request = %q", req)
	}
	res = h.ok("get_option_chain", map[string]any{"underlying": map[string]any{"con_id": aaplConID}, "exchange": "*"})
	if len(res["chains"].([]any)) != 2 {
		t.Fatalf("all exchanges = %v", res)
	}
	h.fail("get_option_chain", map[string]any{"underlying": map[string]any{"symbol": "AAPL"}, "exchange": "BOX"}, "available: SMART, CBOE")

	h.ok("get_option_chain", map[string]any{"underlying": map[string]any{"symbol": "ES", "sec_type": "FUT", "exchange": "CME"}, "exchange": "*"})
	if req := h.gw.LastRequest(78); req[3] != "CME" || req[4] != "FUT" {
		t.Fatalf("future underlying request = %q", req)
	}

	// Futures options are listed on the future's exchange, not SMART (live:
	// ES returned 16 chains, all on CME).
	esChains := func(exch string) func([]string) [][]byte {
		return func(req []string) [][]byte {
			id := req[1]
			return [][]byte{
				fakegw.Msg(75, id, exch, 495512563, "ES", "50", 1, "20261218", 2, 6000.0, 6100.0),
				fakegw.Msg(75, id, exch, 495512563, "EW1", "50", 1, "20261204", 1, 6000.0),
				fakegw.Msg(76, id),
			}
		}
	}
	es := map[string]any{"symbol": "ES", "sec_type": "FUT", "exchange": "CME"}
	h.gw.Handle(78, esChains("CME"))
	res = h.ok("get_option_chain", map[string]any{"underlying": es})
	if cs := res["chains"].([]any); len(cs) != 2 || get(t, cs[0], "exchange") != "CME" || get(t, cs[1], "trading_class") != "EW1" {
		t.Fatalf("future default exchange = %v", res)
	}
	if msg := h.fail("get_option_chain", map[string]any{"underlying": es, "exchange": "BOX"}, "available: CME"); strings.Contains(msg, "CME, CME") {
		t.Fatalf("available exchanges must be listed once: %s", msg)
	}
	// A default exchange without chains returns every chain; an explicit one
	// stays an error.
	h.gw.Handle(78, esChains("XCME"))
	if res := h.ok("get_option_chain", map[string]any{"underlying": es}); len(res["chains"].([]any)) != 2 {
		t.Fatalf("fallback to all chains = %v", res)
	}
	h.fail("get_option_chain", map[string]any{"underlying": es, "exchange": "cme"}, "no option chain on exchange CME; available: XCME")
	h.fail("get_option_chain", map[string]any{"underlying": map[string]any{"symbol": "NOPE"}}, "contract lookup failed")
	h.gw.Handle(78, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(4, 2, req[1], 200, "No security definition has been found", "")}
	})
	h.fail("get_option_chain", map[string]any{"underlying": map[string]any{"con_id": aaplConID}}, "option chain request failed")
}

func TestPositionsAndAccountSummary(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.gw.SetPosition(fakegw.Contract{ConID: msftConID, Symbol: "MSFT", SecType: "STK", Exchange: "NASDAQ", Currency: "USD"}, 10, 400)
	h.gw.SetPosition(fakegw.Contract{ConID: aaplConID, Symbol: "AAPL", SecType: "STK", Exchange: "NASDAQ", Currency: "USD"}, -5, 150)
	res := h.ok("get_positions", nil)
	pos := res["positions"].([]any)
	if len(pos) != 2 || get(t, pos[0], "contract", "symbol") != "AAPL" || get(t, pos[0], "position") != -5.0 ||
		get(t, pos[1], "avg_cost") != 400.0 || get(t, pos[1], "account") != "DU123456" {
		t.Fatalf("positions = %v", res)
	}
	if res := h.ok("get_positions", map[string]any{"account": "DU999"}); len(res["positions"].([]any)) != 0 {
		t.Fatalf("account filter = %v", res)
	}

	res = h.ok("get_account_summary", map[string]any{"account": "DU123456"})
	vals := res["values"].([]any)
	if len(vals) != 2 || get(t, vals[0], "tag") != "BuyingPower" || get(t, vals[1], "value") != "100000.50" {
		t.Fatalf("summary = %v", res)
	}
	if req := h.gw.LastRequest(62); !strings.HasPrefix(req[4], "NetLiquidation,TotalCashValue") {
		t.Fatalf("default tags = %q", req)
	}
	h.ok("get_account_summary", map[string]any{"tags": []string{"netliquidation", "Leverage"}})
	if req := h.gw.LastRequest(62); req[4] != "NetLiquidation,Leverage" {
		t.Fatalf("tags = %q", req)
	}
	h.fail("get_account_summary", map[string]any{"tags": []string{"Bogus"}}, `"tags[0]" must be one of`)
	if res := h.ok("get_account_summary", nil); len(res["values"].([]any)) != 3 {
		t.Fatalf("all accounts = %v", res)
	}
}

func TestPositionOrdering(t *testing.T) {
	a := account.Position{Account: "A", Contract: &contract.Contract{Symbol: "X", ConID: 2}}
	b := account.Position{Account: "A", Contract: &contract.Contract{Symbol: "X", ConID: 3}}
	n := account.Position{Account: "A"}
	if !positionLess(a, b) || positionLess(b, a) || !positionLess(n, a) || positionLess(a, n) {
		t.Fatal("ordering")
	}
}

func TestErrorsFromBrokerRequests(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.gw.Handle(62, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(4, 2, req[2], 322, "Error processing request: duplicate", "")}
	})
	h.fail("get_account_summary", nil, "account summary request failed")
	h.gw.Handle(7, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(4, 2, req[2], 322, "Error processing request", "")}
	})
	h.fail("get_executions", nil, "executions request failed")
}

func TestPositionsRequestFails(t *testing.T) {
	h := newHarness(t, harnessOpts{config: func(c *Config) { c.RequestTimeout = 50 * time.Millisecond }})
	h.gw.Handle(61, func([]string) [][]byte { return nil }) // never ends
	h.fail("get_positions", nil, "positions request failed")
}

func TestOrdersAndExecutionsRead(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	con := &contract.Contract{ConID: aaplConID, Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	tr, err := h.ib.PlaceOrder(con, order.LimitOrder("BUY", 10, 150))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "submitted", func() bool { return tr.Status() == order.StatusSubmitted })
	if err := h.gw.Fill(testClientID, tr.Order.OrderID, 4, 149.5); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fill", func() bool { return tr.FilledQty() == 4 })

	res := h.ok("list_orders", nil)
	o := get(t, res, "orders", 0)
	if get(t, o, "order_id") != float64(tr.Order.OrderID) || get(t, o, "own") != true || get(t, o, "status") != "Submitted" ||
		get(t, o, "filled") != 4.0 || get(t, o, "remaining") != 6.0 || get(t, o, "limit_price") != 150.0 || get(t, o, "fills") != nil {
		t.Fatalf("order = %v", o)
	}

	d := h.ok("get_order", map[string]any{"order_id": tr.Order.OrderID})
	if get(t, d, "fills", 0, "price") != 149.5 || get(t, d, "fills", 0, "shares") != 4.0 || len(d["log"].([]any)) < 2 {
		t.Fatalf("detail = %v", d)
	}
	waitFor(t, "perm id", func() bool { return tr.PermID() != 0 })
	if d := h.ok("get_order", map[string]any{"perm_id": tr.PermID()}); d["order_id"] != float64(tr.Order.OrderID) {
		t.Fatalf("by perm id = %v", d)
	}
	h.fail("get_order", map[string]any{"order_id": 999}, "no order 999 of client 7")
	h.fail("get_order", map[string]any{"perm_id": 999}, "no order with perm_id 999")
	h.fail("get_order", nil, "give order_id or perm_id")

	ex := h.ok("get_executions", map[string]any{"symbol": "aapl", "side": "buy"})
	if get(t, ex, "executions", 0, "shares") != 4.0 || get(t, ex, "executions", 0, "commission") != 1.0 {
		t.Fatalf("executions = %v", ex)
	}
	if req := h.gw.LastRequest(7); !contains(req, "AAPL") || !contains(req, "BUY") {
		t.Fatalf("executions filter = %q", req)
	}

	if _, err := h.ib.CancelOrder(tr.Order); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "cancelled", tr.IsDone)
	if res := h.ok("list_orders", nil); len(res["orders"].([]any)) != 0 {
		t.Fatalf("open orders = %v", res)
	}
	all := h.ok("list_orders", map[string]any{"scope": "ALL"})
	if o := get(t, all, "orders", 0); get(t, o, "status") != "Cancelled" || get(t, o, "remaining") != 0.0 {
		t.Fatalf("all orders = %v", all)
	}
}

// list_orders shows manual TWS orders and other clients' orders, whatever
// the gateway's read-only setting (live, they only appeared in read-only
// API mode, through the SyncOrders fallback), and never stale ones.
func TestListOrdersIncludesOtherClients(t *testing.T) {
	limits := defaultLimits()
	limits.MaxOpenOrders = 2
	h := newHarness(t, harnessOpts{trading: true, limits: limits})
	msft := fakegw.Contract{ConID: msftConID, Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	h.gw.AddOrder(fakegw.Order{Contract: msft, Action: "SELL", Qty: 3, OrderType: "STP", AuxPrice: 380, TIF: "GTC", Account: "DU123456"})
	other := h.gw.AddOrder(fakegw.Order{ClientID: 3, OrderID: 12, Contract: msft, Action: "BUY", Qty: 1, OrderType: "LMT", LmtPrice: 390, TIF: "DAY", Account: "DU123456"})
	con := &contract.Contract{ConID: aaplConID, Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	mine, err := h.ib.PlaceOrder(con, order.LimitOrder("BUY", 2, 150))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "submitted", func() bool { return mine.Status() == order.StatusSubmitted })

	res := h.ok("list_orders", nil)
	orders := res["orders"].([]any)
	if len(orders) != 3 || res["note"] != nil ||
		get(t, orders[0], "client_id") != 0.0 || get(t, orders[0], "own") != false || get(t, orders[0], "stop_price") != 380.0 ||
		get(t, orders[1], "client_id") != 3.0 || get(t, orders[1], "order_id") != 12.0 || get(t, orders[1], "own") != false ||
		get(t, orders[2], "order_id") != float64(mine.Order.OrderID) || get(t, orders[2], "own") != true {
		t.Fatalf("orders = %v", res)
	}

	// TWS sends no update when another client's order ends; it just stops
	// reporting it. The stale copy must not be listed, in either scope.
	h.gw.SetOrderStatus(other, "Cancelled")
	for _, scope := range []string{"open", "all"} {
		res = h.ok("list_orders", map[string]any{"scope": scope})
		for _, o := range res["orders"].([]any) {
			if get(t, o, "client_id") == 3.0 {
				t.Fatalf("%s: stale order of another client listed: %v", scope, res)
			}
		}
		if n := len(res["orders"].([]any)); n != 2 {
			t.Fatalf("%s: orders = %v", scope, res)
		}
	}

	// The risk limits count this client's orders only: the foreign orders the
	// session now knows of do not use up max_open_orders (2).
	if _, err := h.ib.PlaceOrder(con, order.LimitOrder("BUY", 3, 150)); err != nil {
		t.Fatalf("second own order: %v", err)
	}
	if _, err := h.ib.PlaceOrder(con, order.LimitOrder("BUY", 4, 150)); err == nil || !strings.Contains(err.Error(), "2 open orders") {
		t.Fatalf("third own order: %v", err)
	}

	short := newHarness(t, harnessOpts{config: func(c *Config) { c.RequestTimeout = 100 * time.Millisecond }})
	short.gw.Handle(16, func([]string) [][]byte { return nil }) // reqAllOpenOrders never ends
	if note, _ := short.ok("list_orders", nil)["note"].(string); !strings.Contains(note, "only this client's orders") {
		t.Fatalf("refresh failure note = %q", note)
	}
	off := newHarness(t, harnessOpts{offline: true})
	if note, _ := off.ok("list_orders", nil)["note"].(string); !strings.Contains(note, "not connected") {
		t.Fatalf("offline note = %q", note)
	}
}

func TestOrderViewFields(t *testing.T) {
	stp := order.StopLimitOrder("SELL", 5, 99, 100)
	stp.OrderID, stp.ClientID = 3, 1
	tr := order.NewTrade(&contract.Contract{ConID: 1}, stp)
	tr.SetStatus(order.StatusCancelled, time.Now(), "Error 201: rejected", 201)
	v := withDetail(viewTrade(tr, 2), tr, nil)
	if v.Own || *v.LimitPrice != 99 || *v.StopPrice != 100 || v.LastError != "Error 201: rejected" || v.Remaining != 0 || len(v.Log) != 1 {
		t.Fatalf("view = %+v", v)
	}
	if viewTrade(tr, 1).Own != true || viewTrade(order.NewTrade(nil, &order.Order{OrderID: 3}), 0).Own {
		t.Fatal("own: same client id and order id > 0, never for client id 0")
	}
	mkt := order.MarketOrder("BUY", 1)
	if v := viewTrade(order.NewTrade(nil, mkt), 0); v.LimitPrice != nil || v.StopPrice != nil || v.Contract != nil {
		t.Fatalf("market view = %+v", v)
	}
	if viewWhatIf(nil) != nil || viewContract(nil) != nil {
		t.Fatal("nil views")
	}
	f := viewFill(order.Fill{Time: time.Unix(5, 0)})
	if f.Time == "" || f.ExecID != "" {
		t.Fatalf("fill without execution = %+v", f)
	}
	if fillTime(order.Fill{Time: time.Unix(5, 0)}) != time.Unix(5, 0) {
		t.Fatal("fill time fallback")
	}
	if positive(-1) != nil || num(ibgo.UnsetDouble) != nil || price(-1) != nil {
		t.Fatal("unset numbers must be omitted")
	}
	if d := viewDetails(contract.ContractDetails{}, false); d.contractView == nil {
		t.Fatal("details without contract")
	}
}
