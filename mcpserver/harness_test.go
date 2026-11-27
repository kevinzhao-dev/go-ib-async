package mcpserver

// Test harness: the real IB client against internal/fakegw (orders, fills,
// what-if, positions, executions built in; contract details, market data,
// bars, option chains and account summary scripted below), with the MCP
// tools called through the same path as tools/call.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ibgo "github.com/kevinzhao-dev/go-ib-async"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
	"github.com/kevinzhao-dev/go-ib-async/journal"
	"github.com/kevinzhao-dev/go-ib-async/risk"
)

const (
	testClientID = 7
	aaplConID    = 265598
	msftConID    = 272093
	noDataConID  = 4242
)

type harness struct {
	t     *testing.T
	gw    *fakegw.Server
	ib    *ibgo.IB
	srv   *Server
	rm    *risk.Manager
	jpath string
}

type harnessOpts struct {
	trading bool
	account string
	limits  risk.Limits
	config  func(*Config)
	broker  func(*ibgo.IB) Broker
	offline bool
}

func defaultLimits() risk.Limits {
	return risk.Limits{MaxOrderQuantity: 100, MaxOrderNotional: 50000, MaxOrdersPerWindow: 100}
}

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	gw, err := fakegw.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	if o.account != "" {
		gw.Account = o.account
	}
	scriptGateway(gw)

	ib := ibgo.New()
	if !o.offline {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := ib.Connect(ctx, "127.0.0.1", gw.Port(), testClientID); err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(ib.Disconnect)
	}
	h := &harness{t: t, gw: gw, ib: ib}

	cfg := Config{}
	if o.trading {
		h.jpath = filepath.Join(t.TempDir(), "orders.jsonl")
		j, err := journal.Open(h.jpath, journal.Options{NoSync: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { j.Close() })
		ib.SetJournal(j)
		limits := o.limits
		if limits.MaxOrderQuantity == 0 {
			limits = defaultLimits()
		}
		h.rm = risk.New(limits, ib, risk.WithPriceSource(func(c *contract.Contract) (float64, bool) {
			return h.srv.ReferencePrice(c)
		}))
		ib.SetRiskManager(h.rm)
		cfg.Trading = TradingConfig{Enabled: true, Risk: h.rm}
	}
	if o.config != nil {
		o.config(&cfg)
	}
	var b Broker = ib
	if o.broker != nil {
		b = o.broker(ib)
	}
	h.srv, err = New(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// scriptGateway adds responses for requests the fake does not simulate.
func scriptGateway(gw *fakegw.Server) {
	gw.Handle(9, func(req []string) [][]byte { // reqContractDetails: 9, 8, reqId, conId, symbol, secType, ...
		id, conID, sym := req[2], req[3], req[4]
		switch {
		case sym == "NOPE":
			return [][]byte{fakegw.Msg(4, 2, id, 200, "No security definition has been found for the request", "")}
		case sym == "AMBIG":
			return [][]byte{
				detailsMsg(id, "AMBIG", "STK", 1001, "NYSE"),
				detailsMsg(id, "AMBIG", "STK", 1002, "ARCA"),
				fakegw.Msg(52, 1, id),
			}
		case sym == "MSFT" || conID == "272093":
			return [][]byte{detailsMsg(id, "MSFT", "STK", msftConID, "NASDAQ"), fakegw.Msg(52, 1, id)}
		case sym == "NODATA" || conID == "4242":
			return [][]byte{detailsMsg(id, "NODATA", "STK", noDataConID, "NYSE"), fakegw.Msg(52, 1, id)}
		case sym == "EUR":
			return [][]byte{detailsMsg(id, "EUR", "CASH", 12087792, "IDEALPRO"), fakegw.Msg(52, 1, id)}
		case sym == "ES":
			return [][]byte{detailsMsg(id, "ES", "FUT", 495512563, "CME"), fakegw.Msg(52, 1, id)}
		case sym == "NOID":
			return [][]byte{detailsMsg(id, "NOID", "STK", 0, "NYSE"), fakegw.Msg(52, 1, id)}
		case sym == "EMPTY":
			return [][]byte{fakegw.Msg(52, 1, id)}
		}
		return [][]byte{detailsMsg(id, "AAPL", "STK", aaplConID, "NASDAQ"), fakegw.Msg(52, 1, id)}
	})
	gw.Handle(1, func(req []string) [][]byte { // reqMktData: 1, 11, reqId, conId, ...
		id, conID := req[2], req[3]
		if conID == "4242" {
			return [][]byte{fakegw.Msg(57, 1, id)}
		}
		px := 150.0
		if conID == "272093" {
			px = 400.0
		}
		return [][]byte{
			fakegw.Msg(1, 6, id, 1, px, 100.0, 0),
			fakegw.Msg(1, 6, id, 2, px+0.1, 200.0, 0),
			fakegw.Msg(1, 6, id, 4, px+0.05, 5.0, 0),
			fakegw.Msg(1, 6, id, 9, px-1, 0.0, 0), // close
			fakegw.Msg(57, 1, id),
		}
	})
	gw.Handle(20, func(req []string) [][]byte { // reqHistoricalData: 20, reqId, ...
		return [][]byte{fakegw.Msg(17, req[1], "20231019 09:30:00", "20231019 16:00:00", 3,
			"1697722200", 174.5, 175.0, 174.0, 174.8, 1200.5, 174.6, 300,
			"1697725800", 174.8, 176.0, 174.7, 175.9, 900.0, 175.4, 250,
			"1697729400", 175.9, 176.5, 175.5, 176.1, -1.0, -1.0, 10)}
	})
	gw.Handle(78, func(req []string) [][]byte { // reqSecDefOptParams: 78, reqId, ...
		id := req[1]
		return [][]byte{
			fakegw.Msg(75, id, "SMART", aaplConID, "AAPL", "100", 3, "20231117", "20231020", "20231027", 4, 180.0, 170.0, 175.0, 185.0),
			fakegw.Msg(75, id, "CBOE", aaplConID, "AAPL", "100", 1, "20231020", 1, 175.0),
			fakegw.Msg(76, id),
		}
	})
	gw.Handle(62, func(req []string) [][]byte { // reqAccountSummary: 62, 1, reqId, ...
		id := req[2]
		return [][]byte{
			fakegw.Msg(63, 1, id, "DU123456", "NetLiquidation", "100000.50", "USD"),
			fakegw.Msg(63, 1, id, "DU123456", "BuyingPower", "400000", "USD"),
			fakegw.Msg(63, 1, id, "DU999", "NetLiquidation", "5", "USD"),
			fakegw.Msg(64, 1, id),
		}
	})
}

func detailsMsg(reqID, symbol, secType string, conID int64, primary string) []byte {
	return detailsMsgExpiry(reqID, symbol, secType, conID, primary, "")
}

func detailsMsgExpiry(reqID, symbol, secType string, conID int64, primary, expiry string) []byte {
	exch := "SMART"
	if secType == "FUT" {
		exch = "CME"
	}
	return fakegw.Msg(10, reqID, symbol, secType, expiry, 0.0, "", exch, "USD", symbol, "NMS", "NMS", conID, 0.01,
		"", "ACTIVETIM,LMT,MKT", "SMART,"+primary, 1, 0, symbol+" INC", primary, "", "Technology", "Computers", "Computers",
		"US/Eastern", "20231019:0930-20231019:1600", "20231019:0930-20231019:1600", "", 0,
		0, 1, "", "", "26,26", "", "COMMON", 1.0, 1.0, 100)
}

func (h *harness) raw(name string, args any) callResult {
	h.t.Helper()
	tl := h.srv.toolByName[name]
	if tl == nil {
		h.t.Fatalf("tool %s not registered", name)
	}
	var b []byte
	switch a := args.(type) {
	case nil:
	case string:
		b = []byte(a)
	default:
		var err error
		if b, err = json.Marshal(a); err != nil {
			h.t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return h.srv.callTool(ctx, tl, b)
}

// ok calls a tool that must succeed and returns its structured result.
func (h *harness) ok(name string, args any) map[string]any {
	h.t.Helper()
	res := h.raw(name, args)
	if res.IsError {
		h.t.Fatalf("%s: unexpected error: %s", name, res.Content[0].Text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &out); err != nil {
		h.t.Fatalf("%s: result is not a JSON object: %v: %s", name, err, res.Content[0].Text)
	}
	return out
}

// fail calls a tool that must fail with a message containing want.
func (h *harness) fail(name string, args any, want string) string {
	h.t.Helper()
	res := h.raw(name, args)
	if !res.IsError {
		h.t.Fatalf("%s: want error containing %q, got %s", name, want, res.Content[0].Text)
	}
	if msg := res.Content[0].Text; !strings.Contains(msg, want) {
		h.t.Fatalf("%s: error %q does not contain %q", name, msg, want)
	}
	return res.Content[0].Text
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// get follows a path of object keys and array indexes in a decoded result.
func get(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("path %v: %v is not an object", path, v)
			}
			v = m[k]
		case int:
			a, ok := v.([]any)
			if !ok || k >= len(a) {
				t.Fatalf("path %v: %v has no index %d", path, v, k)
			}
			v = a[k]
		}
	}
	return v
}
