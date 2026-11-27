package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ibgo "github.com/kevinzhao-dev/go-ib-async"
	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
)

func noEnv(string) string { return "" }

func TestParseFlagsDefaults(t *testing.T) {
	c, err := parseFlags(nil, noEnv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.host != "127.0.0.1" || c.port != 4002 || c.clientID != 17 || c.clientIDFallbacks != 9 || c.trading || c.marketDataType != 1 || !c.whatIf {
		t.Fatalf("defaults = %+v", c)
	}
	if c, err := parseFlags([]string{"-what-if=false"}, noEnv, io.Discard); err != nil || c.whatIf {
		t.Fatalf("-what-if=false: %+v, %v", c, err)
	}
	env := map[string]string{"IB_HOST": "gw", "IB_PORT": "7497", "IB_CLIENT_ID": "3"}
	c, _ = parseFlags(nil, func(k string) string { return env[k] }, io.Discard)
	if c.host != "gw" || c.port != 7497 || c.clientID != 3 {
		t.Fatalf("env = %+v", c)
	}
}

func TestParseFlagsTrading(t *testing.T) {
	c, err := parseFlags([]string{"-enable-trading", "-journal", "j.jsonl", "-max-order-qty", "10", "-max-order-notional", "5000",
		"-allowed-symbols", "aapl, msft,", "-max-orders-per-minute", "4"}, noEnv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	l := c.limits
	if l.MaxOrderQuantity != 10 || l.MaxOrderNotional != 5000 || l.MaxOrdersPerWindow != 4 || l.OrderWindow != time.Minute ||
		strings.Join(l.AllowedSymbols, ",") != "AAPL,MSFT" || strings.Join(l.AllowedSecTypes, ",") != "STK" || l.PriceCollarPct != 5 {
		t.Fatalf("limits = %+v", l)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-enable-trading"}, "-journal, -max-order-qty, -max-order-notional"},
		{[]string{"-allow-live"}, "only apply with -enable-trading"},
		{[]string{"-market-data-type", "9"}, "must be 1-4"},
		{[]string{"extra"}, "unexpected arguments"},
		{[]string{"-nope"}, "not defined"},
		{[]string{"-client-id", "0"}, "-client-id 0 is not supported"},
		{[]string{"-client-id-fallbacks", "-1"}, "-client-id-fallbacks must be 0-100"},
		{[]string{"-client-id-fallbacks", "101"}, "-client-id-fallbacks must be 0-100"},
		{[]string{"-client-id", "-3"}, "would try client id 0"},
		{[]string{"-enable-trading", "-journal", "j", "-max-order-qty", "NaN", "-max-order-notional", "1"}, "-max-order-qty must be a finite number"},
		{[]string{"-enable-trading", "-journal", "j", "-max-order-qty", "1", "-max-order-notional", "+Inf"}, "-max-order-notional must be a finite number"},
		{[]string{"-max-position", "-1"}, "-max-position must be a finite number >= 0"},
	} {
		if _, err := parseFlags(tc.args, noEnv, io.Discard); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
	}
}

// TestRunEndToEnd drives the binary's run loop over stdio against the fake
// gateway: connect, preview, place, and the risk limits from the flags.
func TestRunEndToEnd(t *testing.T) {
	gw, err := fakegw.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	gw.Handle(9, func(req []string) [][]byte {
		id := req[2]
		return [][]byte{fakegw.Msg(10, id, "AAPL", "STK", "", 0.0, "", "SMART", "USD", "AAPL", "NMS", "NMS", 265598, 0.01,
			"", "LMT,MKT", "SMART,NASDAQ", 1, 0, "APPLE INC", "NASDAQ", "", "", "", "", "US/Eastern", "", "", "", 0,
			0, 1, "", "", "", "", "COMMON", 1.0, 1.0, 100), fakegw.Msg(52, 1, id)}
	})
	gw.Handle(1, func(req []string) [][]byte {
		id := req[2]
		return [][]byte{fakegw.Msg(1, 6, id, 1, 150.0, 1.0, 0), fakegw.Msg(1, 6, id, 2, 150.2, 1.0, 0), fakegw.Msg(57, 1, id)}
	})

	jpath := filepath.Join(t.TempDir(), "orders.jsonl")
	cfg, err := parseFlags([]string{"-port", fmt.Sprint(gw.Port()), "-client-id", "5", "-market-data-type", "3",
		"-enable-trading", "-journal", jpath, "-max-order-qty", "50", "-max-order-notional", "10000"}, noEnv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), cfg, inR, outW)
		outW.Close()
	}()
	sc := bufio.NewScanner(outR)
	id := 0
	call := func(tool string, args string) map[string]any {
		t.Helper()
		id++
		fmt.Fprintf(inW, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`+"\n", id, tool, args)
		if !sc.Scan() {
			t.Fatal("no response")
		}
		var r struct {
			Result struct {
				Content []struct{ Text string }
				IsError bool
			}
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		text := r.Result.Content[0].Text
		if r.Result.IsError {
			return map[string]any{"error": text}
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(text), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		st := call("get_status", "{}")
		if tr, _ := st["trading"].(map[string]any); st["connected"] == true && st["market_data_type"] == "delayed" && tr["not_ready"] == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if gw.Received(59) != 1 {
		t.Fatal("market data type must be requested after connecting")
	}

	pv := call("preview_order", `{"contract":{"symbol":"AAPL"},"action":"BUY","quantity":60,"limit_price":150}`)
	if !strings.Contains(fmt.Sprint(pv["warnings"]), "exceeds max_order_quantity 50") {
		t.Fatalf("preview = %v", pv)
	}
	if r := call("place_order", fmt.Sprintf(`{"preview_id":%q}`, pv["preview_id"])); !strings.Contains(fmt.Sprint(r["error"]), "order_quantity") {
		t.Fatalf("over-limit place = %v", r)
	}

	pv = call("preview_order", `{"contract":{"symbol":"AAPL"},"action":"BUY","quantity":10,"order_type":"MKT"}`)
	if !strings.Contains(fmt.Sprint(pv["error"]), "MKT is not allowed") {
		t.Fatalf("market orders must be off by default: %v", pv)
	}
	pv = call("preview_order", `{"contract":{"symbol":"AAPL"},"action":"BUY","quantity":10,"limit_price":150}`)
	placed := call("place_order", fmt.Sprintf(`{"preview_id":%q}`, pv["preview_id"]))
	orders, _ := placed["orders"].([]any)
	if len(orders) != 1 || orders[0].(map[string]any)["status"] != "Submitted" || orders[0].(map[string]any)["client_id"] != 5.0 {
		t.Fatalf("placed = %v", placed)
	}
	// Price collar (default 5%) against the server's quotes.
	pv = call("preview_order", `{"contract":{"symbol":"AAPL"},"action":"BUY","quantity":1,"limit_price":170}`)
	if r := call("place_order", fmt.Sprintf(`{"preview_id":%q}`, pv["preview_id"])); !strings.Contains(fmt.Sprint(r["error"]), "price_collar") {
		t.Fatalf("collar = %v", r)
	}

	inW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not stop at EOF")
	}
	b, _ := os.ReadFile(jpath)
	if strings.Count(string(b), `"type":"submit"`) != 1 {
		t.Fatalf("journal = %s", b)
	}
}

func TestSyncGate(t *testing.T) {
	g := &syncGate{}
	gen := g.start()
	if err := g.check(); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("before sync: %v", err)
	}
	if !g.done(gen, errors.New("timeout")) {
		t.Fatal("current generation")
	}
	if err := g.check(); err == nil || !strings.Contains(err.Error(), "sync failed (timeout), retrying") {
		t.Fatalf("after a failed sync: %v", err)
	}
	if !g.done(gen, nil) || g.check() != nil {
		t.Fatal("a successful sync makes placement ready")
	}
	g.reset() // disconnected
	if g.check() == nil || g.done(gen, nil) || g.check() == nil {
		t.Fatal("a sync of an earlier connection must not make the new one ready")
	}
}

func TestSyncLoopRetriesUntilSynced(t *testing.T) {
	gw, err := fakegw.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	ib := ibgo.New()
	g := &syncGate{}
	gen := g.start()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { syncLoop(ctx, ib, g, gen, 20*time.Millisecond); close(done) }()
	time.Sleep(50 * time.Millisecond) // not connected: the first attempts fail
	if err := g.check(); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Fatalf("while disconnected: %v", err)
	}
	cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ccancel()
	if err := ib.Connect(cctx, "127.0.0.1", gw.Port(), 5); err != nil {
		t.Fatal(err)
	}
	defer ib.Disconnect()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sync loop did not finish after connecting")
	}
	if err := g.check(); err != nil {
		t.Fatalf("after sync: %v", err)
	}
}

// startRun runs the server over stdio and returns a tool caller (a tool
// error comes back as {"error": text}) and a stop function.
func startRun(t *testing.T, cfg *config) (call func(tool, args string) map[string]any, stop func()) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), cfg, inR, outW)
		outW.Close()
	}()
	sc := bufio.NewScanner(outR)
	id := 0
	call = func(tool, args string) map[string]any {
		t.Helper()
		id++
		fmt.Fprintf(inW, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`+"\n", id, tool, args)
		if !sc.Scan() {
			t.Fatal("no response")
		}
		var r struct {
			Result struct {
				Content []struct{ Text string }
				IsError bool
			}
		}
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		text := r.Result.Content[0].Text
		if r.Result.IsError {
			return map[string]any{"error": text}
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(text), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	stop = func() {
		inW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("run did not stop at EOF")
		}
	}
	return call, stop
}

// waitStatus polls get_status until cond holds.
func waitStatus(t *testing.T, call func(tool, args string) map[string]any, what string, cond func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := call("get_status", "{}")
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: status %v", what, st)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Another API client (e.g. the ib-mcp of another agent session) holds client
// id 17. Read-only, the server takes the next free id; with trading the id
// is fixed and get_status says why the server cannot connect.
func TestRunClientIDInUse(t *testing.T) {
	gw, err := fakegw.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	gw.SetClientIDsInUse(17)

	cfg, err := parseFlags([]string{"-port", fmt.Sprint(gw.Port())}, noEnv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	call, stop := startRun(t, cfg)
	st := waitStatus(t, call, "read-only server never connected", func(st map[string]any) bool { return st["connected"] == true })
	if st["client_id"] != 18.0 {
		t.Fatalf("status = %v, want client id 18", st)
	}
	stop()
	readOnlyAttempts := len(gw.ClientIDAttempts())

	cfg, err = parseFlags([]string{"-port", fmt.Sprint(gw.Port()), "-enable-trading", "-journal", filepath.Join(t.TempDir(), "j.jsonl"),
		"-max-order-qty", "1", "-max-order-notional", "100"}, noEnv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	call, stop = startRun(t, cfg)
	defer stop()
	st = waitStatus(t, call, "no connection error reported", func(st map[string]any) bool { return st["connection_error"] != nil })
	if st["connected"] != false || !strings.Contains(fmt.Sprint(st["connection_error"]), "error 326 for client id 17") {
		t.Fatalf("status = %v", st)
	}
	if r := call("get_positions", "{}"); !strings.Contains(fmt.Sprint(r["error"]), "error 326") {
		t.Fatalf("get_positions = %v", r)
	}
	for _, id := range gw.ClientIDAttempts()[readOnlyAttempts:] {
		if id != 17 {
			t.Fatalf("attempts = %v: with trading only client id 17 may be tried", gw.ClientIDAttempts()[readOnlyAttempts:])
		}
	}
}
