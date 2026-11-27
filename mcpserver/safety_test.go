package mcpserver

// Regression tests for the safety review of the trading tools.

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	ibgo "github.com/kevinzhao-dev/go-ib-async"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/risk"
)

func collarLimits() risk.Limits {
	return risk.Limits{MaxOrderQuantity: 100, MaxOrderNotional: 100000, PriceCollarPct: 5}
}

func bracketBuy(qty, entry, tp, sl float64) map[string]any {
	return map[string]any{"contract": aapl(), "action": "BUY", "quantity": qty, "limit_price": entry, "take_profit_price": tp, "stop_loss_price": sl}
}

func TestNewVerifiesTradingHooks(t *testing.T) {
	ib := ibgo.New()
	rm := risk.New(defaultLimits(), ib)
	cfg := Config{Trading: TradingConfig{Enabled: true, Risk: rm}}
	if _, err := New(ib, cfg); err == nil || !strings.Contains(err.Error(), "SetRiskManager") {
		t.Fatalf("risk manager not installed: %v", err)
	}
	ib.SetRiskManager(risk.New(defaultLimits(), ib))
	if _, err := New(ib, cfg); err == nil || !strings.Contains(err.Error(), "SetRiskManager") {
		t.Fatalf("another risk manager installed: %v", err)
	}
	ib.SetRiskManager(rm)
	if _, err := New(ib, cfg); err == nil || !strings.Contains(err.Error(), "SetJournal") {
		t.Fatalf("no journal: %v", err)
	}
	ib.SetJournal(failingJournal{})
	if _, err := New(ib, cfg); err != nil {
		t.Fatal(err)
	}
	for _, l := range []risk.Limits{
		{MaxOrderQuantity: math.NaN(), MaxOrderNotional: 1000},
		{MaxOrderQuantity: 10, MaxOrderNotional: math.Inf(1)},
	} {
		nan := risk.New(l, ib)
		ib.SetRiskManager(nan)
		if _, err := New(ib, Config{Trading: TradingConfig{Enabled: true, Risk: nan}}); err == nil || !strings.Contains(err.Error(), "finite") {
			t.Fatalf("limits %+v: %v", l, err)
		}
	}
}

func TestPlaceRechecksHooks(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	id := h.previewID(limitBuy(1, 150))
	h.ib.SetJournal(nil)
	sent := h.gw.Received(3)
	h.fail("place_order", map[string]any{"preview_id": id}, "order not sent: trading needs an order journal")
	if h.gw.Received(3) != sent {
		t.Fatal("order sent without a journal")
	}
}

// A preview may be placed after the quote it cached has expired: the quote
// is refreshed so the collar still applies.
func TestPlaceRefreshesStaleQuote(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true, limits: collarLimits(),
		config: func(c *Config) { c.QuoteMaxAge = 40 * time.Millisecond }})
	id := h.previewID(limitBuy(1, 200)) // 33% above the 150.05 quote
	time.Sleep(60 * time.Millisecond)
	quotes := h.gw.Received(1)
	h.fail("place_order", map[string]any{"preview_id": id}, `"price_collar"`)
	if h.gw.Received(1) != quotes+1 {
		t.Fatal("place_order must refresh the expired quote")
	}

	// A market order confirmed late gets a fresh reference instead of
	// failing the notional check.
	m := newHarness(t, harnessOpts{trading: true, config: func(c *Config) {
		c.QuoteMaxAge = 40 * time.Millisecond
		c.Trading.OrderTypes = []string{"LMT", "MKT"}
	}})
	id = m.previewID(map[string]any{"contract": aapl(), "action": "SELL", "quantity": 1, "order_type": "MKT"})
	time.Sleep(60 * time.Millisecond)
	m.ok("place_order", map[string]any{"preview_id": id, "wait_seconds": 1})
}

func TestCollarFailsClosedWithoutQuote(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true, limits: collarLimits()})
	pv := h.ok("preview_order", map[string]any{"contract": map[string]any{"symbol": "NODATA"}, "action": "BUY", "quantity": 1, "limit_price": 10})
	if w := strings.Join(toStrings(pv["warnings"]), "\n"); !strings.Contains(w, "price collar cannot check the limit price") {
		t.Fatalf("warnings = %q", w)
	}
	sent := h.gw.Received(3)
	h.fail("place_order", map[string]any{"preview_id": pv["preview_id"]}, "so the price collar cannot check the limit price. Nothing was sent")
	if h.gw.Received(3) != sent {
		t.Fatal("unchecked limit order was sent")
	}
}

func TestPreviewWarnsAboutEveryBracketLeg(t *testing.T) {
	l := collarLimits()
	l.MaxOrderNotional = 155
	h := newHarness(t, harnessOpts{trading: true, limits: l})
	pv := h.ok("preview_order", bracketBuy(1, 150, 160, 145))
	w := strings.Join(toStrings(pv["warnings"]), "\n")
	for _, want := range []string{
		"take_profit leg: limit 160 is 6.63% from the reference price 150.05, more than price_collar_pct 5",
		"take_profit leg: estimated notional 160.00 exceeds max_order_notional 155.00",
	} {
		if !strings.Contains(w, want) {
			t.Fatalf("warnings %q missing %q", w, want)
		}
	}
	if strings.Contains(w, "entry leg") || strings.Contains(w, "stop_loss leg") {
		t.Fatalf("only the take-profit leg breaks a limit: %q", w)
	}
	h.fail("place_order", map[string]any{"preview_id": pv["preview_id"]}, `"order_notional": notional 160.00`)

	// The collar alone, as with the binary's defaults.
	c := newHarness(t, harnessOpts{trading: true, limits: collarLimits()})
	pv = c.ok("preview_order", bracketBuy(1, 150, 160, 145))
	if w := strings.Join(toStrings(pv["warnings"]), "\n"); !strings.Contains(w, "take_profit leg: limit 160") {
		t.Fatalf("warnings = %q", w)
	}
	c.fail("place_order", map[string]any{"preview_id": pv["preview_id"]}, `"price_collar"`)
}

func TestBracketCountsAllLegsAsOpenOrders(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true, limits: risk.Limits{MaxOrderQuantity: 100, MaxOrderNotional: 100000, MaxOpenOrders: 2}})
	id := h.previewID(bracketBuy(1, 150, 160, 145))
	sent := h.gw.Received(3)
	h.fail("place_order", map[string]any{"preview_id": id}, `"open_orders": 2 open orders, max 2`)
	if h.gw.Received(3) != sent {
		t.Fatal("no leg may be sent")
	}
}

func TestCancelAllIncludesChildrenAfterPartialFill(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	placed := h.ok("place_order", map[string]any{"preview_id": h.previewID(bracketBuy(10, 150, 160, 145))})
	parent := orderID(t, placed, 0)

	// Unfilled parent: cancelling it cancels the children.
	res := h.ok("cancel_all_orders", map[string]any{"wait_seconds": 1})
	if n := len(asSlice(res["orders"])); n != 1 {
		t.Fatalf("cancel_all with an unfilled parent = %v", res)
	}

	placed = h.ok("place_order", map[string]any{"preview_id": h.previewID(bracketBuy(10, 150, 160, 145))})
	parent = orderID(t, placed, 0)
	if err := h.gw.Fill(testClientID, parent, 4, 150); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "partial fill", func() bool { return h.trade(parent).FilledQty() == 4 })
	res = h.ok("cancel_all_orders", map[string]any{"wait_seconds": 1})
	if n := len(asSlice(res["orders"])); n != 3 {
		t.Fatalf("children of a partially filled parent must be cancelled too: %v", res)
	}
}

type clientZero struct{ *ibgo.IB }

func (clientZero) ClientID() int64 { return 0 }

func TestTradingRefusedForClientIDZero(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true, broker: func(ib *ibgo.IB) Broker { return clientZero{ib} }})
	id := h.previewID(limitBuy(1, 150))
	h.fail("place_order", map[string]any{"preview_id": id}, "client id 0")
	h.fail("cancel_all_orders", nil, "client id 0")
	h.fail("cancel_order", map[string]any{"order_id": 1}, "client id 0")
	h.fail("preview_order", map[string]any{"modify_order_id": 1, "limit_price": 1}, "client id 0")
	h.fail("halt_trading", map[string]any{"reason": "x", "cancel_open_orders": true}, "client id 0")
}

func TestCancelsRespectPaperOnly(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true, account: "U1234567"})
	con := &contract.Contract{ConID: aaplConID, Symbol: "AAPL", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	ord := order.LimitOrder("BUY", 1, 150)
	ord.Account = "U1234567"
	tr, err := h.ib.PlaceOrder(con, ord) // e.g. left by an earlier live-enabled session
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "submitted", func() bool { return tr.Status() == order.StatusSubmitted })

	cancels := h.gw.Received(4)
	h.fail("cancel_order", map[string]any{"order_id": tr.Order.OrderID}, "not a paper account")
	res := h.ok("cancel_all_orders", nil)
	if len(asSlice(res["skipped"])) != 1 || len(asSlice(res["orders"])) != 0 || !strings.Contains(res["note"].(string), "live accounts were not cancelled") {
		t.Fatalf("cancel_all = %v", res)
	}
	halt := h.ok("halt_trading", map[string]any{"reason": "test", "cancel_open_orders": true})
	if len(asSlice(get(t, halt, "cancelled", "skipped"))) != 1 {
		t.Fatalf("halt = %v", halt)
	}
	if h.gw.Received(4) != cancels || tr.IsDone() {
		t.Fatal("a live-account order was cancelled")
	}
}

func TestPlaceWaitsForReady(t *testing.T) {
	var mu sync.Mutex
	ready := errors.New("sync in progress")
	h := newHarness(t, harnessOpts{trading: true, config: func(c *Config) {
		c.Trading.Ready = func() error { mu.Lock(); defer mu.Unlock(); return ready }
	}})
	if st := h.ok("get_status", nil); !strings.Contains(get(t, st, "trading", "not_ready").(string), "sync in progress") {
		t.Fatalf("status = %v", st)
	}
	pv := h.ok("preview_order", limitBuy(1, 150))
	if w := strings.Join(toStrings(pv["warnings"]), "\n"); !strings.Contains(w, "not synchronised yet (sync in progress)") {
		t.Fatalf("warnings = %q", w)
	}
	h.fail("place_order", map[string]any{"preview_id": pv["preview_id"]}, "not synchronised yet")

	mu.Lock()
	ready = nil
	mu.Unlock()
	// The refused attempt did not use the preview up.
	h.ok("place_order", map[string]any{"preview_id": pv["preview_id"]})
}

// gateJournal blocks the write-ahead record of the first submission until
// release is closed.
type gateJournal struct {
	ibgo.OrderJournal
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (g *gateJournal) RecordSubmit(c *contract.Contract, o *order.Order, modify bool) error {
	g.once.Do(func() {
		close(g.entered)
		<-g.release
	})
	return g.OrderJournal.RecordSubmit(c, o, modify)
}

// An order that passed the risk check before halt_trading must be cancelled
// by it, not slip through while the halt collects the open orders.
func TestHaltWaitsForInFlightOrder(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	gj := &gateJournal{OrderJournal: h.ib.Journal(), entered: make(chan struct{}), release: make(chan struct{})}
	h.ib.SetJournal(gj)
	id := h.previewID(limitBuy(1, 150))

	placed := make(chan callResult, 1)
	go func() { placed <- h.raw("place_order", map[string]any{"preview_id": id}) }()
	<-gj.entered
	halted := make(chan callResult, 1)
	go func() { halted <- h.raw("halt_trading", map[string]any{"reason": "test", "cancel_open_orders": true}) }()
	select {
	case r := <-halted:
		t.Fatalf("halt returned while an order was being submitted: %s", r.Content[0].Text)
	case <-time.After(100 * time.Millisecond):
	}
	close(gj.release)
	if r := <-placed; r.IsError {
		t.Fatalf("place = %s", r.Content[0].Text)
	}
	r := <-halted
	if r.IsError || !strings.Contains(r.Content[0].Text, `"status":"Cancelled"`) {
		t.Fatalf("halt must cancel the in-flight order: %s", r.Content[0].Text)
	}
}

func TestModifyKeepsAcknowledgedTIF(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	placed := h.ok("place_order", map[string]any{"preview_id": h.previewID(limitBuy(1, 150))})
	oid := orderID(t, placed, 0)
	h.ok("place_order", map[string]any{"preview_id": h.previewID(map[string]any{"modify_order_id": oid, "tif": "GTC"})})
	waitFor(t, "GTC acknowledged", func() bool { return h.trade(oid).OrderSnapshot().TIF == "GTC" })
	pv := h.ok("preview_order", map[string]any{"modify_order_id": oid, "limit_price": 149})
	if get(t, pv, "orders", 0, "tif") != "GTC" {
		t.Fatalf("a price-only modification must keep GTC: %v", pv)
	}
}

func TestResolveCacheOnlyForConIDOnly(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.ok("search_contracts", map[string]any{"contract": aapl()})
	lookups := h.gw.Received(9)
	h.ok("get_quote", map[string]any{"contract": map[string]any{"con_id": aaplConID}})
	if h.gw.Received(9) != lookups {
		t.Fatal("a con_id-only lookup must use the cache")
	}
	h.ok("get_quote", map[string]any{"contract": map[string]any{"con_id": aaplConID, "exchange": "NASDAQ"}})
	if h.gw.Received(9) != lookups+1 || !contains(h.gw.LastRequest(9), "NASDAQ") {
		t.Fatalf("an explicit exchange must be looked up: %q", h.gw.LastRequest(9))
	}
}

func TestHaltTradingIsDestructive(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	if a := h.srv.toolByName["halt_trading"].Annotations; !a.DestructiveHint || a.ReadOnlyHint {
		t.Fatalf("halt_trading annotations = %+v", a)
	}
}

// get_order reads fills while get_executions makes the reader attach
// commission reports to them; run with -race.
func TestOrderFillsReadWithoutRace(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true, config: func(c *Config) { c.Trading.OrderTypes = []string{"LMT", "MKT"} }})
	placed := h.ok("place_order", map[string]any{"preview_id": h.previewID(map[string]any{"contract": aapl(), "action": "BUY", "quantity": 1, "order_type": "MKT"}), "wait_seconds": 1})
	oid := orderID(t, placed, 0)
	waitFor(t, "fill", func() bool { return h.trade(oid).FilledQty() == 1 })
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); h.raw("get_executions", nil) }()
		go func() { defer wg.Done(); h.raw("get_order", map[string]any{"order_id": oid}) }()
	}
	wg.Wait()
	if fills := asSlice(h.ok("get_order", map[string]any{"order_id": oid})["fills"]); len(fills) != 1 {
		t.Fatalf("fills = %v", fills)
	}
}
