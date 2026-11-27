package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kevinzhao-dev/go-ib-async/internal/fakegw"
)

func noEnv(string) string { return "" }

func TestParseFlagsDefaults(t *testing.T) {
	c, err := parseFlags(nil, noEnv, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.host != "127.0.0.1" || c.port != 4002 || c.clientID != 17 || c.trading || c.marketDataType != 1 {
		t.Fatalf("defaults = %+v", c)
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
		if st := call("get_status", "{}"); st["connected"] == true && st["market_data_type"] == "delayed" {
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
