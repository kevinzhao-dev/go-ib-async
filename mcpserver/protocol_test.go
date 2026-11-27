package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	ibgo "github.com/kevinzhao-dev/go-ib-async"
)

type rpcClient struct {
	t     *testing.T
	in    *io.PipeWriter
	lines chan string
	done  chan error
}

func startRPC(t *testing.T, s *Server) *rpcClient {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &rpcClient{t: t, in: inW, lines: make(chan string, 16), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		c.done <- s.Serve(ctx, inR, outW)
		outW.Close()
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			c.lines <- sc.Text()
		}
		close(c.lines)
	}()
	t.Cleanup(func() {
		inW.Close()
		cancel()
		<-c.done
	})
	return c
}

func (c *rpcClient) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

func (c *rpcClient) recv() map[string]any {
	c.t.Helper()
	select {
	case line, ok := <-c.lines:
		if !ok {
			c.t.Fatal("server closed its output")
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			c.t.Fatalf("bad response %q: %v", line, err)
		}
		if m["jsonrpc"] != "2.0" {
			c.t.Fatalf("response without jsonrpc 2.0: %q", line)
		}
		return m
	case <-time.After(5 * time.Second):
		c.t.Fatal("no response")
	}
	return nil
}

func (c *rpcClient) quiet(d time.Duration) {
	c.t.Helper()
	select {
	case line := <-c.lines:
		c.t.Fatalf("unexpected message %q", line)
	case <-time.After(d):
	}
}

func offlineServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(ibgo.New(), Config{Version: "1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProtocolHandshakeAndTools(t *testing.T) {
	c := startRPC(t, offlineServer(t))

	c.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	r := c.recv()
	if r["id"] != 1.0 || get(t, r, "result", "protocolVersion") != "2025-03-26" || get(t, r, "result", "serverInfo", "version") != "1.2.3" ||
		get(t, r, "result", "capabilities", "tools", "listChanged") != false || !strings.Contains(get(t, r, "result", "instructions").(string), "get_status") {
		t.Fatalf("initialize = %v", r)
	}
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	c.send(`{"jsonrpc":"2.0","id":"a","method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)
	if r := c.recv(); r["id"] != "a" || get(t, r, "result", "protocolVersion") != protocolVersions[0] {
		t.Fatalf("unknown version must get the latest: %v", r)
	}

	c.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if r := c.recv(); r["id"] != 2.0 || r["result"] == nil || r["error"] != nil {
		t.Fatalf("ping = %v", r)
	}

	c.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	r = c.recv()
	tools := get(t, r, "result", "tools").([]any)
	names := map[string]bool{}
	for _, tl := range tools {
		names[get(t, tl, "name").(string)] = true
		if get(t, tl, "inputSchema", "type") != "object" || get(t, tl, "annotations", "readOnlyHint") != true || get(t, tl, "description") == "" {
			t.Fatalf("tool = %v", tl)
		}
	}
	for _, n := range []string{"get_status", "search_contracts", "get_quote", "get_historical_bars", "get_option_chain", "get_positions",
		"get_account_summary", "list_orders", "get_order", "get_executions", "preview_order"} {
		if !names[n] {
			t.Fatalf("tool %s missing from %v", n, names)
		}
	}
	if len(names) != 11 {
		t.Fatalf("tools = %v", names)
	}

	c.send(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_status","arguments":{}}}`)
	r = c.recv()
	text := get(t, r, "result", "content", 0, "text").(string)
	if get(t, r, "result", "isError") != nil || get(t, r, "result", "content", 0, "type") != "text" ||
		get(t, r, "result", "structuredContent", "connected") != false || !strings.Contains(text, `"connected":false`) {
		t.Fatalf("call = %v", r)
	}

	c.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"get_quote","arguments":{"contract":{"symbol":"AAPL"}}}}`)
	if r := c.recv(); get(t, r, "result", "isError") != true || !strings.Contains(get(t, r, "result", "content", 0, "text").(string), "not connected") {
		t.Fatalf("tool error = %v", r)
	}
}

func TestProtocolErrors(t *testing.T) {
	c := startRPC(t, offlineServer(t))
	cases := []struct {
		req  string
		id   any
		code float64
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nope"}}`, 1.0, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":[1]}`, 2.0, codeInvalidParams},
		{`{"jsonrpc":"2.0","id":3,"method":"resources/list"}`, 3.0, codeMethodNotFound},
		{`{"jsonrpc":"2.0","id":4,"method":"initialize","params":"x"}`, 4.0, codeInvalidParams},
		{`{not json`, nil, codeParseError},
		{`[]`, nil, codeInvalidRequest},
		{`[{"jsonrpc":"2.0","id":5,"method":"ping"}`, nil, codeParseError},
	}
	for _, tc := range cases {
		c.send(tc.req)
		r := c.recv()
		if r["id"] != tc.id || get(t, r, "error", "code") != tc.code {
			t.Fatalf("%s: response = %v", tc.req, r)
		}
	}
	// Responses (no method) and unknown notifications are ignored.
	c.send(`{"jsonrpc":"2.0","id":9,"result":{}}`)
	c.send(`{"jsonrpc":"2.0","method":"notifications/whatever"}`)
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":12345}}`)
	c.send("   ")
	c.quiet(50 * time.Millisecond)
}

// TestProtocolBatch: MCP 2025-03-26 requires accepting JSON-RPC batches.
func TestProtocolBatch(t *testing.T) {
	c := startRPC(t, offlineServer(t))
	c.send(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},` +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"nope"}},7]`)
	var got []map[string]any
	select {
	case line := <-c.lines:
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("batch response %q: %v", line, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no batch response")
	}
	byID := map[any]map[string]any{}
	for _, r := range got {
		if r["jsonrpc"] != "2.0" {
			t.Fatalf("response without jsonrpc 2.0: %v", r)
		}
		byID[r["id"]] = r
	}
	if len(got) != 3 || byID[1.0]["result"] == nil || get(t, byID[2.0], "error", "code") != float64(codeInvalidParams) ||
		get(t, byID[nil], "error", "code") != float64(codeInvalidRequest) {
		t.Fatalf("batch responses = %v", got)
	}

	// Only notifications: no reply at all.
	c.send(`[{"jsonrpc":"2.0","method":"notifications/initialized"}]`)
	c.quiet(50 * time.Millisecond)
}

func TestProtocolCancellation(t *testing.T) {
	s := offlineServer(t)
	started := make(chan struct{})
	addTool(s, "block", "Block", "blocks until cancelled", readOnly, func(ctx context.Context, _ noArgs) (any, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	c := startRPC(t, s)
	c.send(`{"jsonrpc":"2.0","id":"slow","method":"tools/call","params":{"name":"block"}}`)
	<-started
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"slow","reason":"user"}}`)
	c.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if r := c.recv(); r["id"] != 2.0 {
		t.Fatalf("got %v, want only the ping response", r)
	}
	c.quiet(100 * time.Millisecond)
}

func TestProtocolToolFailures(t *testing.T) {
	s := offlineServer(t)
	s.cfg.RequestTimeout = 20 * time.Millisecond
	addTool(s, "panics", "Panics", "", readOnly, func(context.Context, noArgs) (any, error) { panic("boom") })
	addTool(s, "unencodable", "Unencodable", "", readOnly, func(context.Context, noArgs) (any, error) {
		return map[string]any{"ch": make(chan int)}, nil
	})
	addTool(s, "slow", "Slow", "", readOnly, func(ctx context.Context, _ noArgs) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	c := startRPC(t, s)
	for _, tc := range []struct{ tool, want string }{
		{"panics", "internal error: boom"},
		{"unencodable", "cannot encode result"},
		{"slow", "deadline exceeded"},
	} {
		c.send(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tc.tool + `"}}`)
		r := c.recv()
		if get(t, r, "result", "isError") != true || !strings.Contains(get(t, r, "result", "content", 0, "text").(string), tc.want) {
			t.Fatalf("%s: %v", tc.tool, r)
		}
	}
}

func TestServeStopsOnContextAndReadError(t *testing.T) {
	s := offlineServer(t)
	inR, inW := io.Pipe()
	defer inW.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, inR, io.Discard) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve = %v, want context.Canceled", err)
	}

	boom := errors.New("read failed")
	if err := s.Serve(context.Background(), io.MultiReader(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n"), errReader{boom}), io.Discard); !errors.Is(err, boom) {
		t.Fatalf("Serve = %v, want the read error", err)
	}
	// EOF ends Serve cleanly after answering what was read; a failing
	// writer is only logged.
	if err := s.Serve(context.Background(), strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), errWriter{}); err != nil {
		t.Fatalf("Serve at EOF = %v", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestServeReaderStopsAfterCancel(t *testing.T) {
	s := offlineServer(t)
	inR, inW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, inR, io.Discard) }()
	// The reader goroutine blocks handing a line to a cancelled Serve.
	cancel()
	go func() { _, _ = io.WriteString(inW, `{"jsonrpc":"2.0","id":1,"method":"ping"}`+"\n") }()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve = %v", err)
	}
	inW.Close()
}

func TestWriteEncodesUnencodableResults(t *testing.T) {
	var out strings.Builder
	ss := &session{w: &out}
	ss.write(rpcResponse{ID: json.RawMessage("7"), Result: make(chan int)})
	if !strings.Contains(out.String(), `"code":-32603`) || !strings.Contains(out.String(), `"id":7`) {
		t.Fatalf("out = %s", out.String())
	}
}

// --- schema ---

type schemaInner struct {
	Side string `json:"side" enum:"BUY,SELL" required:"true"`
}

type schemaArgs struct {
	Name     string       `json:"name" desc:"a name" required:"true"`
	Count    int          `json:"count,omitempty"`
	Ratio    float64      `json:"ratio,omitempty"`
	On       bool         `json:"on,omitempty"`
	Tags     []string     `json:"tags,omitempty" enum:"a,b"`
	Inner    schemaInner  `json:"inner,omitempty"`
	Ptr      *schemaInner `json:"ptr,omitempty"`
	Skipped  string       `json:"-"`
	internal string
}

func TestSchemaFor(t *testing.T) {
	s := schemaFor(reflect.TypeOf(&schemaArgs{}))
	b, _ := json.Marshal(s)
	got := string(b)
	for _, want := range []string{
		`"required":["name"]`, `"additionalProperties":false`, `"name":{"description":"a name","type":"string"}`,
		`"count":{"type":"integer"}`, `"ratio":{"type":"number"}`, `"on":{"type":"boolean"}`,
		`"tags":{"items":{"enum":["a","b"],"type":"string"},"type":"array"}`, `"side":{"enum":["BUY","SELL"],"type":"string"}`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("schema %s missing %s", got, want)
		}
	}
	if strings.Contains(got, "Skipped") || strings.Contains(got, "internal") {
		t.Fatalf("schema leaks fields: %s", got)
	}
	for _, bad := range []any{map[string]int{}, struct{ X int }{}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("schemaFor(%T) must panic", bad)
				}
			}()
			schemaFor(reflect.TypeOf(bad))
		}()
	}
}

func TestDecodeArgs(t *testing.T) {
	var a schemaArgs
	if err := decodeArgs(json.RawMessage(`{"name":"x","tags":["A"],"inner":{"side":"sell"},"ptr":{"side":"buy"}}`), &a); err != nil {
		t.Fatal(err)
	}
	if a.Tags[0] != "a" || a.Inner.Side != "SELL" || a.Ptr.Side != "BUY" {
		t.Fatalf("enums must be canonicalised: %+v", a)
	}
	for raw, want := range map[string]string{
		`{"name":"x","inner":{"side":"x"}}`:                `"inner.side" must be one of BUY, SELL`,
		`{"name":"x","inner":{"side":"BUY"},"ptr":{}}`:     `missing required argument "ptr.side"`,
		`{"name":"x","inner":{"side":"BUY"},"tags":["c"]}`: `"tags[0]" must be one of a, b`,
		`null`: `missing required argument "name"`,
	} {
		var a schemaArgs
		if err := decodeArgs(json.RawMessage(raw), &a); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: err = %v, want %q", raw, err, want)
		}
	}
}
