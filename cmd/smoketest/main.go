// Read-only smoke test against a live TWS / IB Gateway.
//
// It exercises connection handling, contract lookup, historical data, market
// data (snapshots, streaming, tick-by-tick, depth, bars), account data, PnL
// and the read-only order queries, and checks the decoded values for sanity.
// It never places, modifies or cancels orders, so it is safe to run against
// a live account. For extra safety run it through cmd/ibguard, which blocks
// order messages at the socket level (and enables the reconnect test):
//
//	go run ./cmd/ibguard -upstream 127.0.0.1:4001 -listen 127.0.0.1:4011 &
//	go run ./cmd/smoketest -port 4011 -guard-pid $(pgrep -x ibguard)
//
// -guard-pid must be the guard process itself (its pid is in the guard's
// startup log), not the "go run" wrapper ($!), which ignores SIGUSR1.
//
// Exit status 1 if any check failed. Account ids are masked in the output.
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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	ibgo "github.com/kevinzhao-dev/go-ib-async"
	"github.com/kevinzhao-dev/go-ib-async/account"
	"github.com/kevinzhao-dev/go-ib-async/contract"
	"github.com/kevinzhao-dev/go-ib-async/market"
	"github.com/kevinzhao-dev/go-ib-async/order"
)

var (
	host     = flag.String("host", envOr("IB_HOST", "127.0.0.1"), "TWS/Gateway host")
	port     = flag.Int("port", envIntOr("IB_PORT", 4002), "TWS/Gateway port (or the ibguard port)")
	clientID = flag.Int("client-id", envIntOr("IB_CLIENT_ID", 99), "API client id")
	guardPID = flag.Int("guard-pid", 0, "pid of cmd/ibguard: enables the reconnect test (SIGUSR1 drops the connection)")
	stream   = flag.Duration("stream", 10*time.Second, "how long to watch streaming data")
)

var pass, fail, warn int

// redactor masks every managed account id; it is set once the accounts
// are known and applied to all output (ours and the library's log).
var redactor atomic.Pointer[strings.Replacer]

func redact(s string) string {
	if r := redactor.Load(); r != nil {
		return r.Replace(s)
	}
	return s
}

// out prints redacted output.
func out(f string, a ...any) { fmt.Print(redact(fmt.Sprintf(f, a...))) }

type redactWriter struct{ w io.Writer }

func (r redactWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(r.w, redact(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func setAccounts(accts []string) {
	pairs := make([]string, 0, 2*len(accts))
	for _, a := range accts {
		if a != "" {
			pairs = append(pairs, a, mask(a))
		}
	}
	redactor.Store(strings.NewReplacer(pairs...))
}

func ok(name, f string, a ...any) {
	pass++
	out("  PASS %-36s %s\n", name, fmt.Sprintf(f, a...))
}
func bad(name, f string, a ...any) {
	fail++
	out("  FAIL %-36s %s\n", name, fmt.Sprintf(f, a...))
}
func wrn(name, f string, a ...any) {
	warn++
	out("  WARN %-36s %s\n", name, fmt.Sprintf(f, a...))
}
func check(name string, cond bool, f string, a ...any) {
	if cond {
		ok(name, f, a...)
	} else {
		bad(name, f, a...)
	}
}
func info(f string, a ...any) { out("       %s\n", fmt.Sprintf(f, a...)) }
func section(s string)        { out("\n== %s ==\n", s) }
func tctx(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
func valid(f float64) bool { return !math.IsNaN(f) && f != -1 && f != ibgo.UnsetDouble }

func mask(acct string) string {
	if len(acct) <= 4 {
		return acct
	}
	return acct[:2] + strings.Repeat("*", len(acct)-4) + acct[len(acct)-2:]
}

func main() {
	flag.Parse()
	log.SetOutput(redactWriter{os.Stderr})
	ib := ibgo.New()
	ib.ErrorEvent.Subscribe(func(e *ibgo.IBError) {
		out("       [ib %d] reqId=%d warn=%v: %.150s\n", e.Code, e.ReqID, e.IsWarning, e.Message)
	})

	connected := make(chan struct{}, 4)
	var connects atomic.Int32
	runCtx, stop := context.WithCancel(context.Background())
	cfg := ibgo.DefaultReconnectConfig()
	cfg.RetryDelay = time.Second
	cfg.ProbeInterval = 5 * time.Second
	done := make(chan error, 1)
	go func() {
		done <- ib.ConnectWithReconnect(runCtx, *host, *port, *clientID, cfg, func() {
			connects.Add(1)
			connected <- struct{}{}
		})
	}()

	section(fmt.Sprintf("connect %s:%d (client id %d)", *host, *port, *clientID))
	select {
	case <-connected:
	case err := <-done:
		bad("ConnectWithReconnect", "%v", err)
		os.Exit(1)
	case <-time.After(20 * time.Second):
		bad("ConnectWithReconnect", "timeout")
		os.Exit(1)
	}
	accts := ib.ManagedAccounts()
	setAccounts(accts)
	ok("ConnectWithReconnect", "serverVersion=%d accounts=%v", ib.ServerVersion(), accts)
	ctx, cancel := tctx(10 * time.Second)
	st, err := ib.ReqCurrentTime(ctx)
	cancel()
	check("ReqCurrentTime", err == nil && time.Since(st).Abs() < time.Minute, "%s err=%v", st.Format(time.RFC3339), err)

	accountChecks(ib)
	aapl := contractChecks(ib)
	if aapl != nil {
		historicalChecks(ib, aapl)
		snapshotChecks(ib, aapl)
	}
	orderQueries(ib)
	streamingChecks(ib, connected, &connects)
	errorChecks(ib)

	stop()
	select {
	case err := <-done:
		check("shutdown", errors.Is(err, context.Canceled), "%v", err)
	case <-time.After(10 * time.Second):
		bad("shutdown", "ConnectWithReconnect did not return")
	}
	fmt.Printf("\n== summary: %d pass, %d fail, %d warn ==\n", pass, fail, warn)
	if fail > 0 {
		os.Exit(1)
	}
}

func accountChecks(ib *ibgo.IB) {
	section("account")
	ctx, cancel := tctx(20 * time.Second)
	defer cancel()
	pos, err := ib.ReqPositions(ctx)
	check("ReqPositions", err == nil, "%d positions err=%v", len(pos), err)
	for _, p := range pos {
		c := p.Contract
		info("%s %s %s pos=%v avgCost=%.4f acct=%s", c.Symbol, c.SecType, c.LocalSymbol, p.Position, p.AvgCost, mask(p.Account))
		if c.ConID == 0 || c.Symbol == "" || p.Position == 0 {
			bad("position decode", "%+v", p)
		}
	}
	vals, err := ib.ReqAccountSummary(ctx, "All", "AccountType,NetLiquidation,TotalCashValue,BuyingPower,AvailableFunds,ExcessLiquidity")
	tags := map[string]bool{}
	for _, v := range vals {
		tags[v.Tag] = true
	}
	check("ReqAccountSummary", err == nil && tags["NetLiquidation"] && tags["BuyingPower"], "%d values err=%v", len(vals), err)
	for i := 0; i < 3; i++ { // TWS allows only 2 open summary subscriptions
		if _, err := ib.ReqAccountSummary(ctx, "All", "NetLiquidation"); err != nil {
			bad("ReqAccountSummary repeated", "%v", err)
			break
		}
	}

	var portEvents atomic.Int32
	ib.PortfolioEvent.Subscribe(func(*account.PortfolioItem) { portEvents.Add(1) })
	if err := ib.ReqAccountUpdates(ctx, ""); err != nil {
		bad("ReqAccountUpdates", "%v", err)
	} else {
		port := ib.Portfolio()
		ok("ReqAccountUpdates", "%d account values, %d portfolio items", len(ib.AccountValues()), len(port))
		for _, p := range port {
			info("portfolio %s pos=%v mktPx=%.2f uPnL=%.2f", p.Contract.LocalSymbol, p.Position, p.MarketPrice, p.UnrealizedPNL)
			if p.Contract.ConID == 0 || p.Position == 0 {
				bad("portfolio decode", "%+v", p)
			}
		}
	}
	if err := ib.ReqPnL("", ""); err != nil {
		bad("ReqPnL", "%v", err)
		return
	}
	var conID int64
	if port := ib.Portfolio(); len(port) > 0 {
		conID = port[0].Contract.ConID
		if err := ib.ReqPnLSingle("", "", conID); err != nil {
			bad("ReqPnLSingle", "%v", err)
		}
	}
	time.Sleep(3 * time.Second)
	p := ib.PnL()
	check("PnL", len(p) == 1 && !math.IsNaN(p[0].DailyPnL), "%+v", p)
	if conID != 0 {
		ps := ib.PnLSingles()
		check("PnLSingle", len(ps) == 1 && !math.IsNaN(ps[0].Position), "%+v", ps)
		ib.CancelPnLSingle("", "", conID)
	}
	ib.CancelPnL("", "")
}

func contractChecks(ib *ibgo.IB) *contract.Contract {
	section("contracts")
	ctx, cancel := tctx(30 * time.Second)
	defer cancel()
	for _, c := range []*contract.Contract{
		contract.Stock("SPY", "SMART", "USD"),
		contract.Forex("EURUSD"),
		contract.Index("SPX", "CBOE", "USD"),
		contract.ContFuture("ES", "CME"),
		contract.Crypto("BTC", "PAXOS", "USD"),
	} {
		cds, err := ib.ReqContractDetails(ctx, c)
		if err != nil || len(cds) == 0 {
			bad("ReqContractDetails "+c.Symbol, "n=%d err=%v", len(cds), err)
			continue
		}
		d := cds[0]
		check("ReqContractDetails "+c.Symbol, d.Contract.ConID != 0 && d.MinTick > 0 && d.TimeZoneID != "",
			"%s conId=%d minTick=%g tz=%s %q", d.Contract.SecType, d.Contract.ConID, d.MinTick, d.TimeZoneID, d.LongName)
	}
	all, err := ib.ReqContractDetails(ctx, &contract.Contract{Symbol: "ES", SecType: "FUT", Exchange: "CME", Currency: "USD"})
	check("ReqContractDetails ES futures", err == nil && len(all) > 1 && all[0].Contract.Multiplier == "50", "%d expiries err=%v", len(all), err)

	aapl := contract.Stock("AAPL", "SMART", "USD")
	missing := contract.Stock("ZZZZNOTREAL", "SMART", "USD")
	ambiguous := &contract.Contract{Symbol: "AAPL", SecType: "STK"}
	err = ib.QualifyContracts(ctx, aapl, missing, ambiguous)
	check("QualifyContracts", aapl.ConID == 265598 && aapl.Exchange == "SMART" && aapl.PrimaryExchange == "NASDAQ",
		"AAPL conId=%d exch=%s prim=%s", aapl.ConID, aapl.Exchange, aapl.PrimaryExchange)
	check("QualifyContracts errors", errors.Is(err, ibgo.ErrContractNotFound) && errors.Is(err, ibgo.ErrAmbiguousContract), "%v", err)

	syms, err := ib.ReqMatchingSymbols(ctx, "TSM")
	check("ReqMatchingSymbols", err == nil && len(syms) > 0 && syms[0].Contract.ConID != 0, "%d matches err=%v", len(syms), err)
	incs, err := ib.ReqMarketRule(ctx, 4563)
	check("ReqMarketRule", err == nil && len(incs) > 0, "%+v err=%v", incs, err)
	head, err := ib.ReqHeadTimeStamp(ctx, aapl, "TRADES", true)
	check("ReqHeadTimeStamp", err == nil && head.Year() < 1990, "%v err=%v", head, err)

	chains, err := ib.ReqSecDefOptParams(ctx, "AAPL", "", "STK", aapl.ConID)
	check("ReqSecDefOptParams", err == nil && len(chains) > 0, "%d chains err=%v", len(chains), err)
	if aapl.ConID == 0 {
		return nil
	}
	return aapl
}

func historicalChecks(ib *ibgo.IB, aapl *contract.Contract) {
	section("historical data")
	ctx, cancel := tctx(60 * time.Second)
	defer cancel()
	fx := contract.Forex("EURUSD")
	if err := ib.QualifyContracts(ctx, fx); err != nil {
		bad("qualify EURUSD", "%v", err)
		return
	}
	for _, tc := range []struct {
		name                string
		c                   *contract.Contract
		end, dur, bar, what string
		rth                 bool
		fmtDate             int
	}{
		{"AAPL daily", aapl, "", "1 M", "1 day", "TRADES", true, 1},
		{"AAPL hourly", aapl, "", "5 D", "1 hour", "TRADES", false, 1},
		{"AAPL 5 min epoch", aapl, "", "2 D", "5 mins", "TRADES", true, 2},
		{"AAPL weekly", aapl, "", "1 Y", "1 week", "TRADES", true, 1},
		{"EURUSD hourly", fx, "", "2 D", "1 hour", "MIDPOINT", false, 1},
	} {
		bars, err := ib.ReqHistoricalData(ctx, tc.c, tc.end, tc.dur, tc.bar, tc.what, tc.rth, tc.fmtDate)
		if err != nil || len(bars) == 0 {
			bad("ReqHistoricalData "+tc.name, "n=%d err=%v", len(bars), err)
			continue
		}
		sane := true
		for i, b := range bars {
			if b.Date.IsZero() || b.High < b.Low || (i > 0 && !b.Date.After(bars[i-1].Date)) {
				sane = false
			}
		}
		last := bars[len(bars)-1]
		check("ReqHistoricalData "+tc.name, sane, "%d bars, last %s C=%v", len(bars), last.Date.Format(time.RFC3339), last.Close)
	}
	_, err := ib.ReqHistoricalData(ctx, aapl, "", "1 D", "7 mins", "TRADES", true, 1)
	var ibe *ibgo.IBError
	check("invalid bar size fails fast", errors.As(err, &ibe) && ibe.Code == 321, "%v", err)

	end := time.Now().UTC().Format("20060102-15:04:05")
	ht, err := ib.ReqHistoricalTicks(ctx, fx, "", end, 50, "BID_ASK", false, false)
	check("ReqHistoricalTicks BID_ASK", err == nil && len(ht.BidAsk) > 0 && ht.BidAsk[0].PriceBid > 0, "err=%v", err)
	ht, err = ib.ReqHistoricalTicks(ctx, aapl, "", end, 50, "TRADES", false, false)
	if err == nil && len(ht.Last) > 0 {
		l := ht.Last[len(ht.Last)-1]
		ok("ReqHistoricalTicks TRADES", "%d ticks, last %v @ %s", len(ht.Last), l.Price, l.Time.Format(time.RFC3339))
	} else {
		wrn("ReqHistoricalTicks TRADES", "n=%v err=%v", ht, err)
	}
}

func snapshotChecks(ib *ibgo.IB, aapl *contract.Contract) {
	section("snapshots (delayed data if not subscribed)")
	if err := ib.ReqMarketDataType(ibgo.MarketDataDelayed); err != nil {
		bad("ReqMarketDataType", "%v", err)
	}
	defer ib.ReqMarketDataType(ibgo.MarketDataLive)
	ctx, cancel := tctx(40 * time.Second)
	defer cancel()
	spy := contract.Stock("SPY", "SMART", "USD")
	if err := ib.QualifyContracts(ctx, spy); err != nil {
		bad("qualify SPY", "%v", err)
		return
	}
	tickers, err := ib.ReqTickers(ctx, aapl, spy)
	if err != nil {
		wrn("ReqTickers", "%v", err)
	}
	for _, t := range tickers {
		d := t.Snapshot()
		check("ReqTickers "+d.Contract.Symbol, valid(d.Close) || valid(d.Last) || valid(d.Bid),
			"mdType=%d bid=%v ask=%v last=%v close=%v minTick=%v", d.MarketDataType, d.Bid, d.Ask, d.Last, d.Close, d.MinTick)
	}
}

func orderQueries(ib *ibgo.IB) {
	section("orders (read-only queries)")
	ctx, cancel := tctx(15 * time.Second)
	defer cancel()
	all, err := ib.ReqAllOpenOrders(ctx)
	check("ReqAllOpenOrders", err == nil, "%d open orders err=%v", len(all), err)
	for _, t := range all {
		o := t.Order
		info("open: %s %s %v %s lmt=%v status=%s", o.Action, t.Contract.LocalSymbol, o.TotalQuantity, o.OrderType, o.LmtPrice, t.Status())
		if t.Contract.ConID == 0 || o.PermID == 0 || o.Action == "" {
			bad("openOrder decode", "%+v", o)
		}
	}
	fills, err := ib.ReqExecutions(ctx, order.ExecutionFilter{})
	check("ReqExecutions", err == nil, "%d executions err=%v", len(fills), err)
	for _, qf := range []struct {
		name string
		f    func(context.Context) (int, error)
	}{
		{"ReqOpenOrders", func(ctx context.Context) (int, error) { t, err := ib.ReqOpenOrders(ctx); return len(t), err }},
		{"ReqCompletedOrders", func(ctx context.Context) (int, error) {
			t, err := ib.ReqCompletedOrders(ctx, false)
			return len(t), err
		}},
	} {
		ctx, cancel := tctx(10 * time.Second)
		n, err := qf.f(ctx)
		cancel()
		switch {
		case errors.Is(err, ibgo.ErrReadOnlyAPI):
			ok(qf.name, "rejected by read-only API mode (expected)")
		default:
			check(qf.name, err == nil, "n=%d err=%v", n, err)
		}
	}
	rep, err := ib.SyncOrders(ctx, ibgo.SyncOptions{IncludeCompleted: true})
	if rep == nil { // an early query failed
		bad("SyncOrders", "err=%v", err)
		return
	}
	check("SyncOrders", err == nil, "open=%d readOnly=%v err=%v", len(rep.OpenTrades), rep.ReadOnly, err)
}

func streamingChecks(ib *ibgo.IB, connected chan struct{}, connects *atomic.Int32) {
	section("streaming EURUSD")
	ctx, cancel := tctx(30 * time.Second)
	defer cancel()
	fx := contract.Forex("EURUSD")
	if err := ib.QualifyContracts(ctx, fx); err != nil {
		bad("qualify EURUSD", "%v", err)
		return
	}
	t, err := ib.ReqMktData(fx, "", false, false)
	if err != nil {
		bad("ReqMktData", "%v", err)
		return
	}
	_, err1 := ib.ReqTickByTickData(fx, ibgo.TickByTickBidAsk, 0, false)
	_, err2 := ib.ReqMktDepth(fx, 5, false)
	rt, err3 := ib.ReqRealTimeBars(fx, 5, "MIDPOINT", false)
	bars, err4 := ib.ReqHistoricalDataStream(ctx, fx, "1800 S", "1 min", "MIDPOINT", false, 1)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		bad("subscribe", "%v", err)
		return
	}
	var updates, tbt atomic.Int32
	t.UpdateEvent.Subscribe(func(t *market.Ticker) {
		updates.Add(1)
		tbt.Add(int32(len(t.TickByTicks)))
	})
	// Read concurrently from another goroutine, as an application would.
	var wg sync.WaitGroup
	quit := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-quit:
				return
			case <-time.After(time.Millisecond):
				_ = t.Snapshot().Midpoint()
				_, _ = rt.Snapshot(), bars.Snapshot()
			}
		}
	}()
	time.Sleep(*stream)
	d := t.Snapshot()
	check("ticker", updates.Load() > 0 && d.Bid > 0 && d.Ask >= d.Bid, "%d updates bid=%v ask=%v", updates.Load(), d.Bid, d.Ask)
	check("tick-by-tick", tbt.Load() > 0, "%d ticks", tbt.Load())
	if len(d.DomBids) > 0 && len(d.DomAsks) > 0 {
		ok("depth", "%d/%d levels, top %v / %v", len(d.DomBids), len(d.DomAsks), d.DomBids[0].Price, d.DomAsks[0].Price)
	} else {
		wrn("depth", "no book (market depth permissions?)")
	}
	check("real-time bars", len(rt.Snapshot()) > 0, "%d bars", len(rt.Snapshot()))
	bs := bars.Snapshot()
	last := "-"
	if len(bs) > 0 {
		last = bs[len(bs)-1].Date.Format(time.RFC3339)
	}
	check("keepUpToDate bars", len(bs) > 0, "%d bars, last %s", len(bs), last)

	if *guardPID != 0 {
		section("reconnect (connection dropped by ibguard)")
		_ = syscall.Kill(*guardPID, syscall.SIGUSR1)
		select {
		case <-connected:
			ok("reconnect", "onConnect #%d", connects.Load())
			u0, r0 := updates.Load(), len(rt.Snapshot())
			time.Sleep(*stream)
			check("subscriptions restored", updates.Load() > u0 && len(rt.Snapshot()) > r0, "+%d updates, +%d bars", updates.Load()-u0, len(rt.Snapshot())-r0)
		case <-time.After(30 * time.Second):
			bad("reconnect", "no reconnect within 30s (is -guard-pid the ibguard process from its startup log, not the go run wrapper?)")
		}
	}
	close(quit)
	wg.Wait()

	ib.CancelMktData(fx)
	ib.CancelTickByTickData(fx, ibgo.TickByTickBidAsk)
	ib.CancelMktDepth(fx, false)
	ib.CancelRealTimeBars(rt)
	ib.CancelHistoricalData(bars)
	time.Sleep(2 * time.Second)
	u := updates.Load()
	time.Sleep(3 * time.Second)
	check("cancel", updates.Load() == u, "no updates after cancel")
}

func errorChecks(ib *ibgo.IB) {
	section("error paths")
	ctx, cancel := tctx(15 * time.Second)
	defer cancel()
	_, err := ib.ReqContractDetails(ctx, contract.Stock("ZZZZNOTREAL", "SMART", "USD"))
	var ibe *ibgo.IBError
	check("unknown symbol", errors.As(err, &ibe) && ibe.Code == 200, "%v", err)
	short, c2 := tctx(time.Millisecond)
	_, err = ib.ReqHistoricalData(short, contract.Stock("AAPL", "SMART", "USD"), "", "1 Y", "1 day", "TRADES", true, 1)
	c2()
	check("context timeout", errors.Is(err, context.DeadlineExceeded), "%v", err)
	cds, err := ib.ReqContractDetails(ctx, contract.Stock("MSFT", "SMART", "USD"))
	check("healthy after timeout", err == nil && len(cds) == 1, "err=%v", err)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
