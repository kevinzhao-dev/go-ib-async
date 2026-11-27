// Command ib-mcp is a Model Context Protocol (MCP) server that lets AI agents
// use Interactive Brokers through TWS or IB Gateway. It speaks MCP over
// stdio; configure it as a stdio server in the MCP client.
//
// Read-only by default. Trading needs -enable-trading, an order journal and
// explicit risk limits, and is restricted to paper accounts unless
// -allow-live is given. See docs/MCP.md.
//
//	ib-mcp -port 4002
//	ib-mcp -port 4002 -enable-trading -journal ~/ib-mcp/orders.jsonl \
//	       -max-order-qty 100 -max-order-notional 10000
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	ibgo "github.com/kevinzhao-dev/go-ib-async"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/journal"
	"github.com/kevinzhao-dev/go-ib-async/mcpserver"
	"github.com/kevinzhao-dev/go-ib-async/risk"
)

var version = "dev"

type config struct {
	host           string
	port           int
	clientID       int
	marketDataType int
	requestTimeout time.Duration
	whatIf         bool

	trading           bool
	allowLive         bool
	allowMarketOrders bool
	journalPath       string
	previewTTL        time.Duration
	limits            risk.Limits
	ordersPerMinute   int
}

func parseFlags(args []string, getenv func(string) string, stderr io.Writer) (*config, error) {
	c := &config{}
	fs := flag.NewFlagSet("ib-mcp", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.host, "host", envStr(getenv, "IB_HOST", "127.0.0.1"), "TWS/Gateway host (env IB_HOST)")
	fs.IntVar(&c.port, "port", envInt(getenv, "IB_PORT", 4002), "TWS/Gateway port: 4002 Gateway paper, 4001 Gateway live, 7497 TWS paper, 7496 TWS live (env IB_PORT)")
	fs.IntVar(&c.clientID, "client-id", envInt(getenv, "IB_CLIENT_ID", 17), "API client id; orders placed by the agent belong to it (env IB_CLIENT_ID)")
	fs.IntVar(&c.marketDataType, "market-data-type", 1, "1 live, 2 frozen, 3 delayed, 4 delayed-frozen (3: delayed data where there is no live subscription)")
	fs.DurationVar(&c.requestTimeout, "request-timeout", 30*time.Second, "timeout of each tool call")
	fs.BoolVar(&c.whatIf, "what-if", true, "let preview_order send what-if orders for margin and commission estimates (TWS evaluates them, nothing executes); -what-if=false sends no order messages without -enable-trading")

	fs.BoolVar(&c.trading, "enable-trading", false, "register the order tools (place_order, cancel_order, cancel_all_orders, halt_trading)")
	fs.BoolVar(&c.allowLive, "allow-live", false, "allow orders for live (non-paper) accounts")
	fs.BoolVar(&c.allowMarketOrders, "allow-market-orders", false, "allow MKT orders (default: LMT, STP, STP LMT only)")
	fs.StringVar(&c.journalPath, "journal", "", "order journal file (JSONL, write-ahead); required with -enable-trading")
	fs.DurationVar(&c.previewTTL, "preview-ttl", 2*time.Minute, "how long a preview_order result can be placed")

	l := &c.limits
	fs.Float64Var(&l.MaxOrderQuantity, "max-order-qty", 0, "max quantity per order; required with -enable-trading")
	fs.Float64Var(&l.MaxOrderNotional, "max-order-notional", 0, "max quantity*price*multiplier per order; required with -enable-trading")
	fs.Float64Var(&l.MaxPositionQuantity, "max-position", 0, "max absolute position per contract, counting open orders (0 = off)")
	fs.IntVar(&l.MaxOpenOrders, "max-open-orders", 10, "max open orders (0 = off)")
	fs.IntVar(&c.ordersPerMinute, "max-orders-per-minute", 10, "max order submissions and modifications per minute; a bracket counts 3 (0 = off)")
	fs.Float64Var(&l.PriceCollarPct, "price-collar-pct", 5, "reject limit prices further than this % from the last quote (0 = off)")
	fs.DurationVar(&l.DuplicateWindow, "duplicate-window", 30*time.Second, "reject an identical new order within this window (0 = off)")
	fs.Float64Var(&l.MaxDailyLoss, "max-daily-loss", 0, "halt trading once today's realized loss reaches this amount (0 = off)")
	secTypes := fs.String("allowed-sec-types", "STK", "comma-separated security types that may be traded (empty = any)")
	symbols := fs.String("allowed-symbols", "", "comma-separated symbols that may be traded (empty = any)")
	blocked := fs.String("blocked-symbols", "", "comma-separated symbols that may never be traded")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	l.AllowedSecTypes = splitList(*secTypes)
	l.AllowedSymbols = splitList(*symbols)
	l.BlockedSymbols = splitList(*blocked)
	if c.ordersPerMinute > 0 {
		l.MaxOrdersPerWindow = c.ordersPerMinute
		l.OrderWindow = time.Minute
	}

	if c.marketDataType < 1 || c.marketDataType > 4 {
		return nil, fmt.Errorf("-market-data-type must be 1-4, got %d", c.marketDataType)
	}
	if c.clientID == 0 {
		// Syncing open orders as client 0 binds manual TWS orders to the
		// API (TWS may resubmit them), and they would count as this
		// server's own orders.
		return nil, errors.New("-client-id 0 is not supported: TWS binds manual TWS orders to client 0; use another id")
	}
	for _, f := range []struct {
		name string
		v    float64
	}{{"-max-order-qty", l.MaxOrderQuantity}, {"-max-order-notional", l.MaxOrderNotional}, {"-max-position", l.MaxPositionQuantity},
		{"-price-collar-pct", l.PriceCollarPct}, {"-max-daily-loss", l.MaxDailyLoss}} {
		if math.IsNaN(f.v) || math.IsInf(f.v, 0) || f.v < 0 {
			return nil, fmt.Errorf("%s must be a finite number >= 0, got %g", f.name, f.v)
		}
	}
	if c.trading {
		var missing []string
		if c.journalPath == "" {
			missing = append(missing, "-journal")
		}
		if l.MaxOrderQuantity <= 0 {
			missing = append(missing, "-max-order-qty")
		}
		if l.MaxOrderNotional <= 0 {
			missing = append(missing, "-max-order-notional")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("-enable-trading needs %s", strings.Join(missing, ", "))
		}
	} else if c.allowLive || c.allowMarketOrders || c.journalPath != "" {
		return nil, errors.New("-allow-live, -allow-market-orders and -journal only apply with -enable-trading")
	}
	return c, nil
}

func main() {
	log.SetOutput(os.Stderr) // stdout carries the MCP protocol
	log.SetPrefix("ib-mcp: ")
	cfg, err := parseFlags(os.Args[1:], os.Getenv, os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, os.Stdin, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}

// run serves MCP on in/out until in is closed or ctx ends.
func run(ctx context.Context, cfg *config, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	ib := ibgo.New()
	ib.ErrorEvent.Subscribe(func(e *ibgo.IBError) {
		log.Printf("IB %s", e.Error())
	})

	gate := &syncGate{}
	ib.DisconnectedEvent.Subscribe(func(struct{}) { gate.reset() })

	sc := mcpserver.Config{
		Version:        version,
		RequestTimeout: cfg.requestTimeout,
		MarketDataType: cfg.marketDataType,
		DisableWhatIf:  !cfg.whatIf,
	}
	var srv *mcpserver.Server
	if cfg.trading {
		j, err := journal.Open(cfg.journalPath, journal.Options{})
		if err != nil {
			return err
		}
		defer j.Close()
		ib.SetJournal(j)
		// The reference price for market-order notional and the price
		// collar comes from the quotes the server has seen.
		rm := risk.New(cfg.limits, ib, risk.WithPriceSource(func(c *contract.Contract) (float64, bool) {
			return srv.ReferencePrice(c)
		}))
		ib.SetRiskManager(rm)
		types := []string{"LMT", "STP", "STP LMT"}
		if cfg.allowMarketOrders {
			types = append(types, "MKT")
		}
		sc.Trading = mcpserver.TradingConfig{
			Enabled:    true,
			AllowLive:  cfg.allowLive,
			OrderTypes: types,
			PreviewTTL: cfg.previewTTL,
			Risk:       rm,
			Ready:      gate.check,
		}
		mode := "paper accounts only"
		if cfg.allowLive {
			mode = "LIVE ACCOUNTS ALLOWED"
		}
		log.Printf("trading enabled (%s), journal %s", mode, cfg.journalPath)
	}
	var err error
	srv, err = mcpserver.New(ib, sc)
	if err != nil {
		return err
	}

	connDone := make(chan struct{})
	var syncs sync.WaitGroup
	go func() {
		defer close(connDone)
		rc := ibgo.DefaultReconnectConfig()
		// Orders and positions are synchronised in onConnect instead, so
		// that place_order stays refused until a sync succeeded on the
		// current connection (the library would only log a failure).
		rc.SyncOrders = false
		err := ib.ConnectWithReconnect(ctx, cfg.host, cfg.port, cfg.clientID, rc, func() {
			log.Printf("connected to %s:%d (server version %d, accounts %v)", cfg.host, cfg.port, ib.ServerVersion(), ib.ManagedAccounts())
			gen := gate.start()
			if cfg.marketDataType != 1 {
				if err := ib.ReqMarketDataType(cfg.marketDataType); err != nil {
					log.Printf("market data type: %v", err)
				}
			}
			syncs.Add(1)
			go func() {
				defer syncs.Done()
				syncLoop(ctx, ib, gate, gen, syncRetryDelay)
			}()
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("connection loop ended: %v", err)
		}
	}()

	err = srv.Serve(ctx, in, out)
	// Stop the connection loop (it disconnects) before the journal closes.
	cancel()
	<-connDone
	ib.Disconnect()
	syncs.Wait()
	return err
}

const (
	syncTimeout    = 30 * time.Second
	syncRetryDelay = 5 * time.Second
)

// syncGate records whether the client's orders and positions have been
// synchronised with TWS on the current connection. place_order is refused
// until then, so the risk manager never checks an order against positions
// or open orders from before a (re)connect.
type syncGate struct {
	mu    sync.Mutex
	gen   int // connection generation; reset and start advance it
	ready bool
	err   error
}

// start begins a new connection and returns its generation.
func (g *syncGate) start() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gen++
	g.ready, g.err = false, nil
	return g.gen
}

// reset marks the state unsynchronised after a disconnect.
func (g *syncGate) reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.gen++
	g.ready, g.err = false, nil
}

// done records the outcome of a sync started in generation gen. It reports
// false when the connection has changed since, and the result is dropped.
func (g *syncGate) done(gen int, err error) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if gen != g.gen {
		return false
	}
	g.ready, g.err = err == nil, err
	return true
}

// check is mcpserver's TradingConfig.Ready.
func (g *syncGate) check() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case g.ready:
		return nil
	case g.err != nil:
		return fmt.Errorf("order and position sync failed (%v), retrying", g.err)
	}
	return errors.New("order and position sync with TWS is in progress")
}

// syncLoop synchronises orders, executions and positions for connection
// generation gen, retrying every retry until it succeeds, the connection
// changes or ctx ends.
func syncLoop(ctx context.Context, ib *ibgo.IB, g *syncGate, gen int, retry time.Duration) {
	for {
		sctx, cancel := context.WithTimeout(ctx, syncTimeout)
		_, err := ib.SyncOrders(sctx, ibgo.SyncOptions{})
		cancel()
		if !g.done(gen, err) || err == nil || ctx.Err() != nil {
			return
		}
		log.Printf("order sync failed: %v; retrying in %v", err, retry)
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, strings.ToUpper(p))
		}
	}
	return out
}

func envStr(getenv func(string) string, key, def string) string {
	if v := getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(getenv func(string) string, key string, def int) int {
	if v, err := strconv.Atoi(getenv(key)); err == nil {
		return v
	}
	return def
}
