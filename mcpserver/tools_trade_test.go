package mcpserver

import (
	"context"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	ibgo "github.com/kevinzhao-dev/go-ib-async"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
	"github.com/kevinzhao-dev/go-ib-async/order"
	"github.com/kevinzhao-dev/go-ib-async/risk"
)

func aapl() map[string]any { return map[string]any{"symbol": "AAPL"} }

func limitBuy(qty, px float64) map[string]any {
	return map[string]any{"contract": aapl(), "action": "BUY", "quantity": qty, "order_type": "LMT", "limit_price": px}
}

// previewID previews args and returns the preview id.
func (h *harness) previewID(args map[string]any) string {
	h.t.Helper()
	pv := h.ok("preview_order", args)
	id, _ := pv["preview_id"].(string)
	if id == "" {
		h.t.Fatalf("no preview id: %v", pv)
	}
	return id
}

func (h *harness) trade(orderID int64) *order.Trade {
	h.t.Helper()
	for _, t := range h.ib.Trades() {
		if t.Order.OrderID == orderID {
			return t
		}
	}
	h.t.Fatalf("no trade %d", orderID)
	return nil
}

func orderID(t *testing.T, res map[string]any, i int) int64 {
	t.Helper()
	return int64(get(t, res, "orders", i, "order_id").(float64))
}

func TestNewValidatesConfig(t *testing.T) {
	ib := ibgo.New()
	if _, err := New(nil, Config{}); err == nil {
		t.Fatal("nil broker")
	}
	if _, err := New(ib, Config{Trading: TradingConfig{Enabled: true}}); err == nil || !strings.Contains(err.Error(), "risk manager") {
		t.Fatalf("trading without risk: %v", err)
	}
	weak := risk.New(risk.Limits{MaxOrderQuantity: 10}, ib)
	if _, err := New(ib, Config{Trading: TradingConfig{Enabled: true, Risk: weak}}); err == nil || !strings.Contains(err.Error(), "MaxOrderNotional") {
		t.Fatalf("trading without notional limit: %v", err)
	}
	if _, err := New(ib, Config{Trading: TradingConfig{OrderTypes: []string{"TRAIL"}}}); err == nil {
		t.Fatal("unsupported order type")
	}
	s, err := New(ib, Config{Trading: TradingConfig{OrderTypes: []string{" mkt "}}})
	if err != nil || s.cfg.Trading.OrderTypes[0] != "MKT" || s.cfg.Name != "ib-mcp" || s.cfg.RequestTimeout != 30*time.Second {
		t.Fatalf("defaults: %+v, %v", s.cfg, err)
	}
}

func TestPreviewThenPlaceLimitOrder(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	pv := h.ok("preview_order", map[string]any{"contract": aapl(), "action": "buy", "quantity": 10, "limit_price": 150, "tif": "gtc"})
	if pv["kind"] != "new" || pv["account"] != "DU123456" || pv["paper_account"] != true || pv["estimated_notional"] != 1500.0 ||
		get(t, pv, "orders", 0, "order_type") != "LMT" || get(t, pv, "orders", 0, "tif") != "GTC" ||
		get(t, pv, "quote", "bid") != 150.0 || get(t, pv, "what_if", "init_margin_change") != 1500.25 ||
		get(t, pv, "what_if", "commission") != 1.25 || pv["expires_at"] == nil {
		t.Fatalf("preview = %v", pv)
	}
	if s := pv["summary"].(string); !strings.Contains(s, "BUY 10 AAPL (STK SMART USD, con_id 265598) LMT 150, GTC, account DU123456") {
		t.Fatalf("summary = %q", s)
	}
	if pv["warnings"] != nil {
		t.Fatalf("unexpected warnings: %v", pv["warnings"])
	}
	placedBefore := h.gw.Received(3) // the what-if was a placeOrder message too

	res := h.ok("place_order", map[string]any{"preview_id": pv["preview_id"]})
	id := orderID(t, res, 0)
	if get(t, res, "orders", 0, "status") != "Submitted" || get(t, res, "orders", 0, "own") != true || res["note"] != nil {
		t.Fatalf("placed = %v", res)
	}
	if h.gw.Received(3) != placedBefore+1 {
		t.Fatal("exactly one order must be sent")
	}
	bo, ok := h.gw.Order(testClientID, id)
	if !ok || bo.Qty != 10 || bo.LmtPrice != 150 || bo.TIF != "GTC" || bo.Account != "DU123456" || bo.OrderRef != "ib-mcp" {
		t.Fatalf("broker order = %+v", bo)
	}
	journal, _ := os.ReadFile(h.jpath)
	if !strings.Contains(string(journal), `"type":"submit"`) {
		t.Fatalf("journal = %s", journal)
	}

	// Single use.
	h.fail("place_order", map[string]any{"preview_id": pv["preview_id"]}, "unknown or already used")
	h.fail("place_order", map[string]any{"preview_id": "pv_nope"}, "unknown or already used")
	h.fail("place_order", nil, `missing required argument "preview_id"`)
}

func TestPreviewExpires(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true, config: func(c *Config) { c.Trading.PreviewTTL = time.Millisecond }})
	id := h.previewID(limitBuy(1, 150))
	time.Sleep(5 * time.Millisecond)
	h.fail("place_order", map[string]any{"preview_id": id}, "expired")
}

func TestPreviewStoreEvictsOldest(t *testing.T) {
	ps := newPreviewStore()
	now := time.Now()
	for i := 0; i < maxPreviews+5; i++ {
		ps.put(&plan{id: newPreviewID(), created: now.Add(time.Duration(i)), expires: now.Add(time.Hour)})
	}
	ps.put(&plan{id: "expired", created: now, expires: now.Add(-time.Second)})
	ps.put(&plan{id: "fresh", created: now.Add(time.Hour), expires: now.Add(time.Hour)})
	if len(ps.plans) > maxPreviews {
		t.Fatalf("%d previews stored, max %d", len(ps.plans), maxPreviews)
	}
	if _, ok := ps.plans["expired"]; ok {
		t.Fatal("expired previews must be dropped")
	}
	if _, err := ps.take("fresh"); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewValidation(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	base := func(kv ...any) map[string]any {
		a := map[string]any{"contract": aapl(), "action": "BUY", "quantity": 1}
		for i := 0; i < len(kv); i += 2 {
			a[kv[i].(string)] = kv[i+1]
		}
		return a
	}
	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"contract": aapl(), "quantity": 1, "limit_price": 1}, `missing required argument "action"`},
		{base("quantity", 0, "limit_price", 1), "quantity must be a positive number"},
		{base("quantity", -2, "limit_price", 1), "quantity must be a positive number"},
		{base(), "a LMT order needs limit_price"},
		{base("limit_price", -1), "limit_price must be a positive number"},
		{base("limit_price", 1, "stop_price", 1), "stop_price is only used by STP"},
		{base("order_type", "MKT"), "order type MKT is not allowed on this server"},
		{base("order_type", "STP"), "a STP order needs stop_price"},
		{base("order_type", "STP", "stop_price", 1, "limit_price", 1), "use STP LMT"},
		{base("order_type", "STP LMT", "stop_price", 1), "needs limit_price and stop_price"},
		{base("order_type", "TRAIL"), "must be one of"},
		{base("limit_price", 150, "take_profit_price", 160), "needs both take_profit_price and stop_loss_price"},
		{base("order_type", "STP", "stop_price", 150, "take_profit_price", 160, "stop_loss_price", 140), "bracket entry must be a LMT order"},
		{base("limit_price", 150, "take_profit_price", 140, "stop_loss_price", 160), "BUY bracket needs takeProfit > limit > stopLoss"},
		{base("limit_price", 150, "account", "DU999"), `account "DU999" is not managed`},
		{map[string]any{"action": "BUY", "quantity": 1, "limit_price": 1}, `missing required argument "contract"`},
		{base("limit_price", 1, "contract", map[string]any{"symbol": "AMBIG"}), "2 contracts match"},
	}
	for _, c := range cases {
		h.fail("preview_order", c.args, c.want)
	}
	if n := len(h.srv.previews.plans); n != 0 {
		t.Fatalf("%d previews stored by failed validations", n)
	}
}

func TestRiskRejectionIsFinal(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true, limits: risk.Limits{MaxOrderQuantity: 5, MaxOrderNotional: 1000, AllowedSecTypes: []string{"OPT"}, BlockedSymbols: []string{"aapl"}}})
	pv := h.ok("preview_order", limitBuy(10, 150))
	warnings := strings.Join(toStrings(pv["warnings"]), "\n")
	for _, want := range []string{"exceeds max_order_quantity 5", "exceeds max_order_notional 1000.00", "sec_type STK is not in allowed_sec_types", "symbol AAPL is not allowed"} {
		if !strings.Contains(warnings, want) {
			t.Fatalf("warnings %q missing %q", warnings, want)
		}
	}
	sent := h.gw.Received(3)
	msg := h.fail("place_order", map[string]any{"preview_id": pv["preview_id"]}, `rejected by risk check "sec_type"`)
	if !strings.Contains(msg, "do not work around it") || h.gw.Received(3) != sent {
		t.Fatalf("rejected order must not be sent: %s", msg)
	}
}

func TestPriceCollarUsesServerQuotes(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true, limits: risk.Limits{MaxOrderQuantity: 100, MaxOrderNotional: 100000, PriceCollarPct: 5}})
	id := h.previewID(limitBuy(1, 200)) // quote midpoint 150.05
	h.fail("place_order", map[string]any{"preview_id": id}, `"price_collar"`)
	id = h.previewID(limitBuy(1, 151))
	h.ok("place_order", map[string]any{"preview_id": id})
}

func TestMarketOrderNotionalFromQuote(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true, config: func(c *Config) { c.Trading.OrderTypes = []string{"LMT", "MKT"} }})
	pv := h.ok("preview_order", map[string]any{"contract": aapl(), "action": "SELL", "quantity": 10, "order_type": "MKT"})
	if pv["estimated_notional"] != 1500.5 || !strings.Contains(pv["summary"].(string), "SELL 10 AAPL") || get(t, pv, "orders", 0, "limit_price") != nil {
		t.Fatalf("preview = %v", pv)
	}
	res := h.ok("place_order", map[string]any{"preview_id": pv["preview_id"], "wait_seconds": 1})
	if get(t, res, "orders", 0, "order_type") != "MKT" {
		t.Fatalf("placed = %v", res)
	}

	// Without a quote the notional is unknown, and the risk manager fails closed.
	pv = h.ok("preview_order", map[string]any{"contract": map[string]any{"symbol": "NODATA"}, "action": "BUY", "quantity": 1, "order_type": "MKT"})
	if pv["estimated_notional"] != nil || !strings.Contains(strings.Join(toStrings(pv["warnings"]), " "), "notional cannot be estimated") {
		t.Fatalf("preview without quote = %v", pv)
	}
	h.fail("place_order", map[string]any{"preview_id": pv["preview_id"]}, `"order_notional"`)
}

func TestBracketOrder(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	pv := h.ok("preview_order", map[string]any{"contract": aapl(), "action": "BUY", "quantity": 10, "limit_price": 150,
		"take_profit_price": 160, "stop_loss_price": 145, "tif": "GTC"})
	if pv["kind"] != "bracket" || len(pv["orders"].([]any)) != 3 || get(t, pv, "orders", 1, "role") != "take_profit" ||
		get(t, pv, "orders", 2, "stop_price") != 145.0 || get(t, pv, "orders", 2, "action") != "SELL" || get(t, pv, "what_if") == nil {
		t.Fatalf("preview = %v", pv)
	}
	res := h.ok("place_order", map[string]any{"preview_id": pv["preview_id"]})
	if len(res["orders"].([]any)) != 3 {
		t.Fatalf("placed = %v", res)
	}
	parent := orderID(t, res, 0)
	if get(t, res, "orders", 1, "parent_id") != float64(parent) || get(t, res, "orders", 2, "tif") != "GTC" {
		t.Fatalf("legs = %v", res)
	}

	// cancel_all_orders cancels the parent, which takes the children along.
	c := h.ok("cancel_all_orders", nil)
	if len(c["orders"].([]any)) != 1 || get(t, c, "orders", 0, "order_id") != float64(parent) {
		t.Fatalf("cancel all = %v", c)
	}
	for _, tr := range h.ib.Trades() {
		waitFor(t, "bracket cancelled", tr.IsDone)
	}
	if c := h.ok("cancel_all_orders", nil); c["note"] != "no open orders of this client" {
		t.Fatalf("nothing to cancel = %v", c)
	}
}

func TestBracketNeedsStopOrders(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true, config: func(c *Config) { c.Trading.OrderTypes = []string{"LMT"} }})
	h.fail("preview_order", map[string]any{"contract": aapl(), "action": "BUY", "quantity": 1, "limit_price": 150,
		"take_profit_price": 160, "stop_loss_price": 145}, "bracket stop-loss leg")
}

func TestModifyOrder(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	res := h.ok("place_order", map[string]any{"preview_id": h.previewID(limitBuy(10, 150))})
	id := orderID(t, res, 0)

	pv := h.ok("preview_order", map[string]any{"modify_order_id": id, "limit_price": 151, "quantity": 12})
	if pv["kind"] != "modify" || pv["modify_order_id"] != float64(id) || get(t, pv, "orders", 0, "limit_price") != 151.0 ||
		!strings.Contains(pv["summary"].(string), "(was 10 LMT 150)") || pv["what_if"] != nil {
		t.Fatalf("preview = %v", pv)
	}
	res = h.ok("place_order", map[string]any{"preview_id": pv["preview_id"]})
	if res["note"] != nil || get(t, res, "orders", 0, "limit_price") != 151.0 || get(t, res, "orders", 0, "quantity") != 12.0 {
		t.Fatalf("modified = %v", res)
	}
	if bo, _ := h.gw.Order(testClientID, id); bo.LmtPrice != 151 || bo.Qty != 12 {
		t.Fatalf("broker order = %+v", bo)
	}
	// Acknowledged: an entry follows the "Modify" one ("Modified", or a status
	// change when the quantity changed).
	if l := h.trade(id).LogSnapshot(); l[len(l)-1].Message == "Modify" || l[len(l)-2].Message != "Modify" {
		t.Fatalf("log = %+v", l)
	}

	if err := h.gw.Fill(testClientID, id, 4, 151); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "fill", func() bool { return h.trade(id).FilledQty() == 4 })
	cases := []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"modify_order_id": id, "quantity": 3}, "below the 4 already filled"},
		{map[string]any{"modify_order_id": id, "action": "SELL", "limit_price": 1}, "cannot change the side"},
		{map[string]any{"modify_order_id": id, "order_type": "STP", "stop_price": 1}, "cannot change the type"},
		{map[string]any{"modify_order_id": id, "stop_price": 1}, "has no stop price"},
		{map[string]any{"modify_order_id": id, "limit_price": 151}, "nothing to change"},
		{map[string]any{"modify_order_id": id, "take_profit_price": 1}, "cannot be used with modify_order_id"},
		{map[string]any{"modify_order_id": id, "contract": map[string]any{"con_id": msftConID}, "limit_price": 152}, "is for con_id 265598"},
		{map[string]any{"modify_order_id": id, "limit_price": -1}, "limit_price must be a positive number"},
		{map[string]any{"modify_order_id": 999, "limit_price": 1}, "no order 999"},
	}
	for _, c := range cases {
		h.fail("preview_order", c.args, c.want)
	}
	// tif alone is a change.
	h.ok("preview_order", map[string]any{"modify_order_id": id, "tif": "GTC"})

	c := h.ok("cancel_order", map[string]any{"order_id": id})
	if get(t, c, "orders", 0, "status") != "Cancelled" {
		t.Fatalf("cancel = %v", c)
	}
	h.fail("preview_order", map[string]any{"modify_order_id": id, "limit_price": 152}, "already Cancelled")
	h.fail("cancel_order", map[string]any{"order_id": id}, "already Cancelled")
	h.fail("cancel_order", map[string]any{"order_id": 999}, "no order 999")
}

func TestModifyStopAndOrderTypeAllowList(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	res := h.ok("place_order", map[string]any{"preview_id": h.previewID(map[string]any{"contract": aapl(), "action": "SELL", "quantity": 1, "order_type": "STP", "stop_price": 140})})
	id := orderID(t, res, 0)
	h.fail("preview_order", map[string]any{"modify_order_id": id, "limit_price": 141}, "has no limit price")
	pv := h.ok("preview_order", map[string]any{"modify_order_id": id, "stop_price": 141})
	if get(t, pv, "orders", 0, "stop_price") != 141.0 {
		t.Fatalf("preview = %v", pv)
	}
	h.srv.cfg.Trading.OrderTypes = []string{"LMT"}
	h.fail("preview_order", map[string]any{"modify_order_id": id, "stop_price": 142}, "order type STP is not allowed")
}

func TestBrokerRejectionIsReported(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	h.gw.SetReject(func(o *fakegw.Order) string { return "insufficient funds" })
	res := h.ok("place_order", map[string]any{"preview_id": h.previewID(limitBuy(1, 150))})
	if get(t, res, "orders", 0, "status") != "Cancelled" || !strings.Contains(res["note"].(string), "insufficient funds") {
		t.Fatalf("rejected = %v", res)
	}
}

func TestHaltTrading(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	res := h.ok("place_order", map[string]any{"preview_id": h.previewID(limitBuy(1, 150))})
	id := orderID(t, res, 0)
	pending := h.previewID(limitBuy(2, 150))

	hr := h.ok("halt_trading", map[string]any{"reason": "unexpected fills", "cancel_open_orders": true})
	if hr["halted"] != true || get(t, hr, "cancelled", "orders", 0, "order_id") != float64(id) {
		t.Fatalf("halt = %v", hr)
	}
	if !h.trade(id).IsDone() {
		t.Fatal("open order must be cancelled")
	}
	st := h.ok("get_status", nil)
	if get(t, st, "trading", "halted") != true || !strings.Contains(get(t, st, "trading", "halt_reason").(string), "unexpected fills") {
		t.Fatalf("status = %v", st)
	}
	h.fail("place_order", map[string]any{"preview_id": pending}, `"halted"`)
	pv := h.ok("preview_order", limitBuy(1, 150))
	if !strings.Contains(strings.Join(toStrings(pv["warnings"]), " "), "trading is halted") {
		t.Fatalf("preview while halted = %v", pv)
	}
	h.fail("halt_trading", nil, `missing required argument "reason"`)
	if h.srv.toolByName["resume_trading"] != nil {
		t.Fatal("agents must not be able to resume trading")
	}
}

func TestHaltWhileDisconnected(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	h.ib.Disconnect()
	h.fail("halt_trading", map[string]any{"reason": "x", "cancel_open_orders": true}, "could not be cancelled")
	if halted, _ := h.rm.Halted(); !halted {
		t.Fatal("the halt itself must succeed")
	}
	h.ok("halt_trading", map[string]any{"reason": "again"})
	h.fail("cancel_order", map[string]any{"order_id": 1}, "not connected")
	h.fail("cancel_all_orders", nil, "not connected")
}

func TestPlaceAfterDisconnect(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	id := h.previewID(limitBuy(1, 150))
	h.ib.Disconnect()
	h.fail("place_order", map[string]any{"preview_id": id}, "not connected")
}

func TestJournalFailureBlocksOrder(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	id := h.previewID(limitBuy(1, 150))
	h.ib.SetJournal(failingJournal{})
	h.fail("place_order", map[string]any{"preview_id": id}, "order failed: ibgo: journal write failed")
}

func TestLiveAccountsAreBlocked(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true, account: "U1234567"})
	pv := h.ok("preview_order", limitBuy(1, 150))
	if pv["preview_id"] != nil || pv["paper_account"] != false || !strings.Contains(pv["next_step"].(string), "not a paper account") {
		t.Fatalf("live preview = %v", pv)
	}
	st := h.ok("get_status", nil)
	if !strings.Contains(get(t, st, "trading", "blocked").(string), "U1234567") {
		t.Fatalf("status = %v", st)
	}
	// A plan for a live account can never be placed, even if one slipped in.
	h.srv.previews.put(&plan{id: "pv_live", kind: kindNew, account: "U1234567", expires: time.Now().Add(time.Minute)})
	h.fail("place_order", map[string]any{"preview_id": "pv_live"}, "not a paper account")
	if !strings.Contains(h.srv.instructions(), "Only paper accounts") {
		t.Fatal("instructions must state the paper-only rule")
	}

	live := newHarness(t, harnessOpts{trading: true, account: "U1234567", config: func(c *Config) { c.Trading.AllowLive = true }})
	if pv := live.ok("preview_order", limitBuy(1, 150)); pv["preview_id"] == nil {
		t.Fatalf("allow-live preview = %v", pv)
	}
	if !strings.Contains(live.srv.instructions(), "WARNING") {
		t.Fatal("instructions must warn about live trading")
	}
}

func TestPreviewWithTradingDisabled(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	pv := h.ok("preview_order", limitBuy(1, 150))
	if pv["preview_id"] != nil || !strings.Contains(pv["next_step"].(string), "informational") || pv["what_if"] == nil {
		t.Fatalf("preview = %v", pv)
	}
	// preview_order is annotated read-only, so clients run it unasked: the
	// one placeOrder message it sends must be a what-if that leaves no order
	// at the broker (the fake, like TWS, decodes the whatIf flag).
	if n := h.gw.Received(3); n != 1 {
		t.Fatalf("%d placeOrder messages, want the one what-if", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if open, err := h.ib.ReqAllOpenOrders(ctx); err != nil || len(open) != 0 {
		t.Fatalf("broker orders after a read-only preview: %v, %v", open, err)
	}
	if st := h.ok("get_status", nil); st["what_if"] != true {
		t.Fatalf("status = %v", st)
	}
}

// With DisableWhatIf a read-only server sends no order message at all.
func TestPreviewWithWhatIfDisabled(t *testing.T) {
	h := newHarness(t, harnessOpts{config: func(c *Config) { c.DisableWhatIf = true }})
	pv := h.ok("preview_order", limitBuy(1, 150))
	if pv["what_if"] != nil || pv["quote"] == nil || !strings.Contains(strings.Join(toStrings(pv["warnings"]), "\n"), "what-if is disabled") {
		t.Fatalf("preview = %v", pv)
	}
	if n := h.gw.Received(3); n != 0 {
		t.Fatalf("%d placeOrder messages sent with what-if disabled", n)
	}
	if st := h.ok("get_status", nil); st["what_if"] != false {
		t.Fatalf("status = %v", st)
	}
	if ins := h.srv.instructions(); strings.Contains(ins, "what-if calculator") {
		t.Fatalf("instructions = %q", ins)
	}
}

func TestPreviewWarnsOnDelayedDataAndWhatIfFailure(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true})
	h.gw.Handle(1, func(req []string) [][]byte {
		id := req[2]
		return [][]byte{fakegw.Msg(58, 1, id, 3), fakegw.Msg(1, 6, id, 68, 150.0, 1.0, 0), fakegw.Msg(57, 1, id)}
	})
	h.gw.Handle(3, func(req []string) [][]byte { // the what-if placeOrder
		return [][]byte{fakegw.Msg(4, 2, req[1], 110, "The price does not conform to the minimum price variation", "")}
	})
	pv := h.ok("preview_order", limitBuy(1, 150.001))
	w := strings.Join(toStrings(pv["warnings"]), "\n")
	if !strings.Contains(w, "quote is delayed market data") || !strings.Contains(w, "what-if failed") {
		t.Fatalf("warnings = %q", w)
	}

	h.gw.Handle(1, func(req []string) [][]byte {
		return [][]byte{fakegw.Msg(4, 2, req[2], 354, "not subscribed", "")}
	})
	pv = h.ok("preview_order", limitBuy(1, 150))
	if w := strings.Join(toStrings(pv["warnings"]), "\n"); !strings.Contains(w, "no quote") {
		t.Fatalf("warnings = %q", w)
	}
}

type multiAccount struct{ *ibgo.IB }

func (multiAccount) ManagedAccounts() []string { return []string{"DU1", "DU2"} }

type noAccount struct{ *ibgo.IB }

func (noAccount) ManagedAccounts() []string { return nil }

func TestAccountSelection(t *testing.T) {
	h := newHarness(t, harnessOpts{trading: true, broker: func(ib *ibgo.IB) Broker { return multiAccount{ib} }})
	h.fail("preview_order", limitBuy(1, 150), "several accounts are managed (DU1, DU2); give account")
	args := limitBuy(1, 150)
	args["account"] = "DU2"
	if pv := h.ok("preview_order", args); pv["account"] != "DU2" {
		t.Fatalf("preview = %v", pv)
	}
	n := newHarness(t, harnessOpts{trading: true, broker: func(ib *ibgo.IB) Broker { return noAccount{ib} }})
	n.fail("preview_order", limitBuy(1, 150), "no managed accounts")
}

func TestWaitHelpers(t *testing.T) {
	if waitDuration(0, 3, 15) != 3*time.Second || waitDuration(99, 3, 15) != 15*time.Second || waitDuration(-1, 3, 15) != 0 {
		t.Fatal("waitDuration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if poll(ctx, time.Second, func() bool { return false }) {
		t.Fatal("poll must stop on a cancelled context")
	}
	if poll(context.Background(), 0, func() bool { return false }) {
		t.Fatal("poll must stop at the deadline")
	}
}

func TestFormatting(t *testing.T) {
	if describePrices(order.Order{OrderType: "STP LMT", AuxPrice: 1, LmtPrice: 2}) != "STP 1 LMT 2" || describePrices(order.Order{OrderType: "MKT"}) != "MKT" {
		t.Fatal("describePrices")
	}
	if positiveFinite("x", 0, true) == nil {
		t.Fatal("required zero")
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range asSlice(v) {
		out = append(out, x.(string))
	}
	return out
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// failingJournal refuses every write-ahead record.
type failingJournal struct{}

func (failingJournal) RecordSubmit(*contract.Contract, *order.Order, bool) error {
	return os.ErrPermission
}
func (failingJournal) RecordCancelRequest(*order.Trade) error           { return nil }
func (failingJournal) RecordStatus(*order.Trade) error                  { return nil }
func (failingJournal) RecordFill(*order.Trade, *order.Fill) error       { return nil }
func (failingJournal) RecordCommission(*order.Trade, *order.Fill) error { return nil }
func (failingJournal) RecordError(*order.Trade, int, string) error      { return nil }

// A closed-market snapshot ends after TWS's own 11s timeout and took up to
// 12.05s against a live Gateway, so the quote budget needs headroom above
// that. place_order spends it before submitting (journal fsync, risk check,
// send) and waiting up to ackWaitMax, all within the default request
// timeout.
func TestQuoteTimeoutBudget(t *testing.T) {
	if quoteTimeout < 13*time.Second {
		t.Fatalf("quoteTimeout %v: closed-market snapshots take up to ~12s", quoteTimeout)
	}
	const submitAllowance = time.Second
	h := newHarness(t, harnessOpts{offline: true})
	maxWait := waitDuration(math.MaxInt32, ackWaitDefault, ackWaitMax)
	if quoteTimeout+submitAllowance+maxWait > h.srv.cfg.RequestTimeout {
		t.Fatalf("quote %v + submission %v + ack wait %v exceed the request timeout %v",
			quoteTimeout, submitAllowance, maxWait, h.srv.cfg.RequestTimeout)
	}
	// The tool descriptions state the same default and cap.
	want := fmt.Sprintf("(default %d, max %d)", ackWaitDefault, ackWaitMax)
	for _, v := range []any{placeArgs{}, cancelArgs{}, cancelAllArgs{}} {
		f, ok := reflect.TypeOf(v).FieldByName("WaitSeconds")
		if !ok || !strings.Contains(f.Tag.Get("desc"), want) {
			t.Fatalf("%T.WaitSeconds desc %q does not say %q", v, f.Tag.Get("desc"), want)
		}
	}
}
